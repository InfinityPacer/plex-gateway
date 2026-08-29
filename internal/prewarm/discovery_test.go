package prewarm

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strconv"
	"testing"
	"time"
)

func TestDiscoveryUsesBidirectionalPlayQueueAndKeepsTokenAtPlex(t *testing.T) {
	var paths []string
	plex := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Plex-Token"); got != "management-secret" {
			t.Fatalf("token = %q", got)
		}
		if r.URL.Query().Get("X-Plex-Token") != "" {
			t.Fatal("token leaked into query")
		}
		paths = append(paths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/library/metadata/42":
			_, _ = io.WriteString(w, episodeJSON("42", "7", "3", 1, 2, "9", "/cloud/e02.strm"))
		case "/playQueues/5":
			_, _ = io.WriteString(w, `{"MediaContainer":{"Metadata":[`+
				`{"type":"movie","ratingKey":"40","playQueueItemID":"98"},`+
				`{"type":"episode","ratingKey":"42","playQueueItemID":"99"},`+
				`{"type":"movie","ratingKey":"70","playQueueItemID":"100"},`+
				`{"type":"episode","ratingKey":"44","playQueueItemID":"101"}]}}`)
		case "/library/metadata/40":
			_, _ = io.WriteString(w, itemJSON("8", "/cloud/previous.strm"))
		case "/library/metadata/70":
			_, _ = io.WriteString(w, itemJSON("11", "/cloud/movie.strm"))
		case "/library/metadata/44":
			_, _ = io.WriteString(w, itemJSON("12", "/cloud/e04.strm"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer plex.Close()

	discovery := newTestDiscovery(t, plex.URL)
	candidates, err := discovery.Neighbors(t.Context(), PlaybackContext{
		RatingKey: "42", PartID: "9", PlayQueueID: "5", PlayQueueItemID: "99",
	}, 2, 3)
	if err != nil {
		t.Fatal(err)
	}
	if got := candidateIDs(candidates); !reflect.DeepEqual(got, []string{"11", "12", "8"}) {
		t.Fatalf("candidate parts = %v", got)
	}
	wantPaths := []string{
		"/library/metadata/42", "/playQueues/5", "/library/metadata/70",
		"/library/metadata/44", "/library/metadata/40",
	}
	if !reflect.DeepEqual(paths, wantPaths) {
		t.Fatalf("paths = %v, want %v", paths, wantPaths)
	}
}

func TestDiscoveryBuildsLibraryWindowAcrossSpecialAndRegularSeasons(t *testing.T) {
	var paths []string
	plex := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/library/metadata/42":
			_, _ = io.WriteString(w, episodeJSON("42", "7", "3", 1, 2, "9", "/cloud/e02.strm"))
		case "/library/metadata/7/children":
			_, _ = io.WriteString(w, episodeListJSON(
				episodeRefJSON("41", "7", "3", 1, 1),
				episodeRefJSON("42", "7", "3", 1, 2),
				episodeRefJSON("43", "7", "3", 1, 3),
			))
		case "/library/metadata/3/children":
			_, _ = io.WriteString(w, `{"MediaContainer":{"Metadata":[`+
				`{"type":"season","ratingKey":"6","index":0},`+
				`{"type":"season","ratingKey":"7","index":1},`+
				`{"type":"season","ratingKey":"8","index":2}]}}`)
		case "/library/metadata/6/children":
			_, _ = io.WriteString(w, episodeListJSON(episodeRefJSON("5", "6", "3", 0, 1)))
		case "/library/metadata/8/children":
			_, _ = io.WriteString(w, episodeListJSON(
				episodeRefJSON("50", "8", "3", 2, 1),
				episodeRefJSON("51", "8", "3", 2, 2),
			))
		case "/library/metadata/5":
			_, _ = io.WriteString(w, itemJSON("5", "/cloud/special.strm"))
		case "/library/metadata/41":
			_, _ = io.WriteString(w, itemJSON("8", "/cloud/e01.strm"))
		case "/library/metadata/43":
			_, _ = io.WriteString(w, itemJSON("10", "/cloud/e03.strm"))
		case "/library/metadata/50":
			_, _ = io.WriteString(w, itemJSON("12", "/cloud/s02e01.strm"))
		case "/library/metadata/51":
			_, _ = io.WriteString(w, itemJSON("13", "/cloud/s02e02.strm"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer plex.Close()

	discovery := newTestDiscovery(t, plex.URL)
	candidates, err := discovery.Neighbors(t.Context(), PlaybackContext{RatingKey: "42", PartID: "9"}, 2, 3)
	if err != nil {
		t.Fatal(err)
	}
	if got := candidateIDs(candidates); !reflect.DeepEqual(got, []string{"10", "12", "13", "8", "5"}) {
		t.Fatalf("candidate parts = %v", got)
	}
	if len(paths) != 10 {
		t.Fatalf("paths = %v", paths)
	}
}

func TestDiscoveryAcceptsActivePartAmongMultipleAndChoosesFirstCandidatePart(t *testing.T) {
	plex := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/library/metadata/42":
			_, _ = io.WriteString(w, `{"MediaContainer":{"Metadata":[{"type":"episode","ratingKey":"42","parentRatingKey":"7","grandparentRatingKey":"3","parentIndex":1,"index":2,"Media":[{"Part":[{"id":"8","file":"/a.strm"},{"id":"9","file":"/b.strm"}]}]}]}}`)
		case "/playQueues/5":
			_, _ = io.WriteString(w, `{"MediaContainer":{"Metadata":[{"ratingKey":"42","playQueueItemID":"99"},{"ratingKey":"44","playQueueItemID":"100"}]}}`)
		case "/library/metadata/44":
			_, _ = io.WriteString(w, `{"MediaContainer":{"Metadata":[{"ratingKey":"44","Media":[{"Part":[{"id":"11","file":"/first.strm"},{"id":"12","file":"/second.strm"}]}]}]}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer plex.Close()
	discovery := newTestDiscovery(t, plex.URL)
	candidates, err := discovery.Neighbors(t.Context(), PlaybackContext{
		RatingKey: "42", PartID: "9", PlayQueueID: "5", PlayQueueItemID: "99",
	}, 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 || candidates[0].Part.ID != "11" {
		t.Fatalf("candidates = %#v", candidates)
	}
}

func TestDiscoveryRejectsCurrentMetadataWithoutActivePart(t *testing.T) {
	plex := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, itemJSON("8", "/cloud/other.strm"))
	}))
	defer plex.Close()
	discovery := newTestDiscovery(t, plex.URL)
	_, err := discovery.Neighbors(t.Context(), PlaybackContext{RatingKey: "42", PartID: "9"}, 2, 3)
	if err != ErrUntrustedCurrent {
		t.Fatalf("error = %v", err)
	}
}

func TestDiscoveryDoesNotFollowRedirectWithManagementToken(t *testing.T) {
	received := make(chan http.Header, 1)
	target := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		received <- r.Header.Clone()
	}))
	defer target.Close()
	plex := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", target.URL)
		w.WriteHeader(http.StatusFound)
	}))
	defer plex.Close()
	discovery := newTestDiscovery(t, plex.URL)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := discovery.Validate(ctx); err == nil {
		t.Fatal("Validate() accepted redirect")
	}
	select {
	case header := <-received:
		t.Fatalf("redirect target received headers %#v", header)
	default:
	}
}

func newTestDiscovery(t *testing.T, rawURL string) *Discovery {
	t.Helper()
	baseURL, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	discovery, err := NewDiscovery(DiscoveryOptions{
		BaseURL: baseURL, Token: "management-secret", Client: &http.Client{Timeout: time.Second},
	})
	if err != nil {
		t.Fatal(err)
	}
	return discovery
}

func candidateIDs(candidates []Candidate) []string {
	result := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		result = append(result, candidate.Part.ID)
	}
	return result
}

func itemJSON(partID, file string) string {
	return `{"MediaContainer":{"Metadata":[{"Media":[{"Part":[{"id":"` + partID + `","file":"` + file + `"}]}]}]}}`
}

func episodeListJSON(items ...string) string {
	result := `{"MediaContainer":{"Metadata":[`
	for index, item := range items {
		if index > 0 {
			result += ","
		}
		result += item
	}
	return result + `]}}`
}

func episodeRefJSON(ratingKey, parentRatingKey, grandparentRatingKey string, season, episode int) string {
	return `{"type":"episode","ratingKey":"` + ratingKey + `","parentRatingKey":"` + parentRatingKey + `","grandparentRatingKey":"` + grandparentRatingKey + `","parentIndex":` + strconv.Itoa(season) + `,"index":` + strconv.Itoa(episode) + `}`
}

func episodeJSON(ratingKey, parentRatingKey, grandparentRatingKey string, season, episode int, partID, file string) string {
	return `{"MediaContainer":{"Metadata":[{"type":"episode","ratingKey":"` + ratingKey + `","parentRatingKey":"` + parentRatingKey + `","grandparentRatingKey":"` + grandparentRatingKey + `","parentIndex":` +
		strconv.Itoa(season) + `,"index":` + strconv.Itoa(episode) + `,"Media":[{"Part":[{"id":"` + partID + `","key":"/library/parts/` + partID + `/file","file":"` + file + `"}]}]}]}}`
}
