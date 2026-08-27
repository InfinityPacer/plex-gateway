package probe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/InfinityPacer/plex-gateway/internal/plexmeta"
)

const maxMetadataBody = 32 << 20

// Part is kept as an alias so existing probe reports retain their public shape
// while the metadata parser is shared with the runtime observer.
type Part = plexmeta.Part

// PartChange describes a key or file-path change for one stable Part ID.
type PartChange struct {
	ID         string `json:"id"`
	KeyBefore  string `json:"key_before,omitempty"`
	KeyAfter   string `json:"key_after,omitempty"`
	FileBefore string `json:"file_before,omitempty"`
	FileAfter  string `json:"file_after,omitempty"`
}

// Comparison summarizes differences between two metadata observations.
type Comparison struct {
	BaselineGeneratedAt time.Time    `json:"baseline_generated_at"`
	Added               []Part       `json:"added,omitempty"`
	Removed             []Part       `json:"removed,omitempty"`
	Changed             []PartChange `json:"changed,omitempty"`
}

// Report is intentionally token-free so it can be retained as local research
// evidence. Part paths can still reveal private storage layout and must be
// sanitized before any report is shared.
type Report struct {
	GeneratedAt      time.Time         `json:"generated_at"`
	RatingKey        string            `json:"rating_key"`
	MetadataEndpoint string            `json:"metadata_endpoint"`
	HTTPStatus       int               `json:"http_status"`
	ContentType      string            `json:"content_type,omitempty"`
	ResponseHeaders  map[string]string `json:"response_headers,omitempty"`
	Parts            []Part            `json:"parts"`
	Comparison       *Comparison       `json:"comparison,omitempty"`
}

// Client performs bounded read-only Plex metadata observations.
type Client struct {
	baseURL *url.URL
	token   string
	http    *http.Client
}

// NewClient validates the Plex origin and creates a timeout-bounded client.
func NewClient(rawBaseURL, token string, timeout time.Duration) (*Client, error) {
	baseURL, err := url.Parse(strings.TrimSpace(rawBaseURL))
	if err != nil {
		return nil, fmt.Errorf("parse Plex URL: %w", err)
	}
	if baseURL.Scheme != "http" && baseURL.Scheme != "https" {
		return nil, errors.New("Plex URL must use http or https")
	}
	if baseURL.Host == "" || baseURL.User != nil {
		return nil, errors.New("Plex URL must include a host and no credentials")
	}
	if timeout <= 0 {
		return nil, errors.New("timeout must be positive")
	}
	baseURL.RawQuery = ""
	baseURL.Fragment = ""

	return &Client{
		baseURL: baseURL,
		token:   strings.TrimSpace(token),
		http: &http.Client{
			Timeout: timeout,
			Transport: &http.Transport{
				MaxIdleConns:          10,
				MaxIdleConnsPerHost:   4,
				IdleConnTimeout:       30 * time.Second,
				TLSHandshakeTimeout:   10 * time.Second,
				ResponseHeaderTimeout: timeout,
			},
		},
	}, nil
}

// FetchMetadata reads one Plex metadata item and extracts all Media Parts.
func (c *Client) FetchMetadata(ctx context.Context, ratingKey string) (Report, error) {
	ratingKey = strings.TrimSpace(ratingKey)
	if ratingKey == "" || strings.ContainsAny(ratingKey, "/?#") {
		return Report{}, errors.New("rating key must be a single path segment")
	}

	endpoint := *c.baseURL
	endpoint.Path = strings.TrimRight(c.baseURL.Path, "/") + "/library/metadata/" + url.PathEscape(ratingKey)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return Report{}, fmt.Errorf("create metadata request: %w", err)
	}
	request.Header.Set("Accept", "application/xml, application/json;q=0.9")
	request.Header.Set("X-Plex-Product", "plex-gateway-probe")
	request.Header.Set("X-Plex-Client-Identifier", "plex-gateway-probe")
	if c.token != "" {
		request.Header.Set("X-Plex-Token", c.token)
	}

	response, err := c.http.Do(request)
	if err != nil {
		return Report{}, fmt.Errorf("request Plex metadata: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxMetadataBody+1))
	if err != nil {
		return Report{}, fmt.Errorf("read Plex metadata: %w", err)
	}
	if len(body) > maxMetadataBody {
		return Report{}, fmt.Errorf("Plex metadata exceeds %d bytes", maxMetadataBody)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return Report{}, fmt.Errorf("Plex metadata returned HTTP %d", response.StatusCode)
	}

	parts, err := ParseParts(body, response.Header.Get("Content-Type"))
	if err != nil {
		return Report{}, err
	}
	sort.Slice(parts, func(i, j int) bool { return parts[i].ID < parts[j].ID })

	return Report{
		GeneratedAt:      time.Now().UTC(),
		RatingKey:        ratingKey,
		MetadataEndpoint: "/library/metadata/" + ratingKey,
		HTTPStatus:       response.StatusCode,
		ContentType:      response.Header.Get("Content-Type"),
		ResponseHeaders:  selectedHeaders(response.Header),
		Parts:            parts,
	}, nil
}

// ParseParts preserves the probe package API while delegating to the shared
// Plex metadata parser used by runtime observation.
func ParseParts(body []byte, contentType string) ([]Part, error) {
	return plexmeta.ParseParts(body, contentType)
}

// Compare treats Part ID as the identity key and reports key/path drift.
func Compare(baseline, current Report) Comparison {
	result := Comparison{BaselineGeneratedAt: baseline.GeneratedAt}
	before := make(map[string]Part, len(baseline.Parts))
	after := make(map[string]Part, len(current.Parts))
	for _, part := range baseline.Parts {
		before[part.ID] = part
	}
	for _, part := range current.Parts {
		after[part.ID] = part
		old, found := before[part.ID]
		if !found {
			result.Added = append(result.Added, part)
			continue
		}
		if old.Key != part.Key || old.File != part.File {
			result.Changed = append(result.Changed, PartChange{
				ID: part.ID, KeyBefore: old.Key, KeyAfter: part.Key,
				FileBefore: old.File, FileAfter: part.File,
			})
		}
	}
	for _, part := range baseline.Parts {
		if _, found := after[part.ID]; !found {
			result.Removed = append(result.Removed, part)
		}
	}
	sort.Slice(result.Added, func(i, j int) bool { return result.Added[i].ID < result.Added[j].ID })
	sort.Slice(result.Removed, func(i, j int) bool { return result.Removed[i].ID < result.Removed[j].ID })
	sort.Slice(result.Changed, func(i, j int) bool { return result.Changed[i].ID < result.Changed[j].ID })
	return result
}

// LoadReport reads a prior JSON observation for stability comparison.
func LoadReport(path string) (Report, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Report{}, fmt.Errorf("read baseline report: %w", err)
	}
	var report Report
	if err := json.Unmarshal(data, &report); err != nil {
		return Report{}, fmt.Errorf("parse baseline report: %w", err)
	}
	return report, nil
}

func selectedHeaders(header http.Header) map[string]string {
	result := make(map[string]string)
	for _, name := range []string{"Server", "X-Plex-Protocol", "X-Plex-Protocol-Version"} {
		if value := strings.TrimSpace(header.Get(name)); value != "" {
			result[name] = value
		}
	}
	if len(result) == 0 {
		return nil
	}
	return result
}
