package gateway

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/InfinityPacer/plex-gateway/internal/playback"
	"github.com/InfinityPacer/plex-gateway/internal/plexmeta"
)

const (
	defaultDecisionMetadataMaxBytes = 4 << 20
	defaultDecisionGrantTTL         = 5 * time.Minute
	defaultDecisionGrantLimit       = 4096
)

// decisionHandler opts an eligible STRM Part into Plex Direct Play while
// leaving Plex responsible for the decision response and session semantics.
// Any uncertainty preserves the original request unchanged.
type decisionHandler struct {
	plex    http.Handler
	probe   *decisionMetadataProbe
	service *playback.Service
	grants  *playback.GrantStore
	logger  *slog.Logger
}

func (h *decisionHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	attempt, query, validAttempt := normalizePlaybackAttempt(r)
	if !hasPlexToken(r) || !validAttempt {
		h.plex.ServeHTTP(w, r)
		return
	}
	selection, state := h.selectCloudPart(r, attempt, query)
	if state != decisionSelectionUnauthenticated {
		h.grants.Delete(attempt)
	}
	if state != decisionSelectionCloud {
		h.plex.ServeHTTP(w, r)
		return
	}

	h.service.Remember(selection)
	request := r.Clone(r.Context())
	requestURL := *r.URL
	requestURL.RawQuery = forceDirectPlay(requestURL.RawQuery)
	if _, present := query["mediaIndex"]; !present {
		requestURL.RawQuery += "&mediaIndex=0"
	}
	request.URL = &requestURL
	h.logger.Info("decision_direct_play", "part", selection.Part.ID)
	capture := newDecisionResponseCapture(w, defaultDecisionMetadataMaxBytes)
	h.plex.ServeHTTP(capture, request)
	granted := false
	if capture.successful() && plexmeta.IsDirectPlayDecision(capture.body(), capture.Header().Get("Content-Type"), selection.Part) {
		granted = h.grants.Put(attempt, selection)
	}
	if err := capture.commit(); err != nil && granted {
		h.grants.Delete(attempt)
	}
}

func (h *decisionHandler) selectCloudPart(r *http.Request, attempt playback.Attempt, query url.Values) (playback.PreparedPart, decisionSelectionState) {
	if h.probe == nil || h.service == nil {
		return playback.PreparedPart{}, decisionSelectionUnauthenticated
	}
	_, mediaIndexPresent := query["mediaIndex"]
	if !mediaIndexPresent && attempt.PartIndex != 0 {
		return playback.PreparedPart{}, decisionSelectionPassthrough
	}
	metadata, err := h.probe.read(r, attempt.MetadataPath, query)
	if err != nil {
		return playback.PreparedPart{}, decisionSelectionUnauthenticated
	}
	var part plexmeta.Part
	if mediaIndexPresent {
		part, err = plexmeta.SelectPart(metadata.body, metadata.contentType, attempt.MediaIndex, attempt.PartIndex)
	} else {
		part, err = plexmeta.SelectUniquePart(metadata.body, metadata.contentType)
	}
	if err != nil || part.ID == "" || part.File == "" {
		return playback.PreparedPart{}, decisionSelectionPassthrough
	}
	preparation := h.service.Prepare(part)
	if preparation.State != playback.PreparationReady {
		return playback.PreparedPart{}, decisionSelectionPassthrough
	}
	return preparation.Part, decisionSelectionCloud
}

type decisionMetadataProbe struct {
	upstream *url.URL
	client   *http.Client
	maxBytes int64
}

type decisionMetadata struct {
	body        []byte
	contentType string
}

type decisionSelectionState uint8

const (
	decisionSelectionUnauthenticated decisionSelectionState = iota
	decisionSelectionPassthrough
	decisionSelectionCloud
)

// read fetches only the selected item's metadata with the active client's Plex
// credentials. It never follows redirects or retains a response beyond this
// decision request.
func (p *decisionMetadataProbe) read(original *http.Request, metadataPath string, originalQuery url.Values) (decisionMetadata, error) {
	if p == nil || p.upstream == nil || p.client == nil || p.maxBytes <= 0 {
		return decisionMetadata{}, errors.New("decision metadata probe is unavailable")
	}
	target := *p.upstream
	target.Path = joinURLPath(target.Path, metadataPath)
	target.RawPath = ""
	target.RawQuery = ""
	target.Fragment = ""
	query := target.Query()
	for name, values := range originalQuery {
		for _, value := range values {
			query.Add(name, value)
		}
	}
	target.RawQuery = query.Encode()

	request, err := http.NewRequestWithContext(original.Context(), http.MethodGet, target.String(), nil)
	if err != nil {
		return decisionMetadata{}, err
	}
	request.Header = original.Header.Clone()
	removeHopByHopHeaders(request.Header)
	removeForwardingHeaders(request.Header)
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Accept-Encoding", "identity")
	request.Header.Del("Range")
	request.Header.Del("If-Range")
	request.Header.Del("If-Modified-Since")
	request.Header.Del("If-None-Match")
	request.Host = p.upstream.Host

	response, err := p.client.Do(request)
	if err != nil {
		return decisionMetadata{}, err
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return decisionMetadata{}, errors.New("Plex metadata probe was not authorized")
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, p.maxBytes+1))
	if err != nil {
		return decisionMetadata{}, err
	}
	if int64(len(body)) > p.maxBytes {
		return decisionMetadata{}, errors.New("Plex metadata response exceeds the limit")
	}
	return decisionMetadata{body: body, contentType: response.Header.Get("Content-Type")}, nil
}

