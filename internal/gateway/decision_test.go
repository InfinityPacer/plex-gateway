package gateway

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/InfinityPacer/plex-gateway/internal/pathmap"
)

func TestCloudDecisionForcesDirectPlayAndContinuesThroughPartRedirect(t *testing.T) {
	localRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(localRoot, "D.strm"), []byte("http://public.invalid/redirect/pickcode/D.mkv\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var mediaVaultRequests int
	mediaVault := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mediaVaultRequests++
		if r.URL.Path != "/redirect/pickcode/D.mkv" {
			t.Fatalf("MediaVault path = %q", r.URL.Path)
		}
		w.Header().Set("Location", "https://cdn.invalid/D.mkv?signature=private")
		w.WriteHeader(http.StatusFound)
	}))
	defer mediaVault.Close()

	var metadataRequests, decisionRequests, partRequests int
	plex := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/library/metadata/42":
			metadataRequests++
			if r.Method != http.MethodGet || r.URL.Query().Get("X-Plex-Token") != "query-token" {
				t.Fatalf("metadata request = %s %s", r.Method, r.URL.RequestURI())
			}
			if r.Header.Get("X-Plex-Token") != "header-token" || r.Header.Get("Cookie") != "plex=session" {
				t.Fatalf("metadata credentials were not preserved")
			}
			if r.Header.Get("X-Plex-Client-Identifier") != "client-id" {
				t.Fatalf("metadata client identifier = %q", r.Header.Get("X-Plex-Client-Identifier"))
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"MediaContainer":{"Metadata":[{"Media":[{"Part":[{"id":10,"key":"/library/parts/10/1/file","file":"/media/local/A.mkv"}]},{"Part":[{"id":20,"key":"/library/parts/20/1/file","file":"/media/cloud/C.strm"},{"id":21,"key":"/library/parts/21/1/file","file":"/media/cloud/D.strm"}]}]}]}}`))
		case "/video/:/transcode/universal/decision":
			decisionRequests++
			query := r.URL.Query()
			if got := query["directPlay"]; len(got) != 1 || got[0] != "1" {
				t.Fatalf("directPlay = %#v", got)
			}
			if got := query["directStream"]; len(got) != 1 || got[0] != "1" {
				t.Fatalf("directStream = %#v", got)
			}
			if got := query["hasMDE"]; len(got) != 1 || got[0] != "1" {
				t.Fatalf("hasMDE = %#v", got)
			}
			if got := query["profileExtra"]; len(got) != 2 || got[0] != "first" || got[1] != "second" {
				t.Fatalf("profileExtra = %#v", got)
			}
			if r.Header.Get("X-Plex-Client-Identifier") != "client-id" || r.Header.Get("Cookie") != "plex=session" {
				t.Fatal("decision headers were not preserved")
			}
			w.Header().Set("Content-Type", "application/xml")
			_, _ = w.Write([]byte(`<MediaContainer><Video><Media decision="directplay"><Part decision="directplay" key="/library/parts/21/1/file"/></Media></Video></MediaContainer>`))
		case "/library/parts/21/1/file":
			partRequests++
			if r.Header.Get("X-Plex-Token") != "header-token" {
				http.Error(w, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)
				return
			}
			w.Header().Set("Location", "http://public.invalid/redirect/pickcode/D.mkv")
			w.WriteHeader(http.StatusMovedPermanently)
		default:
			http.NotFound(w, r)
		}
	}))
	defer plex.Close()

	handler, _, cache := newCloudHandler(t, plex.URL, mediaVault.URL, []pathmap.Mapping{{
		PlexPrefix:  "/media/cloud",
		LocalPrefix: localRoot,
	}})
	query := url.Values{}
	query.Set("path", "/library/metadata/42")
	query.Set("mediaIndex", "1")
	query.Set("partIndex", "1")
	query.Add("directPlay", "0")
	query.Add("directPlay", "0")
	query.Set("directStream", "0")
	query.Set("hasMDE", "0")
	query.Add("hasMDE", "1")
	query.Add("profileExtra", "first")
	query.Add("profileExtra", "second")
	query.Set("X-Plex-Token", "query-token")
	decisionRequest := httptest.NewRequest(http.MethodGet, "/video/:/transcode/universal/decision?"+query.Encode(), nil)
	decisionRequest.Header.Set("X-Plex-Token", "header-token")
	decisionRequest.Header.Set("X-Plex-Client-Identifier", "client-id")
	decisionRequest.Header.Set("Cookie", "plex=session")
	decisionResponse := httptest.NewRecorder()
	handler.ServeHTTP(decisionResponse, decisionRequest)

	if decisionResponse.Code != http.StatusOK || !strings.Contains(decisionResponse.Body.String(), `decision="directplay"`) {
		t.Fatalf("decision status = %d body = %q", decisionResponse.Code, decisionResponse.Body.String())
	}
	if part, ok := cache.Get("21"); !ok || part.PlexFilePath != "/media/cloud/D.strm" {
		t.Fatalf("selected Part was not cached: %#v, %v", part, ok)
	}

	partRequest := httptest.NewRequest(http.MethodGet, "/library/parts/21/1/file", nil)
	partRequest.Header.Set("X-Plex-Token", "header-token")
	partResponse := httptest.NewRecorder()
	handler.ServeHTTP(partResponse, partRequest)
	if partResponse.Code != http.StatusFound || partResponse.Header().Get("Location") != "https://cdn.invalid/D.mkv?signature=private" {
		t.Fatalf("Part status = %d Location = %q", partResponse.Code, partResponse.Header().Get("Location"))
	}
	if metadataRequests != 1 || decisionRequests != 1 || partRequests != 1 || mediaVaultRequests != 1 {
		t.Fatalf("requests metadata=%d decision=%d part=%d MediaVault=%d", metadataRequests, decisionRequests, partRequests, mediaVaultRequests)
	}
}

func TestCloudDecisionInfersOmittedMediaIndexFromUniqueMetadata(t *testing.T) {
	localRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(localRoot, "Unique.strm"), []byte("http://public.invalid/redirect/pickcode/Unique.mkv\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	mediaVault := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", "https://cdn.invalid/Unique.mkv?signature=private")
		w.WriteHeader(http.StatusFound)
	}))
	defer mediaVault.Close()

	var decisionRequests, partRequests int
	plex := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/library/metadata/42":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"MediaContainer":{"Metadata":[{"Media":[{"Part":[{"id":20,"key":"/library/parts/20/1/file","file":"/media/cloud/Unique.strm"}]}]}]}}`))
		case "/video/:/transcode/universal/decision":
			decisionRequests++
			query := r.URL.Query()
			if query.Get("mediaIndex") != "0" || query.Get("partIndex") != "0" || query.Get("directPlay") != "1" || query.Get("hasMDE") != "1" {
				t.Fatalf("adapted decision query = %q", r.URL.RawQuery)
			}
			w.Header().Set("Content-Type", "application/xml")
			_, _ = w.Write([]byte(`<MediaContainer><Video><Media decision="directplay"><Part decision="directplay" key="/library/parts/20/1/file"/></Media></Video></MediaContainer>`))
		case "/library/parts/20/1/file":
			partRequests++
			w.Header().Set("Location", "http://public.invalid/redirect/pickcode/Unique.mkv")
			w.WriteHeader(http.StatusMovedPermanently)
		default:
			http.NotFound(w, r)
		}
	}))
	defer plex.Close()

	handler, _, _ := newCloudHandler(t, plex.URL, mediaVault.URL, []pathmap.Mapping{{
		PlexPrefix:  "/media/cloud",
		LocalPrefix: localRoot,
	}})
	query := url.Values{
		"path":                       {"/library/metadata/42"},
		"partIndex":                  {"0"},
		"directPlay":                 {"0"},
		"directStream":               {"0"},
		"X-Plex-Playback-Session-Id": {"apple-tv-session"},
	}
	decisionRequest := httptest.NewRequest(http.MethodGet, "/video/:/transcode/universal/decision?"+query.Encode(), nil)
	decisionRequest.Header.Set("X-Plex-Token", "client-token")
	decisionResponse := httptest.NewRecorder()
	handler.ServeHTTP(decisionResponse, decisionRequest)
	if decisionResponse.Code != http.StatusOK {
		t.Fatalf("decision status = %d body = %q", decisionResponse.Code, decisionResponse.Body.String())
	}

	startRequest := httptest.NewRequest(http.MethodGet, "/video/:/transcode/universal/start.m3u8?"+query.Encode(), nil)
	startRequest.Header.Set("X-Plex-Token", "client-token")
	startResponse := httptest.NewRecorder()
	handler.ServeHTTP(startResponse, startRequest)
	if startResponse.Code != http.StatusFound || startResponse.Header().Get("Location") != "https://cdn.invalid/Unique.mkv?signature=private" {
		t.Fatalf("start status = %d Location = %q", startResponse.Code, startResponse.Header().Get("Location"))
	}
	if decisionRequests != 1 || partRequests != 1 {
		t.Fatalf("decision requests = %d, part requests = %d", decisionRequests, partRequests)
	}
}

