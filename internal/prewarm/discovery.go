// Package prewarm discovers and schedules nearby speculative MediaInfo records
// after a cloud redirect is ready.
package prewarm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/InfinityPacer/plex-gateway/internal/plexmeta"
)

const defaultPlexResponseLimit int64 = 8 << 20

var (
	ErrNoCandidates         = errors.New("no nearby candidates")
	ErrUntrustedCurrent     = errors.New("current media identity is ambiguous")
	errPlayQueueUnavailable = errors.New("play queue is unavailable")
)

// PlaybackContext carries the exact authorized Part and STRM target captured
// when a cloud redirect is ready. Target is transient analysis input: it is
// never logged, persisted, or combined with the Plex management credential.
type PlaybackContext struct {
	RatingKey string
	PartID    string
	Target    string
	// WindowKey identifies one client playback window. It should remain stable
	// while the client moves between items and must differ across clients.
	WindowKey       string
	PlayQueueID     string
	PlayQueueItemID string
	UserAgent       string
}

// Candidate is one nearby speculative Plex item and its first usable Part.
type Candidate struct {
	RatingKey string
	Part      plexmeta.Part
}

// Discovery reads only the configured Plex origin with a management token.
// The token is held by the client and never copied into playback events.
type Discovery struct {
	baseURL   *url.URL
	token     string
	client    *http.Client
	maxBytes  int64
	userAgent string
}

// DiscoveryOptions defines the isolated Plex management client.
type DiscoveryOptions struct {
	BaseURL   *url.URL
	Token     string
	Client    *http.Client
	MaxBytes  int64
	UserAgent string
}