func normalizePlaybackAttempt(request *http.Request) (playback.Attempt, url.Values, bool) {
	if request == nil || request.URL == nil {
		return playback.Attempt{}, nil, false
	}
	query, err := url.ParseQuery(request.URL.RawQuery)
	if err != nil {
		return playback.Attempt{}, nil, false
	}
	metadataPath, ok := singleQueryValue(query, "path")
	if !ok || !isMetadataItemPath(metadataPath) {
		return playback.Attempt{}, query, false
	}
	mediaIndex, _, ok := optionalNonNegativeIndex(query, "mediaIndex")
	if !ok {
		return playback.Attempt{}, query, false
	}
	partIndex, ok := nonNegativeIndex(query, "partIndex")
	if !ok {
		return playback.Attempt{}, query, false
	}

	var session playback.SessionIdentity
	for _, name := range []string{
		"X-Plex-Playback-Session-Id",
		"X-Plex-Playback-Id",
		"X-Plex-Session-Id",
		"X-Plex-Session-Identifier",
		"session",
	} {
		value, present, valid := requestIdentity(request, name)
		if !valid {
			return playback.Attempt{}, query, false
		}
		if present {
			session = playback.SessionIdentity{Name: name, Value: value}
			break
		}
	}
	attempt := playback.Attempt{
		MetadataPath: metadataPath,
		MediaIndex:   mediaIndex,
		PartIndex:    partIndex,
		Session:      session,
	}
	return attempt, query, attempt.Valid()
}

func optionalNonNegativeIndex(query url.Values, name string) (index int, present, valid bool) {
	values, present := query[name]
	if !present {
		return 0, false, true
	}
	if len(values) != 1 || values[0] == "" {
		return 0, true, false
	}
	index, err := strconv.Atoi(values[0])
	return index, true, err == nil && index >= 0
}

func requestIdentity(request *http.Request, name string) (value string, present, valid bool) {
	queryValues, queryPresent := request.URL.Query()[name]
	headerValues := request.Header.Values(name)
	if queryPresent && (len(queryValues) != 1 || strings.TrimSpace(queryValues[0]) == "") {
		return "", false, false
	}
	if len(headerValues) > 1 || len(headerValues) == 1 && strings.TrimSpace(headerValues[0]) == "" {
		return "", false, false
	}
	if queryPresent && len(headerValues) == 1 && queryValues[0] != headerValues[0] {
		return "", false, false
	}
	if queryPresent {
		return queryValues[0], true, true
	}
	if len(headerValues) == 1 {
		return headerValues[0], true, true
	}
	return "", false, true
}

func singleQueryValue(query url.Values, name string) (string, bool) {
	values := query[name]
	returnValue := ""
	if len(values) == 1 {
		returnValue = values[0]
	}
	return returnValue, len(values) == 1 && returnValue != ""
}

func nonNegativeIndex(query url.Values, name string) (int, bool) {
	value, ok := singleQueryValue(query, name)
	if !ok {
		return 0, false
	}
	index, err := strconv.Atoi(value)
	return index, err == nil && index >= 0
}

func isMetadataItemPath(value string) bool {
	const prefix = "/library/metadata/"
	if !strings.HasPrefix(value, prefix) || path.Clean(value) != value {
		return false
	}
	identifier := strings.TrimPrefix(value, prefix)
	if identifier == "" || strings.Contains(identifier, "/") {
		return false
	}
	for _, character := range identifier {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}

// forceDirectPlay preserves every unrelated raw query component, including
// repeated client-profile parameters, while replacing all conflicting Direct
// Play flags with one authoritative value each.
func forceDirectPlay(rawQuery string) string {
	components := strings.Split(rawQuery, "&")
	result := make([]string, 0, len(components)+2)
	for _, component := range components {
		if component == "" {
			continue
		}
		rawName, _, _ := strings.Cut(component, "=")
		name, err := url.QueryUnescape(rawName)
		if err == nil && (name == "directPlay" || name == "directStream") {
			continue
		}
		result = append(result, component)
	}
	return strings.Join(append(result, "directPlay=1", "directStream=1"), "&")
}