func TestOmittedMediaIndexWithAmbiguousMetadataRemainsUnchanged(t *testing.T) {
	originalQuery := "path=%2Flibrary%2Fmetadata%2F42&partIndex=0&directPlay=0&directStream=0"
	var metadataRequests int
	var decisionQuery string
	plex := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/library/metadata/42":
			metadataRequests++
			w.Header().Set("Content-Type", "application/xml")
			_, _ = w.Write([]byte(`<MediaContainer><Video><Media><Part id="10" file="/media/cloud/A.strm"/></Media><Media><Part id="20" file="/media/cloud/B.strm"/></Media></Video></MediaContainer>`))
		case "/video/:/transcode/universal/decision":
			decisionQuery = r.URL.RawQuery
			http.Error(w, "original decision", http.StatusBadRequest)
		default:
			http.NotFound(w, r)
		}
	}))
	defer plex.Close()

	handler, _, _ := newCloudHandler(t, plex.URL, "http://mediavault.invalid:7811", []pathmap.Mapping{{
		PlexPrefix:  "/media/cloud",
		LocalPrefix: t.TempDir(),
	}})
	request := httptest.NewRequest(http.MethodGet, "/video/:/transcode/universal/decision?"+originalQuery, nil)
	request.Header.Set("X-Plex-Token", "client-token")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || metadataRequests != 1 || decisionQuery != originalQuery {
		t.Fatalf("status=%d metadata=%d query=%q", response.Code, metadataRequests, decisionQuery)
	}
}