// NewDiscovery rejects credentials that cannot safely be represented in an
// HTTP header and pins every request to one configured Plex origin.
func NewDiscovery(options DiscoveryOptions) (*Discovery, error) {
	if options.BaseURL == nil || (options.BaseURL.Scheme != "http" && options.BaseURL.Scheme != "https") ||
		options.BaseURL.Host == "" || options.BaseURL.User != nil {
		return nil, errors.New("Plex management origin is invalid")
	}
	token := strings.TrimSpace(options.Token)
	if token == "" || strings.ContainsAny(token, "\r\n") {
		return nil, errors.New("Plex management token is invalid")
	}
	defaultTransport := http.DefaultTransport.(*http.Transport).Clone()
	defaultTransport.Proxy = nil
	transport := http.RoundTripper(defaultTransport)
	timeout := 15 * time.Second
	if options.Client != nil {
		if options.Client.Transport != nil {
			transport = options.Client.Transport
		}
		if options.Client.Timeout > 0 {
			timeout = options.Client.Timeout
		}
	}
	client := &http.Client{
		Transport: transport,
		Timeout:   timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	maxBytes := options.MaxBytes
	if maxBytes <= 0 {
		maxBytes = defaultPlexResponseLimit
	}
	userAgent := strings.TrimSpace(options.UserAgent)
	if strings.ContainsAny(userAgent, "\r\n") {
		return nil, errors.New("Plex management User-Agent is invalid")
	}
	baseURL := *options.BaseURL
	baseURL.RawQuery = ""
	baseURL.Fragment = ""
	baseURL.User = nil
	return &Discovery{baseURL: &baseURL, token: token, client: client, maxBytes: maxBytes, userAgent: userAgent}, nil
}

// Validate confirms that the management token can read an authenticated Plex
// library endpoint. Failure disables prewarming without affecting proxying.
func (discovery *Discovery) Validate(ctx context.Context) error {
	_, _, err := discovery.get(ctx, "/library/sections", url.Values{
		"X-Plex-Container-Start": {"0"},
		"X-Plex-Container-Size":  {"1"},
	})
	return err
}

// Neighbors verifies that the current metadata contains the active Part, then
// prefers the explicit play-queue order and falls back to the episode hierarchy.
// Returned candidates are ordered with following items first because continuous
// playback is more likely than backward navigation.
func (discovery *Discovery) Neighbors(ctx context.Context, current PlaybackContext, before, after int) ([]Candidate, error) {
	if !numericIdentity(current.RatingKey) || !numericIdentity(current.PartID) {
		return nil, ErrUntrustedCurrent
	}
	if before < 0 || after < 0 || before+after == 0 {
		return nil, ErrNoCandidates
	}
	body, contentType, err := discovery.get(ctx, "/library/metadata/"+current.RatingKey, nil)
	if err != nil || !partsContainID(body, contentType, current.PartID) {
		return nil, ErrUntrustedCurrent
	}
	currentEpisode, hasEpisode := exactEpisode(body, contentType, current.RatingKey)
	if numericIdentity(current.PlayQueueID) && numericIdentity(current.PlayQueueItemID) {
		candidates, queueErr := discovery.neighborsFromPlayQueue(ctx, current, before, after)
		if queueErr == nil && len(candidates) > 0 {
			return candidates, nil
		}
	}
	if !hasEpisode || currentEpisode.ParentRatingKey == "" || currentEpisode.GrandparentRatingKey == "" {
		return nil, ErrNoCandidates
	}
	return discovery.neighborsFromLibrary(ctx, currentEpisode, before, after)
}

func (discovery *Discovery) neighborsFromPlayQueue(ctx context.Context, playback PlaybackContext, before, after int) ([]Candidate, error) {
	body, contentType, err := discovery.get(ctx, "/playQueues/"+playback.PlayQueueID, nil)
	if err != nil {
		return nil, err
	}
	items, err := plexmeta.ParsePlayQueue(body, contentType)
	if err != nil {
		return nil, err
	}
	position := -1
	for index, item := range items {
		if item.PlayQueueItemID != playback.PlayQueueItemID {
			continue
		}
		if position >= 0 || item.RatingKey != playback.RatingKey {
			return nil, errPlayQueueUnavailable
		}
		position = index
	}
	if position < 0 {
		return nil, errPlayQueueUnavailable
	}
	ratingKeys := make([]string, 0, before+after)
	for index := position + 1; index < len(items) && len(ratingKeys) < after; index++ {
		if numericIdentity(items[index].RatingKey) {
			ratingKeys = append(ratingKeys, items[index].RatingKey)
		}
	}
	previous := 0
	for index := position - 1; index >= 0 && previous < before; index-- {
		if numericIdentity(items[index].RatingKey) {
			ratingKeys = append(ratingKeys, items[index].RatingKey)
			previous++
		}
	}
	return discovery.confirmCandidates(ctx, ratingKeys)
}

func (discovery *Discovery) neighborsFromLibrary(ctx context.Context, current plexmeta.Episode, before, after int) ([]Candidate, error) {
	body, contentType, err := discovery.get(ctx, "/library/metadata/"+current.ParentRatingKey+"/children", nil)
	if err != nil {
		return nil, err
	}
	episodes, err := plexmeta.ParseEpisodes(body, contentType)
	if err != nil {
		return nil, err
	}
	previousRefs, nextRefs, positioned := splitEpisodeNeighbors(episodes, current)
	if !positioned {
		return nil, ErrNoCandidates
	}
	previousRefs = takeFirst(previousRefs, before)
	nextRefs = takeFirst(nextRefs, after)
	if len(previousRefs) < before || len(nextRefs) < after {
		body, contentType, err = discovery.get(ctx, "/library/metadata/"+current.GrandparentRatingKey+"/children", nil)
		if err == nil {
			seasons, parseErr := plexmeta.ParseSeasons(body, contentType)
			if parseErr == nil {
				previousRefs, nextRefs = discovery.extendAcrossSeasons(
					ctx, seasons, current, previousRefs, nextRefs, before, after,
				)
			}
		}
	}
	ratingKeys := make([]string, 0, len(previousRefs)+len(nextRefs))
	ratingKeys = append(ratingKeys, nextRefs...)
	ratingKeys = append(ratingKeys, previousRefs...)
	return discovery.confirmCandidates(ctx, ratingKeys)
}

func (discovery *Discovery) extendAcrossSeasons(
	ctx context.Context,
	seasons []plexmeta.Season,
	current plexmeta.Episode,
	previousRefs, nextRefs []string,
	before, after int,
) ([]string, []string) {
	position := seasonPosition(seasons, current.ParentRatingKey, current.SeasonIndex)
	if position < 0 {
		return previousRefs, nextRefs
	}
	for index := position + 1; index < len(seasons) && len(nextRefs) < after; index++ {
		if ctx.Err() != nil {
			break
		}
		episodes, err := discovery.readSeasonEpisodes(ctx, seasons[index].RatingKey)
		if err != nil {
			continue
		}
		for _, episode := range episodes {
			if usableEpisodeReference(episode) {
				nextRefs = append(nextRefs, episode.RatingKey)
				if len(nextRefs) == after {
					break
				}
			}
		}
	}
	for index := position - 1; index >= 0 && len(previousRefs) < before; index-- {
		if ctx.Err() != nil {
			break
		}
		episodes, err := discovery.readSeasonEpisodes(ctx, seasons[index].RatingKey)
		if err != nil {
			continue
		}
		for episodeIndex := len(episodes) - 1; episodeIndex >= 0; episodeIndex-- {
			if usableEpisodeReference(episodes[episodeIndex]) {
				previousRefs = append(previousRefs, episodes[episodeIndex].RatingKey)
				if len(previousRefs) == before {
					break
				}
			}
		}
	}
	return previousRefs, nextRefs
}

func (discovery *Discovery) readSeasonEpisodes(ctx context.Context, ratingKey string) ([]plexmeta.Episode, error) {
	if !numericIdentity(ratingKey) {
		return nil, ErrNoCandidates
	}
	body, contentType, err := discovery.get(ctx, "/library/metadata/"+ratingKey+"/children", nil)
	if err != nil {
		return nil, err
	}
	return plexmeta.ParseEpisodes(body, contentType)
}

func (discovery *Discovery) confirmCandidates(ctx context.Context, ratingKeys []string) ([]Candidate, error) {
	candidates := make([]Candidate, 0, len(ratingKeys))
	seen := make(map[string]struct{}, len(ratingKeys))
	for _, ratingKey := range ratingKeys {
		if ctx.Err() != nil {
			break
		}
		if _, exists := seen[ratingKey]; exists || !numericIdentity(ratingKey) {
			continue
		}
		seen[ratingKey] = struct{}{}
		candidate, err := discovery.confirmCandidate(ctx, ratingKey)
		if err == nil {
			candidates = append(candidates, candidate)
		}
	}
	if len(candidates) == 0 {
		return nil, ErrNoCandidates
	}
	return candidates, nil
}

func (discovery *Discovery) confirmCandidate(ctx context.Context, ratingKey string) (Candidate, error) {
	body, contentType, err := discovery.get(ctx, "/library/metadata/"+ratingKey, nil)
	if err != nil {
		return Candidate{}, ErrNoCandidates
	}
	parts, err := plexmeta.ParseParts(body, contentType)
	if err != nil {
		return Candidate{}, ErrNoCandidates
	}
	for _, part := range parts {
		if part.ID != "" && part.File != "" {
			return Candidate{RatingKey: ratingKey, Part: part}, nil
		}
	}
	return Candidate{}, ErrNoCandidates
}

func exactEpisode(body []byte, contentType, ratingKey string) (plexmeta.Episode, bool) {
	episodes, err := plexmeta.ParseEpisodes(body, contentType)
	if err != nil || len(episodes) != 1 || episodes[0].RatingKey != ratingKey {
		return plexmeta.Episode{}, false
	}
	return episodes[0], true
}

func (discovery *Discovery) get(ctx context.Context, endpointPath string, query url.Values) ([]byte, string, error) {
	if discovery == nil || discovery.baseURL == nil || discovery.client == nil {
		return nil, "", errors.New("Plex management client is unavailable")
	}
	target := *discovery.baseURL
	target.Path = strings.TrimRight(target.Path, "/") + endpointPath
	target.RawPath = ""
	target.RawQuery = query.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return nil, "", errors.New("create Plex management request")
	}
	request.Header.Set("X-Plex-Token", discovery.token)
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Accept-Encoding", "identity")
	if discovery.userAgent != "" {
		request.Header.Set("User-Agent", discovery.userAgent)
	}
	request.Host = discovery.baseURL.Host
	response, err := discovery.client.Do(request)
	if err != nil {
		return nil, "", errors.New("Plex management request failed")
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return nil, "", fmt.Errorf("Plex management request returned HTTP %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, discovery.maxBytes+1))
	if err != nil || int64(len(body)) > discovery.maxBytes {
		return nil, "", errors.New("Plex management response exceeds the limit")
	}
	return body, response.Header.Get("Content-Type"), nil
}

func splitEpisodeNeighbors(episodes []plexmeta.Episode, current plexmeta.Episode) ([]string, []string, bool) {
	currentPosition := -1
	for index, episode := range episodes {
		if episode.RatingKey == current.RatingKey {
			currentPosition = index
			break
		}
	}
	if currentPosition < 0 && current.EpisodeIndex >= 0 {
		for index, episode := range episodes {
			if episode.EpisodeIndex >= current.EpisodeIndex {
				currentPosition = index
				break
			}
		}
	}
	if currentPosition < 0 {
		return nil, nil, false
	}
	previous := make([]string, 0, currentPosition)
	for index := currentPosition - 1; index >= 0; index-- {
		if usableEpisodeReference(episodes[index]) {
			previous = append(previous, episodes[index].RatingKey)
		}
	}
	next := make([]string, 0, len(episodes)-currentPosition-1)
	start := currentPosition + 1
	if episodes[currentPosition].RatingKey != current.RatingKey {
		start = currentPosition
	}
	for index := start; index < len(episodes); index++ {
		if usableEpisodeReference(episodes[index]) {
			next = append(next, episodes[index].RatingKey)
		}
	}
	return previous, next, true
}

func seasonPosition(seasons []plexmeta.Season, ratingKey string, index int) int {
	for position, season := range seasons {
		if season.RatingKey == ratingKey {
			return position
		}
	}
	if index >= 0 {
		for position, season := range seasons {
			if season.Index == index {
				return position
			}
		}
	}
	return -1
}

func takeFirst(values []string, limit int) []string {
	if limit <= 0 {
		return nil
	}
	if len(values) <= limit {
		return values
	}
	return values[:limit]
}

func usableEpisodeReference(episode plexmeta.Episode) bool {
	return numericIdentity(episode.RatingKey)
}

func partsContainID(body []byte, contentType, partID string) bool {
	parts, err := plexmeta.ParseParts(body, contentType)
	if err != nil {
		return false
	}
	for _, part := range parts {
		if part.ID == partID {
			return true
		}
	}
	return false
}

func numericIdentity(value string) bool {
	if value == "" {
		return false
	}
	parsed, err := strconv.ParseUint(value, 10, 64)
	return err == nil && parsed > 0
}
