package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/InfinityPacer/plex-gateway/internal/pathmap"
)

const (
	transcodeMetadataPath = "/library/metadata/42"
	transcodePartPath     = "/library/parts/21/1/file"
	transcodeControlURL   = "http://public.invalid/redirect/pickcode/D.mkv?source=cloud"
	transcodeDecisionBody = `<MediaContainer><Video><Media videoDecision="directplay"><Part id="21" key="/library/parts/21/1/file" decision="directplay"/></Media></Video></MediaContainer>`
)

func TestGrantedCloudTranscodeStartRedirectsToDirectURL(t *testing.T) {
	for _, test := range []struct {
		name   string
		method string
		path   string
	}{
		{name: "start", method: http.MethodGet, path: "/video/:/transcode/universal/start"},
		{name: "dash", method: http.MethodGet, path: "/video/:/transcode/universal/start.mpd"},
		{name: "hls", method: http.MethodGet, path: "/video/:/transcode/universal/start.m3u8"},
		{name: "head", method: http.MethodHead, path: "/video/:/transcode/universal/start.mpd"},
	} {
		t.Run(test.name, func(t *testing.T) {
			localRoot := writeTranscodeSTRM(t)
			query := transcodeQuery()
			var metadataRequests, decisionRequests, partRequests, startRequests, mediaVaultRequests int

			mediaVault := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mediaVaultRequests++
				if r.Method != http.MethodGet {
					t.Fatalf("MediaVault method = %q", r.Method)
				}
				if r.URL.Path != "/redirect/pickcode/D.mkv" || r.URL.RawQuery != "source=cloud" {
					t.Fatalf("MediaVault request URI = %q", r.URL.RequestURI())
				}
				for name, want := range map[string]string{
					"X-Plex-Token":               "header-token",
					"Authorization":              "Bearer client-credential",
					"Cookie":                     "plex=session",
					"X-Plex-Client-Identifier":   "client-id",
					"X-Plex-Session-Identifier":  "session-id",
					"X-Plex-Session-Id":          "session-uuid",
					"X-Plex-Playback-Session-Id": "playback-session",
					"Accept-Language":            "zh-CN",
				} {
					if got := r.Header.Get(name); got != want {
						t.Fatalf("MediaVault %s = %q, want %q", name, got, want)
					}
				}
				if r.Header.Get("Protocol") != "" || r.Header.Get("Profileextra") != "" {
					t.Fatal("non-Plex transcode query leaked into MediaVault headers")
				}
				w.Header().Set("Location", "https://cdn.invalid/D.mkv?signature=private")
				w.WriteHeader(http.StatusFound)
			}))
			defer mediaVault.Close()

			plex := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case transcodeMetadataPath:
					metadataRequests++
					assertPlexQueryContext(t, r.URL.Query())
					if r.Header.Get("X-Plex-Token") != "header-token" || r.Header.Get("Cookie") != "plex=session" {
						t.Fatal("metadata headers were not preserved")
					}
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"MediaContainer":{"Metadata":[{"Media":[{"Part":[{"id":21,"key":"/library/parts/21/1/file","file":"/media/cloud/D.strm"}]}]}]}}`))
				case "/video/:/transcode/universal/decision":
					decisionRequests++
					if r.URL.Query().Get("directPlay") != "1" || r.URL.Query().Get("directStream") != "1" {
						t.Fatalf("decision query = %q", r.URL.RawQuery)
					}
					writeDirectPlayDecision(w)
				case transcodePartPath:
					partRequests++
					assertPlexQueryContext(t, r.URL.Query())
					if r.Method != http.MethodGet || r.Header.Get("Range") != "bytes=0-0" {
						t.Fatalf("Part authorization = %s Range=%q", r.Method, r.Header.Get("Range"))
					}
					w.Header().Set("Location", transcodeControlURL)
					w.WriteHeader(http.StatusMovedPermanently)
				case test.path:
					startRequests++
					http.Error(w, "Plex cannot open STRM", http.StatusBadRequest)
				default:
					http.NotFound(w, r)
				}
			}))
			defer plex.Close()

			handler, registry, cache := newCloudHandler(t, plex.URL, mediaVault.URL, []pathmap.Mapping{{
				PlexPrefix:  "/media/cloud",
				LocalPrefix: localRoot,
			}})
			performCloudDecision(t, handler, query, http.StatusOK)

			response := httptest.NewRecorder()
			handler.ServeHTTP(response, newTranscodeRequest(test.method, test.path, query))

			if response.Code != http.StatusFound || response.Header().Get("Location") != "https://cdn.invalid/D.mkv?signature=private" {
				t.Fatalf("status=%d Location=%q", response.Code, response.Header().Get("Location"))
			}
			if metadataRequests != 1 || decisionRequests != 1 || partRequests != 1 || startRequests != 0 || mediaVaultRequests != 1 {
				t.Fatalf("metadata=%d decision=%d Part=%d start=%d MediaVault=%d", metadataRequests, decisionRequests, partRequests, startRequests, mediaVaultRequests)
			}
			if part, ok := cache.Get("21"); !ok || part.PlexFilePath != "/media/cloud/D.strm" {
				t.Fatalf("Part was not cached: %#v %v", part, ok)
			}
			if snapshot := registry.Snapshot(); snapshot.RedirectSuccess != 1 || snapshot.PlexFallbackTotal != 0 {
				t.Fatalf("metrics = %#v", snapshot)
			}
		})
	}
}

func TestDecisionGrantExistsBeforeResponseBecomesVisible(t *testing.T) {
	localRoot := writeTranscodeSTRM(t)
	var startRequests int
	mediaVault := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", "https://cdn.invalid/D.mkv")
		w.WriteHeader(http.StatusFound)
	}))
	defer mediaVault.Close()
	plex := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case transcodeMetadataPath:
			w.Header().Set("Content-Type", "application/xml")
			_, _ = w.Write([]byte(`<MediaContainer><Video><Media><Part id="21" key="/library/parts/21/1/file" file="/media/cloud/D.strm"/></Media></Video></MediaContainer>`))
		case "/video/:/transcode/universal/decision":
			writeDirectPlayDecision(w)
		case transcodePartPath:
			w.Header().Set("Location", transcodeControlURL)
			w.WriteHeader(http.StatusMovedPermanently)
		case "/video/:/transcode/universal/start.mpd":
			startRequests++
			w.WriteHeader(http.StatusTeapot)
		default:
			http.NotFound(w, r)
		}
	}))
	defer plex.Close()

	handler, _, _ := newCloudHandler(t, plex.URL, mediaVault.URL, []pathmap.Mapping{{
		PlexPrefix:  "/media/cloud",
		LocalPrefix: localRoot,
	}})
	query := transcodeQuery()
	visible := &visibilityRecorder{ResponseRecorder: httptest.NewRecorder()}
	visible.onVisible = func() {
		start := httptest.NewRecorder()
		handler.ServeHTTP(start, newTranscodeRequest(http.MethodGet, "/video/:/transcode/universal/start.mpd", query))
		if start.Code != http.StatusFound {
			t.Fatalf("start status at decision visibility = %d", start.Code)
		}
	}
	handler.ServeHTTP(visible, newTranscodeRequest(http.MethodGet, "/video/:/transcode/universal/decision", query))
	if startRequests != 0 {
		t.Fatalf("Plex start requests = %d", startRequests)
	}
}

func TestTranscodeStartWithoutMatchingGrantRemainsPlexOwned(t *testing.T) {
	for _, test := range []struct {
		name            string
		performDecision bool
		mutate          func(url.Values)
	}{
		{name: "no decision"},
		{name: "session mismatch", performDecision: true, mutate: func(query url.Values) {
			query.Set("X-Plex-Playback-Session-Id", "different-session")
		}},
		{name: "part mismatch", performDecision: true, mutate: func(query url.Values) {
			query.Set("partIndex", "1")
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			localRoot := writeTranscodeSTRM(t)
			var metadataRequests, decisionRequests, startRequests int
			plex := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case transcodeMetadataPath:
					metadataRequests++
					w.Header().Set("Content-Type", "application/xml")
					_, _ = w.Write([]byte(`<MediaContainer><Video><Media><Part id="21" key="/library/parts/21/1/file" file="/media/cloud/D.strm"/></Media></Video></MediaContainer>`))
				case "/video/:/transcode/universal/decision":
					decisionRequests++
					writeDirectPlayDecision(w)
				case "/video/:/transcode/universal/start.mpd":
					startRequests++
					w.WriteHeader(http.StatusTeapot)
				default:
					http.NotFound(w, r)
				}
			}))
			defer plex.Close()

			handler, _, _ := newCloudHandler(t, plex.URL, "http://mediavault.invalid:7811", []pathmap.Mapping{{
				PlexPrefix:  "/media/cloud",
				LocalPrefix: localRoot,
			}})
			query := transcodeQuery()
			if test.performDecision {
				performCloudDecision(t, handler, query, http.StatusOK)
			}
			if test.mutate != nil {
				test.mutate(query)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, newTranscodeRequest(http.MethodGet, "/video/:/transcode/universal/start.mpd", query))

			if response.Code != http.StatusTeapot || startRequests != 1 {
				t.Fatalf("status=%d start=%d", response.Code, startRequests)
			}
			wantMetadata := 0
			wantDecision := 0
			if test.performDecision {
				wantMetadata = 1
				wantDecision = 1
			}
			if metadataRequests != wantMetadata || decisionRequests != wantDecision {
				t.Fatalf("metadata=%d decision=%d", metadataRequests, decisionRequests)
			}
		})
	}
}

func TestFailedDecisionDoesNotGrantTranscodeStart(t *testing.T) {
	localRoot := writeTranscodeSTRM(t)
	var startRequests int
	plex := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case transcodeMetadataPath:
			w.Header().Set("Content-Type", "application/xml")
			_, _ = w.Write([]byte(`<MediaContainer><Video><Media><Part id="21" key="/library/parts/21/1/file" file="/media/cloud/D.strm"/></Media></Video></MediaContainer>`))
		case "/video/:/transcode/universal/decision":
			http.Error(w, "decision rejected", http.StatusBadRequest)
		case "/video/:/transcode/universal/start.mpd":
			startRequests++
			w.WriteHeader(http.StatusTeapot)
		default:
			http.NotFound(w, r)
		}
	}))
	defer plex.Close()

	handler, _, _ := newCloudHandler(t, plex.URL, "http://mediavault.invalid:7811", []pathmap.Mapping{{
		PlexPrefix:  "/media/cloud",
		LocalPrefix: localRoot,
	}})
	query := transcodeQuery()
	performCloudDecision(t, handler, query, http.StatusBadRequest)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, newTranscodeRequest(http.MethodGet, "/video/:/transcode/universal/start.mpd", query))
	if response.Code != http.StatusTeapot || startRequests != 1 {
		t.Fatalf("status=%d start=%d", response.Code, startRequests)
	}
}

func TestTranscodeDecisionDoesNotGrantTranscodeStart(t *testing.T) {
	localRoot := writeTranscodeSTRM(t)
	var startRequests int
	plex := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case transcodeMetadataPath:
			w.Header().Set("Content-Type", "application/xml")
			_, _ = w.Write([]byte(`<MediaContainer><Video><Media><Part id="21" key="/library/parts/21/1/file" file="/media/cloud/D.strm"/></Media></Video></MediaContainer>`))
		case "/video/:/transcode/universal/decision":
			w.Header().Set("Content-Type", "application/xml")
			_, _ = w.Write([]byte(`<MediaContainer><Video><Media videoDecision="transcode"><Part id="21" key="/library/parts/21/1/file" decision="transcode"/></Media></Video></MediaContainer>`))
		case "/video/:/transcode/universal/start.mpd":
			startRequests++
			w.WriteHeader(http.StatusTeapot)
		default:
			http.NotFound(w, r)
		}
	}))
	defer plex.Close()

	handler, _, _ := newCloudHandler(t, plex.URL, "http://mediavault.invalid:7811", []pathmap.Mapping{{
		PlexPrefix:  "/media/cloud",
		LocalPrefix: localRoot,
	}})
	query := transcodeQuery()
	performCloudDecision(t, handler, query, http.StatusOK)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, newTranscodeRequest(http.MethodGet, "/video/:/transcode/universal/start.mpd", query))
	if response.Code != http.StatusTeapot || startRequests != 1 {
		t.Fatalf("status=%d start=%d", response.Code, startRequests)
	}
}

func TestNewTranscodeDecisionRevokesPreviousDirectPlayGrant(t *testing.T) {
	localRoot := writeTranscodeSTRM(t)
	decisionBody := transcodeDecisionBody
	var startRequests int
	plex := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case transcodeMetadataPath:
			w.Header().Set("Content-Type", "application/xml")
			_, _ = w.Write([]byte(`<MediaContainer><Video><Media><Part id="21" key="/library/parts/21/1/file" file="/media/cloud/D.strm"/></Media></Video></MediaContainer>`))
		case "/video/:/transcode/universal/decision":
			w.Header().Set("Content-Type", "application/xml")
			_, _ = w.Write([]byte(decisionBody))
		case "/video/:/transcode/universal/start.mpd":
			startRequests++
			w.WriteHeader(http.StatusTeapot)
		default:
			http.NotFound(w, r)
		}
	}))
	defer plex.Close()

	handler, _, _ := newCloudHandler(t, plex.URL, "http://mediavault.invalid:7811", []pathmap.Mapping{{
		PlexPrefix:  "/media/cloud",
		LocalPrefix: localRoot,
	}})
	query := transcodeQuery()
	performCloudDecision(t, handler, query, http.StatusOK)
	decisionBody = `<MediaContainer><Video><Media videoDecision="transcode"><Part id="21" key="/library/parts/21/1/file" decision="transcode"/></Media></Video></MediaContainer>`
	performCloudDecision(t, handler, query, http.StatusOK)

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, newTranscodeRequest(http.MethodGet, "/video/:/transcode/universal/start.mpd", query))
	if response.Code != http.StatusTeapot || startRequests != 1 {
		t.Fatalf("status=%d start=%d", response.Code, startRequests)
	}
}

func TestUnauthenticatedDecisionDoesNotRevokePreviousDirectPlayGrant(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*http.Request)
	}{
		{name: "missing token", mutate: func(request *http.Request) {
			query := request.URL.Query()
			query.Del("X-Plex-Token")
			request.URL.RawQuery = query.Encode()
			request.Header.Del("X-Plex-Token")
		}},
		{name: "invalid token", mutate: func(request *http.Request) {
			query := request.URL.Query()
			query.Set("X-Plex-Token", "invalid-token")
			request.URL.RawQuery = query.Encode()
			request.Header.Set("X-Plex-Token", "invalid-token")
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			localRoot := writeTranscodeSTRM(t)
			mediaVault := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Location", "https://cdn.invalid/D.mkv")
				w.WriteHeader(http.StatusFound)
			}))
			defer mediaVault.Close()
			plex := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case transcodeMetadataPath:
					if r.Header.Get("X-Plex-Token") != "header-token" {
						http.Error(w, http.StatusText(http.StatusForbidden), http.StatusForbidden)
						return
					}
					w.Header().Set("Content-Type", "application/xml")
					_, _ = w.Write([]byte(`<MediaContainer><Video><Media><Part id="21" key="/library/parts/21/1/file" file="/media/cloud/D.strm"/></Media></Video></MediaContainer>`))
				case "/video/:/transcode/universal/decision":
					writeDirectPlayDecision(w)
				case transcodePartPath:
					w.Header().Set("Location", transcodeControlURL)
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
			query := transcodeQuery()
			performCloudDecision(t, handler, query, http.StatusOK)
			unauthorized := newTranscodeRequest(http.MethodGet, "/video/:/transcode/universal/decision", query)
			test.mutate(unauthorized)
			handler.ServeHTTP(httptest.NewRecorder(), unauthorized)

			response := httptest.NewRecorder()
			handler.ServeHTTP(response, newTranscodeRequest(http.MethodGet, "/video/:/transcode/universal/start.mpd", query))
			if response.Code != http.StatusFound || response.Header().Get("Location") != "https://cdn.invalid/D.mkv" {
				t.Fatalf("status=%d Location=%q", response.Code, response.Header().Get("Location"))
			}
		})
	}
}

func TestGrantedTranscodeStartFailuresFallBackToOriginalPlexRequest(t *testing.T) {
	for _, test := range []struct {
		name                  string
		partStatus            int
		partLocation          string
		mediaVaultStatus      int
		wantMediaVaultRequest int
	}{
		{name: "authorization rejected", partStatus: http.StatusForbidden},
		{name: "authorization target mismatch", partStatus: http.StatusMovedPermanently, partLocation: "http://public.invalid/redirect/other"},
		{name: "resolver failure", partStatus: http.StatusMovedPermanently, partLocation: transcodeControlURL, mediaVaultStatus: http.StatusBadGateway, wantMediaVaultRequest: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			localRoot := writeTranscodeSTRM(t)
			var startRequests, mediaVaultRequests int
			mediaVault := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				mediaVaultRequests++
				w.WriteHeader(test.mediaVaultStatus)
			}))
			defer mediaVault.Close()

			plex := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case transcodeMetadataPath:
					w.Header().Set("Content-Type", "application/xml")
					_, _ = w.Write([]byte(`<MediaContainer><Video><Media><Part id="21" key="/library/parts/21/1/file" file="/media/cloud/D.strm"/></Media></Video></MediaContainer>`))
				case "/video/:/transcode/universal/decision":
					writeDirectPlayDecision(w)
				case transcodePartPath:
					if test.partLocation != "" {
						w.Header().Set("Location", test.partLocation)
					}
					w.WriteHeader(test.partStatus)
				case "/video/:/transcode/universal/start.mpd":
					startRequests++
					w.WriteHeader(http.StatusTeapot)
				default:
					http.NotFound(w, r)
				}
			}))
			defer plex.Close()

			handler, registry, _ := newCloudHandler(t, plex.URL, mediaVault.URL, []pathmap.Mapping{{
				PlexPrefix:  "/media/cloud",
				LocalPrefix: localRoot,
			}})
			query := transcodeQuery()
			performCloudDecision(t, handler, query, http.StatusOK)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, newTranscodeRequest(http.MethodGet, "/video/:/transcode/universal/start.mpd", query))

			if response.Code != http.StatusTeapot || startRequests != 1 || mediaVaultRequests != test.wantMediaVaultRequest {
				t.Fatalf("status=%d start=%d MediaVault=%d", response.Code, startRequests, mediaVaultRequests)
			}
			snapshot := registry.Snapshot()
			if snapshot.PlexFallbackTotal != 1 {
				t.Fatalf("metrics = %#v", snapshot)
			}
			if snapshot.RedirectFailure != 1 {
				t.Fatalf("metrics = %#v", snapshot)
			}
		})
	}
}

func TestCanceledGrantedTranscodeStartDoesNotFallback(t *testing.T) {
	localRoot := writeTranscodeSTRM(t)
	secondMediaVaultStarted := make(chan struct{})
	var mediaVaultRequests, startRequests atomic.Int64
	mediaVault := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if mediaVaultRequests.Add(1) == 1 {
			w.Header().Set("Location", "https://cdn.invalid/D.mkv?signature=private")
			w.WriteHeader(http.StatusFound)
			return
		}
		close(secondMediaVaultStarted)
		<-r.Context().Done()
	}))
	defer mediaVault.Close()

	plex := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case transcodeMetadataPath:
			w.Header().Set("Content-Type", "application/xml")
			_, _ = w.Write([]byte(`<MediaContainer><Video><Media><Part id="21" key="/library/parts/21/1/file" file="/media/cloud/D.strm"/></Media></Video></MediaContainer>`))
		case "/video/:/transcode/universal/decision":
			writeDirectPlayDecision(w)
		case transcodePartPath:
			w.Header().Set("Location", transcodeControlURL)
			w.WriteHeader(http.StatusMovedPermanently)
		case "/video/:/transcode/universal/start.mpd":
			startRequests.Add(1)
			w.WriteHeader(http.StatusTeapot)
		default:
			http.NotFound(w, r)
		}
	}))
	defer plex.Close()

	handler, registry, _ := newCloudHandler(t, plex.URL, mediaVault.URL, []pathmap.Mapping{{
		PlexPrefix:  "/media/cloud",
		LocalPrefix: localRoot,
	}})
	query := transcodeQuery()
	performCloudDecision(t, handler, query, http.StatusOK)
	firstResponse := httptest.NewRecorder()
	handler.ServeHTTP(firstResponse, newTranscodeRequest(http.MethodGet, "/video/:/transcode/universal/start.mpd", query))
	if firstResponse.Code != http.StatusFound {
		t.Fatalf("first start status = %d", firstResponse.Code)
	}

	request := newTranscodeRequest(http.MethodGet, "/video/:/transcode/universal/start.mpd", query)
	ctx, cancel := context.WithCancel(request.Context())
	request = request.WithContext(ctx)
	response := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		handler.ServeHTTP(response, request)
		close(done)
	}()

	select {
	case <-secondMediaVaultStarted:
	case <-time.After(time.Second):
		t.Fatal("second MediaVault request did not start")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("canceled start request did not return")
	}

	if mediaVaultRequests.Load() != 2 || startRequests.Load() != 0 {
		t.Fatalf("MediaVault=%d Plex start=%d", mediaVaultRequests.Load(), startRequests.Load())
	}
	if snapshot := registry.Snapshot(); snapshot.RedirectFailure != 0 ||
		snapshot.PlexFallbackTotal != 0 || snapshot.RedirectSuccess != 1 ||
		snapshot.ResolverLatencySamples != 1 || snapshot.ActiveRequests != 0 {
		t.Fatalf("metrics = %#v", snapshot)
	}
}

func TestCanceledGrantedTranscodeAuthorizationDoesNotFallback(t *testing.T) {
	localRoot := writeTranscodeSTRM(t)
	partProbeStarted := make(chan struct{})
	var startRequests, mediaVaultRequests atomic.Int64
	mediaVault := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		mediaVaultRequests.Add(1)
	}))
	defer mediaVault.Close()

	plex := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case transcodeMetadataPath:
			w.Header().Set("Content-Type", "application/xml")
			_, _ = w.Write([]byte(`<MediaContainer><Video><Media><Part id="21" key="/library/parts/21/1/file" file="/media/cloud/D.strm"/></Media></Video></MediaContainer>`))
		case "/video/:/transcode/universal/decision":
			writeDirectPlayDecision(w)
		case transcodePartPath:
			close(partProbeStarted)
			<-r.Context().Done()
		case "/video/:/transcode/universal/start.mpd":
			startRequests.Add(1)
			w.WriteHeader(http.StatusTeapot)
		default:
			http.NotFound(w, r)
		}
	}))
	defer plex.Close()

	handler, registry, _ := newCloudHandler(t, plex.URL, mediaVault.URL, []pathmap.Mapping{{
		PlexPrefix:  "/media/cloud",
		LocalPrefix: localRoot,
	}})
	query := transcodeQuery()
	performCloudDecision(t, handler, query, http.StatusOK)
	request := newTranscodeRequest(http.MethodGet, "/video/:/transcode/universal/start.mpd", query)
	ctx, cancel := context.WithCancel(request.Context())
	request = request.WithContext(ctx)
	done := make(chan struct{})
	go func() {
		handler.ServeHTTP(httptest.NewRecorder(), request)
		close(done)
	}()

	select {
	case <-partProbeStarted:
	case <-time.After(time.Second):
		t.Fatal("Part authorization did not start")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("canceled authorization did not return")
	}

	if startRequests.Load() != 0 || mediaVaultRequests.Load() != 0 {
		t.Fatalf("Plex start=%d MediaVault=%d", startRequests.Load(), mediaVaultRequests.Load())
	}
	if snapshot := registry.Snapshot(); snapshot.RedirectFailure != 0 ||
		snapshot.PlexFallbackTotal != 0 || snapshot.RedirectSuccess != 0 || snapshot.ActiveRequests != 0 {
		t.Fatalf("metrics = %#v", snapshot)
	}
}

func TestLocalTranscodeStartRemainsUnchanged(t *testing.T) {
	const originalQuery = "path=%2Flibrary%2Fmetadata%2F42&mediaIndex=0&partIndex=0&directPlay=0&directStream=0"
	var startQuery string
	plex := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/video/:/transcode/universal/start.mpd" {
			startQuery = r.URL.RawQuery
			w.WriteHeader(http.StatusAccepted)
			return
		}
		http.NotFound(w, r)
	}))
	defer plex.Close()

	handler, _, _ := newCloudHandler(t, plex.URL, "http://mediavault.invalid:7811", []pathmap.Mapping{{
		PlexPrefix:  "/media/cloud",
		LocalPrefix: t.TempDir(),
	}})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/video/:/transcode/universal/start.mpd?"+originalQuery, nil))
	if response.Code != http.StatusAccepted || startQuery != originalQuery {
		t.Fatalf("status=%d query=%q", response.Code, startQuery)
	}
}

func transcodeQuery() url.Values {
	query := url.Values{}
	query.Set("path", transcodeMetadataPath)
	query.Set("mediaIndex", "0")
	query.Set("partIndex", "0")
	query.Set("directPlay", "0")
	query.Set("directStream", "0")
	query.Set("protocol", "dash")
	query.Set("X-Plex-Token", "query-token")
	query.Set("X-Plex-Client-Identifier", "client-id")
	query.Set("X-Plex-Session-Identifier", "session-id")
	query.Set("X-Plex-Session-Id", "session-uuid")
	query.Set("X-Plex-Playback-Session-Id", "playback-session")
	query.Set("Accept-Language", "zh-CN")
	query.Add("profileExtra", "first")
	query.Add("profileExtra", "second")
	return query
}

func performCloudDecision(t *testing.T, handler http.Handler, query url.Values, wantStatus int) {
	t.Helper()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, newTranscodeRequest(http.MethodGet, "/video/:/transcode/universal/decision", query))
	if response.Code != wantStatus {
		t.Fatalf("decision status = %d, want %d", response.Code, wantStatus)
	}
}

func writeDirectPlayDecision(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/xml")
	_, _ = w.Write([]byte(transcodeDecisionBody))
}

func newTranscodeRequest(method, path string, query url.Values) *http.Request {
	request := httptest.NewRequest(method, path+"?"+query.Encode(), nil)
	request.Header.Set("X-Plex-Token", "header-token")
	request.Header.Set("Authorization", "Bearer client-credential")
	request.Header.Set("Cookie", "plex=session")
	return request
}

func writeTranscodeSTRM(t *testing.T) string {
	t.Helper()
	localRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(localRoot, "D.strm"), []byte(transcodeControlURL+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return localRoot
}

func assertPlexQueryContext(t *testing.T, query url.Values) {
	t.Helper()
	for name, want := range map[string]string{
		"path":                       transcodeMetadataPath,
		"mediaIndex":                 "0",
		"partIndex":                  "0",
		"X-Plex-Token":               "query-token",
		"X-Plex-Client-Identifier":   "client-id",
		"X-Plex-Session-Identifier":  "session-id",
		"X-Plex-Session-Id":          "session-uuid",
		"X-Plex-Playback-Session-Id": "playback-session",
		"Accept-Language":            "zh-CN",
	} {
		if got := query.Get(name); got != want {
			t.Fatalf("%s = %q, want %q", name, got, want)
		}
	}
	if got := query["profileExtra"]; len(got) != 2 || got[0] != "first" || got[1] != "second" {
		t.Fatalf("profileExtra = %#v", got)
	}
}

type visibilityRecorder struct {
	*httptest.ResponseRecorder
	onVisible func()
	visible   bool
}

func (w *visibilityRecorder) WriteHeader(status int) {
	if !w.visible {
		w.visible = true
		w.onVisible()
	}
	w.ResponseRecorder.WriteHeader(status)
}