func TestLocalDecisionRemainsUnchanged(t *testing.T) {
	originalQuery := "path=%2Flibrary%2Fmetadata%2F42&partIndex=0&directPlay=0&directStream=0"
	var metadataRequests int
	var decisionQuery string
	plex := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/library/metadata/42":
			metadataRequests++
			w.Header().Set("Content-Type", "application/xml")
			_, _ = w.Write([]byte(`<MediaContainer><Video><Media><Part id="10" key="/library/parts/10/1/file" file="/media/local/A.mkv"/></Media></Video></MediaContainer>`))
		case "/video/:/transcode/universal/decision":
			decisionQuery = r.URL.RawQuery
			http.Error(w, "original decision", http.StatusBadRequest)
		default:
			http.NotFound(w, r)
		}
	}))
	defer plex.Close()

	handler, _, _ := newCloudHandler(t, plex.URL, "http://mediavault.invalid:7811", []pathmap.Mapping{{
		PlexPrefix:  "/media/cloud",
		LocalPrefix: t.TempDir(),
	}})
	request := httptest.NewRequest(http.MethodGet, "/video/:/transcode/universal/decision?"+originalQuery, nil)
	request.Header.Set("X-Plex-Token", "client-token")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || metadataRequests != 1 || decisionQuery != originalQuery {
		t.Fatalf("status=%d metadata=%d decision_query=%q", response.Code, metadataRequests, decisionQuery)
	}
}

func TestAmbiguousDecisionIndicesFailOpenWithoutMetadataProbe(t *testing.T) {
	for _, test := range []struct {
		name  string
		query string
	}{
		{name: "automatic media", query: "path=%2Flibrary%2Fmetadata%2F42&mediaIndex=-1&partIndex=0&directPlay=0&directStream=0"},
		{name: "automatic part", query: "path=%2Flibrary%2Fmetadata%2F42&mediaIndex=0&partIndex=-1&directPlay=0&directStream=0"},
		{name: "missing part", query: "path=%2Flibrary%2Fmetadata%2F42&mediaIndex=0&directPlay=0&directStream=0"},
		{name: "duplicate media", query: "path=%2Flibrary%2Fmetadata%2F42&mediaIndex=0&mediaIndex=1&partIndex=0&directPlay=0&directStream=0"},
		{name: "empty media", query: "path=%2Flibrary%2Fmetadata%2F42&mediaIndex=&partIndex=0&directPlay=0&directStream=0"},
		{name: "empty part", query: "path=%2Flibrary%2Fmetadata%2F42&partIndex=&directPlay=0&directStream=0"},
		{name: "duplicate part", query: "path=%2Flibrary%2Fmetadata%2F42&partIndex=0&partIndex=1&directPlay=0&directStream=0"},
		{name: "omitted media with nonzero part", query: "path=%2Flibrary%2Fmetadata%2F42&partIndex=1&directPlay=0&directStream=0"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var metadataRequests int
			var decisionQuery string
			plex := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/library/metadata/42" {
					metadataRequests++
				}
				if r.URL.Path == "/video/:/transcode/universal/decision" {
					decisionQuery = r.URL.RawQuery
					w.WriteHeader(http.StatusNoContent)
					return
				}
				http.NotFound(w, r)
			}))
			defer plex.Close()

			handler, _, _ := newCloudHandler(t, plex.URL, "http://mediavault.invalid:7811", []pathmap.Mapping{{
				PlexPrefix:  "/media/cloud",
				LocalPrefix: t.TempDir(),
			}})
			request := httptest.NewRequest(http.MethodGet, "/video/:/transcode/universal/decision?"+test.query, nil)
			request.Header.Set("X-Plex-Token", "client-token")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusNoContent || metadataRequests != 0 || decisionQuery != test.query {
				t.Fatalf("status=%d metadata=%d query=%q", response.Code, metadataRequests, decisionQuery)
			}
		})
	}
}

