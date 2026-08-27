// Package observe provides a fail-open response observer for Plex metadata.
// It never rewrites the response body or delays the response headers; only a
// bounded copy is parsed after the caller has consumed the complete body.
package observe

import (
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"

	"github.com/InfinityPacer/plex-gateway/internal/plexmeta"
)

const defaultMaxBodyBytes int64 = 4 << 20

// Config controls which Plex responses are eligible for observation.
// MetadataPaths are path prefixes; a trailing * is accepted for readability
// and matches the remainder of that path. MaxBodyBytes bounds both the copied
// wire body and the decompressed gzip body.
type Config struct {
	MetadataPaths []string
	MaxBodyBytes  int64
	OnParts       func(Observation)
}

// Observation contains the request and response metadata associated with a
// successfully parsed response. The response body has reached EOF when the
// callback runs and must not be read again.
type Observation struct {
	Request  *http.Request
	Response *http.Response
	Parts    []plexmeta.Part
}

// RoundTripper wraps a Plex transport and observes only configured metadata
// responses. Any observation failure is deliberately invisible to the caller.
type RoundTripper struct {
	base          http.RoundTripper
	metadataPaths []string
	maxBodyBytes  int64
	onParts       func(Observation)
}

// NewRoundTripper creates a fail-open metadata observer around base. A nil
// base uses http.DefaultTransport, matching the behavior of http.Client.
func NewRoundTripper(base http.RoundTripper, config Config) *RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	maxBodyBytes := config.MaxBodyBytes
	if maxBodyBytes <= 0 {
		maxBodyBytes = defaultMaxBodyBytes
	}
	return &RoundTripper{
		base:          base,
		metadataPaths: append([]string(nil), config.MetadataPaths...),
		maxBodyBytes:  maxBodyBytes,
		onParts:       config.OnParts,
	}
}

// RoundTrip forwards the request and wraps an eligible response body. The
// response headers and wire bytes are returned exactly as received from base.
func (t *RoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := t.base.RoundTrip(request)
	if err != nil || response == nil || response.Body == nil || t.onParts == nil {
		return response, err
	}
	if !matchesMetadataPath(request.URL.Path, t.metadataPaths) || !successfulMetadataResponse(response) {
		return response, nil
	}
	response.Body = &observedBody{
		source:          response.Body,
		request:         request,
		response:        response,
		contentType:     response.Header.Get("Content-Type"),
		contentEncoding: response.Header.Get("Content-Encoding"),
		maxBodyBytes:    t.maxBodyBytes,
		onParts:         t.onParts,
	}
	return response, nil
}

type observedBody struct {
	source          io.ReadCloser
	request         *http.Request
	response        *http.Response
	contentType     string
	contentEncoding string
	maxBodyBytes    int64
	onParts         func(Observation)
	captured        bytes.Buffer
	bytesSeen       int64
	overflow        bool
	failed          bool
	finished        bool
}

func (body *observedBody) Read(destination []byte) (int, error) {
	count, err := body.source.Read(destination)
	if count > 0 {
		body.capture(destination[:count])
	}
	if err != nil {
		if errors.Is(err, io.EOF) {
			body.finish()
		} else {
			body.failed = true
		}
	}
	return count, err
}

func (body *observedBody) Close() error {
	if !body.finished {
		// Closing before EOF is intentionally non-observable: callers may stop
		// reading a normal Plex response at any point.
		body.failed = true
	}
	return body.source.Close()
}

func (body *observedBody) capture(chunk []byte) {
	body.bytesSeen += int64(len(chunk))
	if body.overflow || body.bytesSeen > body.maxBodyBytes {
		body.overflow = true
		return
	}
	_, _ = body.captured.Write(chunk)
}

func (body *observedBody) finish() {
	if body.finished {
		return
	}
	body.finished = true
	if body.failed || body.overflow {
		return
	}
	decoded, err := decodeBody(body.captured.Bytes(), body.contentEncoding, body.maxBodyBytes)
	if err != nil {
		return
	}
	parts, err := plexmeta.ParseParts(decoded, body.contentType)
	if err != nil {
		return
	}
	// A callback is extension code on the playback path. Do not let a bug in
	// it turn an otherwise valid Plex response into a failed client read.
	defer func() { _ = recover() }()
	body.onParts(Observation{Request: body.request, Response: body.response, Parts: parts})
}

func decodeBody(body []byte, contentEncoding string, maxBodyBytes int64) ([]byte, error) {
	encodings := strings.Split(strings.ToLower(strings.TrimSpace(contentEncoding)), ",")
	if (len(encodings) == 1 && strings.TrimSpace(encodings[0]) == "") || strings.EqualFold(strings.TrimSpace(contentEncoding), "identity") {
		return body, nil
	}
	if len(encodings) != 1 || strings.TrimSpace(encodings[0]) != "gzip" {
		return nil, errors.New("unsupported content encoding")
	}
	reader, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	decoded, err := io.ReadAll(io.LimitReader(reader, maxBodyBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(decoded)) > maxBodyBytes {
		return nil, errors.New("decompressed body exceeds limit")
	}
	return decoded, nil
}

func matchesMetadataPath(path string, configured []string) bool {
	for _, configuredPath := range configured {
		pattern := strings.TrimSpace(configuredPath)
		if pattern == "" {
			continue
		}
		if strings.HasSuffix(pattern, "*") {
			if strings.HasPrefix(path, strings.TrimSuffix(pattern, "*")) {
				return true
			}
			continue
		}
		pattern = strings.TrimRight(pattern, "/")
		if path == pattern || strings.HasPrefix(path, pattern+"/") {
			return true
		}
	}
	return false
}

func successfulMetadataResponse(response *http.Response) bool {
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return false
	}
	mediaType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil {
		mediaType = strings.TrimSpace(strings.Split(response.Header.Get("Content-Type"), ";")[0])
	}
	mediaType = strings.ToLower(mediaType)
	return mediaType == "application/xml" || mediaType == "text/xml" ||
		mediaType == "application/json" || strings.HasSuffix(mediaType, "+xml") || strings.HasSuffix(mediaType, "+json")
}