func TestDecisionMetadataFailureFallsBackToOriginalRequest(t *testing.T) {
	originalQuery := "path=%2Flibrary%2Fmetadata%2F42&mediaIndex=0&partIndex=0&directPlay=0&directStream=0"
	var decisionQuery string
	plex := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/library/metadata/42":
			http.Error(w, http.StatusText(http.StatusForbidden), http.StatusForbidden)
		case "/video/:/transcode/universal/decision":
			decisionQuery = r.URL.RawQuery
			http.Error(w, "original decision", http.StatusBadRequest)
		default:
			http.NotFound(w, r)
		}
	}))
	defer plex.Close()

	handler, _, _ := newCloudHandler(t, plex.URL, "http://mediavault.invalid:7811", []pathmap.Mapping{{
		PlexPrefix:  "/media/cloud",
		LocalPrefix: t.TempDir(),
	}})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/video/:/transcode/universal/decision?"+originalQuery, nil))
	if response.Code != http.StatusBadRequest || decisionQuery != originalQuery {
		t.Fatalf("status = %d decision query = %q", response.Code, decisionQuery)
	}
}

func TestCloudDecisionWithoutPlexTokenDoesNotProbeMetadata(t *testing.T) {
	var metadataRequests, decisionRequests int
	plex := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/library/metadata/42":
			metadataRequests++
			w.WriteHeader(http.StatusOK)
		case "/video/:/transcode/universal/decision":
			decisionRequests++
			w.WriteHeader(http.StatusTeapot)
		default:
			http.NotFound(w, r)
		}
	}))
	defer plex.Close()

	handler, _, _ := newCloudHandler(t, plex.URL, "http://mediavault.invalid:7811", []pathmap.Mapping{{
		PlexPrefix:  "/media/cloud",
		LocalPrefix: t.TempDir(),
	}})
	query := url.Values{
		"path":       {"/library/metadata/42"},
		"mediaIndex": {"0"},
		"partIndex":  {"0"},
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/video/:/transcode/universal/decision?"+query.Encode(), nil))

	if recorder.Code != http.StatusTeapot || metadataRequests != 0 || decisionRequests != 1 {
		t.Fatalf("status=%d metadata=%d decision=%d", recorder.Code, metadataRequests, decisionRequests)
	}
}

func TestDecisionRequiresReadableValidSTRMTarget(t *testing.T) {
	for _, test := range []struct {
		name    string
		content string
	}{
		{name: "missing file"},
		{name: "invalid target", content: "ftp://media.invalid/Movie.mkv\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			localRoot := t.TempDir()
			if test.content != "" {
				if err := os.WriteFile(filepath.Join(localRoot, "Movie.strm"), []byte(test.content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			var decisionQuery string
			plex := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/library/metadata/42":
					w.Header().Set("Content-Type", "application/xml")
					_, _ = w.Write([]byte(`<MediaContainer><Video><Media><Part id="10" key="/library/parts/10/1/file" file="/media/cloud/Movie.strm"/></Media></Video></MediaContainer>`))
				case "/video/:/transcode/universal/decision":
					decisionQuery = r.URL.RawQuery
					w.WriteHeader(http.StatusNoContent)
				default:
					http.NotFound(w, r)
				}
			}))
			defer plex.Close()

			handler, _, _ := newCloudHandler(t, plex.URL, "http://mediavault.invalid:7811", []pathmap.Mapping{{
				PlexPrefix:  "/media/cloud",
				LocalPrefix: localRoot,
			}})
			originalQuery := "path=%2Flibrary%2Fmetadata%2F42&mediaIndex=0&partIndex=0&directPlay=0&directStream=0"
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/video/:/transcode/universal/decision?"+originalQuery, nil))

			if response.Code != http.StatusNoContent || decisionQuery != originalQuery {
				t.Fatalf("status=%d query=%q", response.Code, decisionQuery)
			}
		})
	}
}
