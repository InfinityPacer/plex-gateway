package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/InfinityPacer/plex-gateway/internal/metrics"
	"github.com/InfinityPacer/plex-gateway/internal/partcache"
	"github.com/InfinityPacer/plex-gateway/internal/pathmap"
	"github.com/InfinityPacer/plex-gateway/internal/prewarm"
	"github.com/InfinityPacer/plex-gateway/internal/resolver"
	"github.com/InfinityPacer/plex-gateway/internal/trace"
)

func TestTransparentProxyPreservesRequestAndResponse(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s", r.Method)
		}
		if r.URL.RequestURI() != "/library/metadata/42?includeMarkers=1" {
			t.Fatalf("request URI = %q", r.URL.RequestURI())
		}
		if r.Header.Get("X-Plex-Token") != "token-value" {
			t.Fatal("X-Plex-Token was not forwarded")
		}
		w.Header().Set("X-Plex-Protocol", "1.0")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write([]byte("plex-response"))
	}))
	defer upstream.Close()

	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	handler := New(Options{
		Upstream: upstreamURL,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Tracer:   trace.New(false, nil),
	})
	request := httptest.NewRequest(http.MethodPost, "/library/metadata/42?includeMarkers=1", nil)
	request.Header.Set("X-Plex-Token", "token-value")
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusPartialContent {
		t.Fatalf("status = %d", recorder.Code)
	}
	if recorder.Header().Get("X-Plex-Protocol") != "1.0" {
		t.Fatal("response header was not preserved")
	}
	if recorder.Body.String() != "plex-response" {
		t.Fatalf("body = %q", recorder.Body.String())
	}
}

func TestHealthDoesNotReachPlex(t *testing.T) {
	reached := false
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		reached = true
	}))
	defer upstream.Close()
	upstreamURL, _ := url.Parse(upstream.URL)
	handler := New(Options{
		Upstream: upstreamURL,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Tracer:   trace.New(false, nil),
	})
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/health", nil))

	if recorder.Code != http.StatusOK || reached {
		t.Fatalf("status = %d, upstream reached = %v", recorder.Code, reached)
	}
}

func TestMetadataObservationEnablesCloudPartRedirect(t *testing.T) {
	localRoot := t.TempDir()
	plexRoot := "/media/cloud"
	strmPath := filepath.Join(localRoot, "Movie.strm")
	var mediaVaultRequests int
	mediaVault := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mediaVaultRequests++
		if r.URL.Path != "/redirect/pickcode" {
			t.Fatalf("MediaVault path = %q", r.URL.Path)
		}
		for name, want := range map[string]string{
			"User-Agent":            "Client-Test/1.0",
			"Range":                 "bytes=100-200",
			"X-Plex-Token":          "valid-token",
			"Authorization":         "Bearer client-credential",
			"Cookie":                "session=client-cookie",
			"X-Playback-Session-Id": "playback-session",
		} {
			if got := r.Header.Get(name); got != want {
				t.Errorf("MediaVault %s = %q, want %q", name, got, want)
			}
		}
		w.Header().Set("Location", "https://cdn.invalid/Movie.mkv?signature=private")
		w.WriteHeader(http.StatusFound)
	}))
	defer mediaVault.Close()
	if err := os.WriteFile(strmPath, []byte("http://public.invalid/redirect/pickcode\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var plexPartRequests int
	plex := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/library/metadata/42":
			w.Header().Set("Content-Type", "application/xml")
			_, _ = w.Write([]byte(`<MediaContainer><Video><Media><Part id="123" key="/library/parts/123/7/file" file="/media/cloud/Movie.strm" /></Media></Video></MediaContainer>`))
		case strings.HasPrefix(r.URL.Path, "/library/parts/"):
			plexPartRequests++
			if r.Header.Get("X-Forwarded-For") != "" || r.Header.Get("Forwarded") != "" {
				t.Fatal("client forwarding identity reached Plex authorization probe")
			}
			if r.Header.Get("X-Plex-Token") != "valid-token" {
				http.Error(w, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)
				return
			}
			if r.Header.Get("Range") != "bytes=0-0" {
				t.Fatalf("authorization Range = %q", r.Header.Get("Range"))
			}
			w.Header().Set("Location", "http://public.invalid/redirect/pickcode")
			w.WriteHeader(http.StatusMovedPermanently)
		default:
			http.NotFound(w, r)
		}
	}))
	defer plex.Close()

	handler, registry, cache := newCloudHandler(t, plex.URL, mediaVault.URL, []pathmap.Mapping{{PlexPrefix: plexRoot, LocalPrefix: localRoot}})
	metadata := httptest.NewRecorder()
	handler.ServeHTTP(metadata, httptest.NewRequest(http.MethodGet, "/library/metadata/42", nil))
	if metadata.Code != http.StatusOK {
		t.Fatalf("metadata status = %d", metadata.Code)
	}
	if observed, ok := cache.Get("123"); !ok || observed.RatingKey != "42" {
		t.Fatalf("observed Part = %#v, found = %v", observed, ok)
	}

	playback := httptest.NewRecorder()
	playbackRequest := httptest.NewRequest(http.MethodGet, "/library/parts/123/7/file", nil)
	playbackRequest.Header.Set("X-Plex-Token", "valid-token")
	playbackRequest.Header.Set("User-Agent", "Client-Test/1.0")
	playbackRequest.Header.Set("Range", "bytes=100-200")
	playbackRequest.Header.Set("Authorization", "Bearer client-credential")
	playbackRequest.Header.Set("Cookie", "session=client-cookie")
	playbackRequest.Header.Set("X-Playback-Session-Id", "playback-session")
	playbackRequest.Header.Set("X-Forwarded-For", "198.51.100.10")
	playbackRequest.Header.Set("Forwarded", "for=198.51.100.10")
	handler.ServeHTTP(playback, playbackRequest)
	if playback.Code != http.StatusFound {
		t.Fatalf("playback status = %d, body = %q", playback.Code, playback.Body.String())
	}
	if playback.Header().Get("Location") != "https://cdn.invalid/Movie.mkv?signature=private" {
		t.Fatalf("Location = %q", playback.Header().Get("Location"))
	}
	if plexPartRequests != 1 || mediaVaultRequests != 1 {
		t.Fatalf("Plex Part requests = %d, MediaVault requests = %d", plexPartRequests, mediaVaultRequests)
	}
	snapshot := registry.Snapshot()
	if snapshot.CloudPartHits != 1 || snapshot.RedirectSuccess != 1 || snapshot.PlexRequestsTotal != 1 {
		t.Fatalf("metrics = %#v", snapshot)
	}
}

func TestSuccessfulCloudRedirectEnqueuesCredentialFreePlaybackContext(t *testing.T) {
	localRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(localRoot, "Episode.strm"), []byte("http://public.invalid/redirect/pickcode\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	plex := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", "http://public.invalid/redirect/pickcode")
		w.WriteHeader(http.StatusMovedPermanently)
	}))
	defer plex.Close()
	mediaVault := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", "https://cdn.invalid/Episode.mkv")
		w.WriteHeader(http.StatusFound)
	}))
	defer mediaVault.Close()
	upstream, _ := url.Parse(plex.URL)
	mapper, err := pathmap.New([]pathmap.Mapping{{PlexPrefix: "/media/cloud", LocalPrefix: localRoot}})
	if err != nil {
		t.Fatal(err)
	}
	control, err := resolver.NewMediaVaultSTRMResolver(mediaVault.URL, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	cache := partcache.New(time.Hour)
	cache.Put(partcache.PartInfo{
		PartID: "123", RatingKey: "42", PlexFilePath: "/media/cloud/Episode.strm",
		PartKey: "/library/parts/123/7/file",
	})
	events := make(chan prewarm.PlaybackContext, 1)
	handler := New(Options{
		Upstream: upstream, PartCache: cache, PathMapper: mapper, Resolver: control,
		CloudExtensions: []string{".strm"}, MediaInfoPrewarmer: recordingPrewarmer{events: events},
	})
	request := httptest.NewRequest(http.MethodGet, "/library/parts/123/7/file?playQueueID=5&playQueueItemID=99&X-Plex-Token=client-secret", nil)
	request.Header.Set("User-Agent", "Client-Test/1.0")
	request.Header.Set("X-Plex-Client-Identifier", "client-device")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusFound {
		t.Fatalf("status = %d", response.Code)
	}
	select {
	case event := <-events:
		want := prewarm.PlaybackContext{
			RatingKey: "42", PartID: "123", Target: "http://public.invalid/redirect/pickcode",
			WindowKey:   "X-Plex-Client-Identifier\x00client-device",
			PlayQueueID: "5", PlayQueueItemID: "99", UserAgent: "Client-Test/1.0",
		}
		if event != want {
			t.Fatalf("event = %#v, want %#v", event, want)
		}
	case <-time.After(time.Second):
		t.Fatal("prewarm event was not enqueued")
	}
}

func TestAuthorizedPlexSTRMRedirectEnablesCloudPartRedirect(t *testing.T) {
	localRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(localRoot, "Movie.strm"), []byte("http://public.invalid/redirect/pickcode\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var mediaVaultRequests int
	mediaVault := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mediaVaultRequests++
		w.Header().Set("Location", "https://cdn.invalid/Movie.mkv?signature=private")
		w.WriteHeader(http.StatusFound)
	}))
	defer mediaVault.Close()

	var plexRequests int
	plex := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		plexRequests++
		if r.Header.Get("X-Plex-Token") != "valid-token" {
			http.Error(w, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)
			return
		}
		w.Header().Set("Location", "http://public.invalid/redirect/pickcode")
		w.WriteHeader(http.StatusMovedPermanently)
	}))
	defer plex.Close()

	handler, registry, cache := newCloudHandler(t, plex.URL, mediaVault.URL, []pathmap.Mapping{{PlexPrefix: "/media/cloud", LocalPrefix: localRoot}})
	cache.Put(partcache.PartInfo{PartID: "123", PlexFilePath: "/media/cloud/Movie.strm"})
	request := httptest.NewRequest(http.MethodGet, "/library/parts/123/7/file", nil)
	request.Header.Set("X-Plex-Token", "valid-token")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusFound || recorder.Header().Get("Location") != "https://cdn.invalid/Movie.mkv?signature=private" {
		t.Fatalf("status = %d, Location = %q", recorder.Code, recorder.Header().Get("Location"))
	}
	if plexRequests != 1 || mediaVaultRequests != 1 {
		t.Fatalf("Plex requests = %d, MediaVault requests = %d", plexRequests, mediaVaultRequests)
	}
	if snapshot := registry.Snapshot(); snapshot.RedirectSuccess != 1 || snapshot.PlexFallbackTotal != 0 {
		t.Fatalf("metrics = %#v", snapshot)
	}
}

func TestCloudPartRedirectDoesNotApplyPlaybackVeto(t *testing.T) {
	localRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(localRoot, "Movie.strm"), []byte("http://public.invalid/redirect/pickcode\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	mediaVault := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", "https://cdn.invalid/Movie.mkv?signature=private")
		w.WriteHeader(http.StatusFound)
	}))
	defer mediaVault.Close()
	plex := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", "http://public.invalid/redirect/pickcode")
		w.WriteHeader(http.StatusMovedPermanently)
	}))
	defer plex.Close()

	handler, _, cache := newCloudHandlerWithVeto(t, plex.URL, mediaVault.URL, []pathmap.Mapping{{
		PlexPrefix: "/media/cloud", LocalPrefix: localRoot,
	}}, true)
	cache.Put(partcache.PartInfo{PartID: "123", PlexFilePath: "/media/cloud/Movie.strm"})
	clients := []struct {
		product   string
		platform  string
		userAgent string
	}{
		{product: "Infuse-Library", platform: "iOS", userAgent: "Infuse-Library/8.5.1"},
		{product: "Plex for iOS", platform: "iOS", userAgent: "PlexMobile/8.0"},
		{product: "Plex for Apple TV", platform: "tvOS", userAgent: "PlexTV/8.45"},
	}
	for _, client := range clients {
		request := httptest.NewRequest(http.MethodGet, "/library/parts/123/7/file", nil)
		request.Header.Set("X-Plex-Token", "valid-token")
		request.Header.Set("X-Plex-Product", client.product)
		request.Header.Set("X-Plex-Platform", client.platform)
		request.Header.Set("User-Agent", client.userAgent)
		response := httptest.NewRecorder()

		handler.ServeHTTP(response, request)

		if response.Code != http.StatusFound || response.Header().Get("Location") != "https://cdn.invalid/Movie.mkv?signature=private" {
			t.Fatalf("client %q status=%d Location=%q", client.product, response.Code, response.Header().Get("Location"))
		}
	}
}

func TestCloudPartHeadUsesMediaVaultGet(t *testing.T) {
	localRoot := t.TempDir()
	const controlTarget = "http://public.invalid/redirect/pickcode/Movie.mkv"
	if err := os.WriteFile(filepath.Join(localRoot, "Movie.strm"), []byte(controlTarget+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var mediaVaultRequests int
	mediaVault := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mediaVaultRequests++
		if r.Method != http.MethodGet || r.Header.Get("Range") != "bytes=100-200" {
			t.Fatalf("MediaVault request = %s Range=%q", r.Method, r.Header.Get("Range"))
		}
		w.Header().Set("Location", "https://cdn.invalid/Movie.mkv?signature=private")
		w.WriteHeader(http.StatusFound)
	}))
	defer mediaVault.Close()

	var plexRequests int
	plex := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		plexRequests++
		if r.Method != http.MethodGet || r.Header.Get("Range") != "bytes=0-0" {
			t.Fatalf("Plex authorization = %s Range=%q", r.Method, r.Header.Get("Range"))
		}
		w.Header().Set("Location", controlTarget)
		w.WriteHeader(http.StatusMovedPermanently)
	}))
	defer plex.Close()

	handler, _, cache := newCloudHandler(t, plex.URL, mediaVault.URL, []pathmap.Mapping{{
		PlexPrefix:  "/media/cloud",
		LocalPrefix: localRoot,
	}})
	cache.Put(partcache.PartInfo{PartID: "123", PlexFilePath: "/media/cloud/Movie.strm"})
	request := httptest.NewRequest(http.MethodHead, "/library/parts/123/7/file", nil)
	request.Header.Set("X-Plex-Token", "valid-token")
	request.Header.Set("Range", "bytes=100-200")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusFound || response.Header().Get("Location") != "https://cdn.invalid/Movie.mkv?signature=private" {
		t.Fatalf("status=%d Location=%q", response.Code, response.Header().Get("Location"))
	}
	if response.Body.Len() != 0 || plexRequests != 1 || mediaVaultRequests != 1 {
		t.Fatalf("body=%d Plex=%d MediaVault=%d", response.Body.Len(), plexRequests, mediaVaultRequests)
	}
}

func TestPlexPartRedirectMustMatchSTRMTarget(t *testing.T) {
	for _, test := range []struct {
		name     string
		location string
		status   int
	}{
		{name: "missing location", status: http.StatusFound},
		{name: "different location", location: "http://public.invalid/redirect/other", status: http.StatusFound},
		{name: "successful content response", status: http.StatusPartialContent},
	} {
		t.Run(test.name, func(t *testing.T) {
			localRoot := t.TempDir()
			if err := os.WriteFile(filepath.Join(localRoot, "Movie.strm"), []byte("http://public.invalid/redirect/pickcode\n"), 0o600); err != nil {
				t.Fatal(err)
			}

			var plexRequests, mediaVaultRequests int
			plex := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				plexRequests++
				if r.Header.Get("Range") == "bytes=0-0" {
					if test.location != "" {
						w.Header().Set("Location", test.location)
					}
					w.WriteHeader(test.status)
					return
				}
				w.WriteHeader(http.StatusTeapot)
			}))
			defer plex.Close()
			mediaVault := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				mediaVaultRequests++
				w.Header().Set("Location", "https://cdn.invalid/Movie.mkv")
				w.WriteHeader(http.StatusFound)
			}))
			defer mediaVault.Close()

			handler, _, cache := newCloudHandler(t, plex.URL, mediaVault.URL, []pathmap.Mapping{{
				PlexPrefix:  "/media/cloud",
				LocalPrefix: localRoot,
			}})
			cache.Put(partcache.PartInfo{PartID: "123", PlexFilePath: "/media/cloud/Movie.strm"})
			response := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodGet, "/library/parts/123/7/file", nil)
			request.Header.Set("X-Plex-Token", "valid-token")
			handler.ServeHTTP(response, request)

			if response.Code != http.StatusTeapot || plexRequests != 2 || mediaVaultRequests != 0 {
				t.Fatalf("status=%d Plex=%d MediaVault=%d", response.Code, plexRequests, mediaVaultRequests)
			}
		})
	}
}

func TestControlTargetKeepsEncodedPathDelimitersDistinct(t *testing.T) {
	if sameControlTarget(
		"http://public.invalid/redirect/a%2Fb?source=cloud",
		"http://public.invalid/redirect/a/b?source=cloud",
	) {
		t.Fatal("encoded delimiter matched a path delimiter")
	}
}

func TestColdCacheRedirectFallsBackToPlexWithoutMediaVault(t *testing.T) {
	var mediaVaultRequests int
	mediaVault := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mediaVaultRequests++
		w.Header().Set("Location", "https://cdn.invalid/Movie.mkv?signature=private")
		w.WriteHeader(http.StatusFound)
	}))
	defer mediaVault.Close()

	plex := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Plex-Token") != "valid-token" {
			http.Error(w, http.StatusText(http.StatusForbidden), http.StatusForbidden)
			return
		}
		w.Header().Set("Location", "https://public.invalid/redirect/pickcode")
		w.WriteHeader(http.StatusMovedPermanently)
	}))
	defer plex.Close()

	handler, registry, _ := newCloudHandler(t, plex.URL, mediaVault.URL, []pathmap.Mapping{{PlexPrefix: "/media/cloud", LocalPrefix: t.TempDir()}})
	request := httptest.NewRequest(http.MethodGet, "/library/parts/123/7/file", nil)
	request.Header.Set("X-Plex-Token", "valid-token")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusMovedPermanently || recorder.Header().Get("Location") != "https://public.invalid/redirect/pickcode" {
		t.Fatalf("status = %d, Location = %q", recorder.Code, recorder.Header().Get("Location"))
	}
	if mediaVaultRequests != 0 {
		t.Fatalf("MediaVault requests = %d", mediaVaultRequests)
	}
	if snapshot := registry.Snapshot(); snapshot.CloudPartMisses != 1 || snapshot.RedirectSuccess != 0 || snapshot.PlexFallbackTotal != 1 {
		t.Fatalf("metrics = %#v", snapshot)
	}
}

func TestCloudRedirectRequiresPlexPartAuthorization(t *testing.T) {
	for _, test := range []struct {
		name             string
		token            string
		plexStatus       int
		wantPlexRequests int
		wantFailures     uint64
	}{
		{name: "anonymous", plexStatus: http.StatusUnauthorized, wantPlexRequests: 1},
		{name: "unauthorized library", token: "limited-token", plexStatus: http.StatusForbidden, wantPlexRequests: 2, wantFailures: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			localRoot := t.TempDir()
			if err := os.WriteFile(filepath.Join(localRoot, "Movie.strm"), []byte("http://public.invalid/redirect/pickcode\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			var plexRequests, mediaVaultRequests int
			plex := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				plexRequests++
				http.Error(w, http.StatusText(test.plexStatus), test.plexStatus)
			}))
			defer plex.Close()
			mediaVault := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				mediaVaultRequests++
				w.Header().Set("Location", "https://cdn.invalid/Movie.mkv?signature=private")
				w.WriteHeader(http.StatusFound)
			}))
			defer mediaVault.Close()

			handler, registry, cache := newCloudHandler(t, plex.URL, mediaVault.URL, []pathmap.Mapping{{PlexPrefix: "/media/cloud", LocalPrefix: localRoot}})
			cache.Put(partcache.PartInfo{PartID: "123", PlexFilePath: "/media/cloud/Movie.strm"})
			request := httptest.NewRequest(http.MethodGet, "/library/parts/123/7/file", nil)
			if test.token != "" {
				request.Header.Set("X-Plex-Token", test.token)
			}
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)

			if recorder.Code != test.plexStatus {
				t.Fatalf("status = %d", recorder.Code)
			}
			if recorder.Header().Get("Location") != "" || mediaVaultRequests != 0 {
				t.Fatalf("redirect leaked: Location=%q MediaVault requests=%d", recorder.Header().Get("Location"), mediaVaultRequests)
			}
			if plexRequests != test.wantPlexRequests {
				t.Fatalf("Plex requests = %d, want %d", plexRequests, test.wantPlexRequests)
			}
			snapshot := registry.Snapshot()
			if snapshot.RedirectFailure != test.wantFailures || snapshot.PlexFallbackTotal != 1 {
				t.Fatalf("metrics = %#v", snapshot)
			}
		})
	}
}

func TestCloudRedirectDoesNotInheritPlexTrustedNetworkAccess(t *testing.T) {
	localRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(localRoot, "Movie.strm"), []byte("http://public.invalid/redirect/pickcode\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var probes, fallbacks, mediaVaultRequests int
	plex := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") == "bytes=0-0" {
			probes++
			w.Header().Set("Location", "http://public.invalid/redirect/pickcode")
			w.WriteHeader(http.StatusMovedPermanently)
			return
		}
		fallbacks++
		w.WriteHeader(http.StatusTeapot)
	}))
	defer plex.Close()
	mediaVault := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mediaVaultRequests++
		w.Header().Set("Location", "https://cdn.invalid/Movie.mkv")
		w.WriteHeader(http.StatusFound)
	}))
	defer mediaVault.Close()

	handler, _, cache := newCloudHandler(t, plex.URL, mediaVault.URL, []pathmap.Mapping{{PlexPrefix: "/media/cloud", LocalPrefix: localRoot}})
	cache.Put(partcache.PartInfo{PartID: "123", PlexFilePath: "/media/cloud/Movie.strm"})
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/library/parts/123/7/file", nil))

	if recorder.Code != http.StatusTeapot || probes != 0 || fallbacks != 1 || mediaVaultRequests != 0 {
		t.Fatalf("status=%d probes=%d fallbacks=%d MediaVault=%d", recorder.Code, probes, fallbacks, mediaVaultRequests)
	}
}

func TestCloudPartPreparationFailureStillCountsCacheHit(t *testing.T) {
	for _, test := range []struct {
		name      string
		plexPath  string
		localRoot string
		writeSTRM bool
	}{
		{name: "mapping failure", plexPath: "/other/Movie.strm"},
		{name: "invalid STRM", plexPath: "/media/cloud/Movie.strm", writeSTRM: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			localRoot := t.TempDir()
			if test.writeSTRM {
				if err := os.WriteFile(filepath.Join(localRoot, "Movie.strm"), []byte("ftp://invalid.example/Movie.mkv\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			plex := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusTeapot)
			}))
			defer plex.Close()
			handler, registry, cache := newCloudHandler(t, plex.URL, "http://mediavault.invalid:7811", []pathmap.Mapping{{
				PlexPrefix:  "/media/cloud",
				LocalPrefix: localRoot,
			}})
			cache.Put(partcache.PartInfo{PartID: "123", PlexFilePath: test.plexPath})
			request := httptest.NewRequest(http.MethodGet, "/library/parts/123/7/file", nil)
			request.Header.Set("X-Plex-Token", "valid-token")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusTeapot {
				t.Fatalf("status = %d", response.Code)
			}
			snapshot := registry.Snapshot()
			if snapshot.CloudPartHits != 1 || snapshot.RedirectFailure != 1 || snapshot.PlexFallbackTotal != 1 {
				t.Fatalf("metrics = %#v", snapshot)
			}
		})
	}
}

func TestColdCacheDoesNotProbeMediaBytes(t *testing.T) {
	var plexRequests, mediaVaultRequests int
	plex := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		plexRequests++
		if r.Header.Get("X-Plex-Token") != "valid-token" {
			http.Error(w, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)
			return
		}
		if r.Header.Get("Range") != "bytes=10-20" {
			t.Fatalf("forwarded Range = %q", r.Header.Get("Range"))
		}
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write([]byte("media bytes"))
	}))
	defer plex.Close()
	mediaVault := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mediaVaultRequests++
		if r.URL.Path != "/redirect/pickcode/Movie.mkv" {
			t.Fatalf("MediaVault path = %q", r.URL.Path)
		}
		w.Header().Set("Location", "https://cdn.invalid/Movie.mkv?signature=private")
		w.WriteHeader(http.StatusFound)
	}))
	defer mediaVault.Close()

	handler, registry, _ := newCloudHandler(t, plex.URL, mediaVault.URL, []pathmap.Mapping{{PlexPrefix: "/media/cloud", LocalPrefix: t.TempDir()}})
	request := httptest.NewRequest(http.MethodGet, "/library/parts/777/7/file", nil)
	request.Header.Set("X-Plex-Token", "valid-token")
	request.Header.Set("Range", "bytes=10-20")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusPartialContent || recorder.Header().Get("Location") != "" {
		t.Fatalf("status = %d Location = %q", recorder.Code, recorder.Header().Get("Location"))
	}
	if plexRequests != 1 || mediaVaultRequests != 0 {
		t.Fatalf("Plex requests = %d, MediaVault requests = %d", plexRequests, mediaVaultRequests)
	}
	snapshot := registry.Snapshot()
	if snapshot.CloudPartMisses != 1 || snapshot.RedirectSuccess != 0 || snapshot.PlexFallbackTotal != 1 {
		t.Fatalf("metrics = %#v", snapshot)
	}
}

func TestLocalPartAndCacheMissFallBackToPlex(t *testing.T) {
	var requests int
	plex := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Header.Get("Range") != "bytes=10-20" {
			t.Fatalf("fallback Range = %q", r.Header.Get("Range"))
		}
		w.Header().Set("Content-Range", "bytes 10-20/100")
		w.WriteHeader(http.StatusPartialContent)
	}))
	defer plex.Close()

	handler, registry, cache := newCloudHandler(t, plex.URL, "http://mediavault.invalid:7811", []pathmap.Mapping{{PlexPrefix: "/media/cloud", LocalPrefix: t.TempDir()}})
	request := httptest.NewRequest(http.MethodGet, "/library/parts/999/7/file", nil)
	request.Header.Set("Range", "bytes=10-20")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusPartialContent || requests != 1 {
		t.Fatalf("status = %d, Plex requests = %d", recorder.Code, requests)
	}

	cache.Put(partcache.PartInfo{PartID: "2", PlexFilePath: "/media/local/Local.mkv"})
	localRequest := httptest.NewRequest(http.MethodGet, "/library/parts/2/7/file", nil)
	localRequest.Header.Set("Range", "bytes=10-20")
	localRecorder := httptest.NewRecorder()
	handler.ServeHTTP(localRecorder, localRequest)
	if localRecorder.Code != http.StatusPartialContent || requests != 2 {
		t.Fatalf("local status = %d, Plex requests = %d", localRecorder.Code, requests)
	}
	snapshot := registry.Snapshot()
	if snapshot.CloudPartMisses != 1 || snapshot.PlexFallbackTotal != 1 || snapshot.PlexRequestsTotal != 2 || snapshot.CloudPartHits != 0 {
		t.Fatalf("metrics = %#v", snapshot)
	}
}

func TestResolverFailureFallsBackWithoutLoggingTarget(t *testing.T) {
	var logOutput strings.Builder
	logger := slog.New(slog.NewTextHandler(&logOutput, nil))
	plex := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer plex.Close()
	upstream, _ := url.Parse(plex.URL)
	cache := partcache.New(time.Hour)
	cache.Put(partcache.PartInfo{PartID: "5", PlexFilePath: "/media/cloud/Missing.strm"})
	mapper, err := pathmap.New([]pathmap.Mapping{{PlexPrefix: "/media/cloud", LocalPrefix: t.TempDir()}})
	if err != nil {
		t.Fatal(err)
	}
	strmResolver, err := resolver.NewMediaVaultSTRMResolver("http://mediavault.invalid:7811", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	registry := metrics.New()
	handler := New(Options{
		Upstream:        upstream,
		Logger:          logger,
		Tracer:          trace.New(false, logger),
		PartCache:       cache,
		PathMapper:      mapper,
		Resolver:        strmResolver,
		Metrics:         registry,
		CloudExtensions: []string{".strm"},
	})
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/library/parts/5/7/file", nil)
	request.Header.Set("X-Plex-Token", "valid-token")
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusNoContent {
		t.Fatalf("status = %d", recorder.Code)
	}
	if strings.Contains(logOutput.String(), "Missing.strm") || strings.Contains(logOutput.String(), "mediavault.invalid") {
		t.Fatalf("log leaked resolver target: %s", logOutput.String())
	}
	snapshot := registry.Snapshot()
	if snapshot.RedirectFailure != 1 || snapshot.PlexFallbackTotal != 1 {
		t.Fatalf("metrics = %#v", snapshot)
	}
}

func TestCanceledCloudPartRequestDoesNotFallback(t *testing.T) {
	localRoot := t.TempDir()
	strmPath := filepath.Join(localRoot, "Movie.strm")
	mediaVaultStarted := make(chan struct{})
	var mediaVaultRequests, plexPartRequests atomic.Int64
	mediaVault := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		mediaVaultRequests.Add(1)
		close(mediaVaultStarted)
		<-r.Context().Done()
	}))
	defer mediaVault.Close()
	if err := os.WriteFile(strmPath, []byte("http://public.invalid/redirect/pickcode\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	plex := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/library/metadata/42":
			w.Header().Set("Content-Type", "application/xml")
			_, _ = w.Write([]byte(`<MediaContainer><Video><Media><Part id="123" key="/library/parts/123/7/file" file="/media/cloud/Movie.strm" /></Media></Video></MediaContainer>`))
		case "/library/parts/123/7/file":
			plexPartRequests.Add(1)
			w.Header().Set("Location", "http://public.invalid/redirect/pickcode")
			w.WriteHeader(http.StatusMovedPermanently)
		default:
			http.NotFound(w, r)
		}
	}))
	defer plex.Close()

	handler, registry, _ := newCloudHandler(t, plex.URL, mediaVault.URL, []pathmap.Mapping{{
		PlexPrefix:  "/media/cloud",
		LocalPrefix: localRoot,
	}})
	metadata := httptest.NewRecorder()
	handler.ServeHTTP(metadata, httptest.NewRequest(http.MethodGet, "/library/metadata/42", nil))
	request := httptest.NewRequest(http.MethodGet, "/library/parts/123/7/file", nil)
	request.Header.Set("X-Plex-Token", "valid-token")
	ctx, cancel := context.WithCancel(request.Context())
	request = request.WithContext(ctx)
	response := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		handler.ServeHTTP(response, request)
		close(done)
	}()

	select {
	case <-mediaVaultStarted:
	case <-time.After(time.Second):
		t.Fatal("MediaVault request did not start")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("canceled Part request did not return")
	}

	if mediaVaultRequests.Load() != 1 || plexPartRequests.Load() != 1 {
		t.Fatalf("MediaVault=%d Plex Part=%d", mediaVaultRequests.Load(), plexPartRequests.Load())
	}
	if snapshot := registry.Snapshot(); snapshot.RedirectFailure != 0 ||
		snapshot.PlexFallbackTotal != 0 || snapshot.RedirectSuccess != 0 ||
		snapshot.ResolverLatencySamples != 0 || snapshot.ActiveRequests != 0 {
		t.Fatalf("metrics = %#v", snapshot)
	}
}

func TestCanceledCloudPartAuthorizationDoesNotFallback(t *testing.T) {
	localRoot := t.TempDir()
	strmPath := filepath.Join(localRoot, "Movie.strm")
	if err := os.WriteFile(strmPath, []byte("http://public.invalid/redirect/pickcode\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	partProbeStarted := make(chan struct{})
	var plexPartRequests atomic.Int64
	plex := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/library/parts/123/7/file" {
			t.Errorf("Plex path = %q", r.URL.Path)
			return
		}
		plexPartRequests.Add(1)
		close(partProbeStarted)
		<-r.Context().Done()
	}))
	defer plex.Close()

	handler, registry, cache := newCloudHandler(t, plex.URL, "http://mediavault.invalid:7811", []pathmap.Mapping{{
		PlexPrefix:  "/media/cloud",
		LocalPrefix: localRoot,
	}})
	cache.Put(partcache.PartInfo{
		PartID:       "123",
		PlexFilePath: "/media/cloud/Movie.strm",
		PartKey:      "/library/parts/123/7/file",
	})
	request := httptest.NewRequest(http.MethodGet, "/library/parts/123/7/file", nil)
	request.Header.Set("X-Plex-Token", "valid-token")
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
		t.Fatal("canceled Part authorization did not return")
	}

	if plexPartRequests.Load() != 1 {
		t.Fatalf("Plex Part requests = %d", plexPartRequests.Load())
	}
	if snapshot := registry.Snapshot(); snapshot.RedirectFailure != 0 ||
		snapshot.PlexFallbackTotal != 0 || snapshot.RedirectSuccess != 0 ||
		snapshot.ResolverLatencySamples != 0 || snapshot.ActiveRequests != 0 {
		t.Fatalf("metrics = %#v", snapshot)
	}
}

func TestCanceledAfterCloudPartResolutionDoesNotRedirect(t *testing.T) {
	var plexPartRequests atomic.Int64
	plex := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		plexPartRequests.Add(1)
		w.Header().Set("Location", "http://public.invalid/redirect/pickcode")
		w.WriteHeader(http.StatusMovedPermanently)
	}))
	defer plex.Close()
	upstream, err := url.Parse(plex.URL)
	if err != nil {
		t.Fatal(err)
	}
	mapper, err := pathmap.New([]pathmap.Mapping{{
		PlexPrefix:  "/media/cloud",
		LocalPrefix: t.TempDir(),
	}})
	if err != nil {
		t.Fatal(err)
	}
	cache := partcache.New(time.Hour)
	cache.Put(partcache.PartInfo{
		PartID:       "123",
		PlexFilePath: "/media/cloud/Movie.strm",
		PartKey:      "/library/parts/123/7/file",
	})
	registry := metrics.New()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	request := httptest.NewRequest(http.MethodGet, "/library/parts/123/7/file", nil)
	request.Header.Set("X-Plex-Token", "valid-token")
	ctx, cancel := context.WithCancel(request.Context())
	request = request.WithContext(ctx)
	strmResolver := cancelingResolver{cancel: cancel}
	handler := New(Options{
		Upstream:        upstream,
		Logger:          logger,
		Tracer:          trace.New(false, logger),
		PartCache:       cache,
		PathMapper:      mapper,
		Resolver:        strmResolver,
		Metrics:         registry,
		CloudExtensions: []string{".strm"},
	})
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if location := recorder.Header().Get("Location"); location != "" {
		t.Fatalf("Location = %q", location)
	}
	if plexPartRequests.Load() != 1 {
		t.Fatalf("Plex Part requests = %d", plexPartRequests.Load())
	}
	if snapshot := registry.Snapshot(); snapshot.RedirectFailure != 0 ||
		snapshot.PlexFallbackTotal != 0 || snapshot.RedirectSuccess != 0 ||
		snapshot.ResolverLatencySamples != 0 || snapshot.ActiveRequests != 0 {
		t.Fatalf("metrics = %#v", snapshot)
	}
}

func TestRequestCanceledRequiresMatchingOperationError(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/library/parts/123/7/file", nil)
	ctx, cancel := context.WithCancel(request.Context())
	request = request.WithContext(ctx)
	if requestCanceled(request, context.Canceled) {
		t.Fatal("active request accepted an unrelated cancellation error")
	}
	cancel()
	if !requestCanceled(request, context.Canceled) {
		t.Fatal("matching request cancellation was not recognized")
	}
	if requestCanceled(request, errors.New("upstream failed")) {
		t.Fatal("unrelated upstream error was treated as request cancellation")
	}
}

func TestPlexTransportDoesNotUseEnvironmentProxy(t *testing.T) {
	if newTransport().Proxy != nil {
		t.Fatal("Plex transport inherited an environment proxy")
	}
}

func TestMetricsEndpointIsLocalAndStable(t *testing.T) {
	upstream, _ := url.Parse("http://plex.invalid:32400")
	registry := metrics.New()
	handler := New(Options{Upstream: upstream, Metrics: registry})
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}
	var snapshot metrics.Snapshot
	if err := json.Unmarshal(recorder.Body.Bytes(), &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.ActiveRequests != 1 {
		t.Fatalf("snapshot while request active = %#v", snapshot)
	}
}

func newCloudHandler(t *testing.T, plexURL, mediaVaultURL string, mappings []pathmap.Mapping) (http.Handler, *metrics.Metrics, *partcache.Cache) {
	return newCloudHandlerWithVeto(t, plexURL, mediaVaultURL, mappings, false)
}

func newCloudHandlerWithVeto(t *testing.T, plexURL, mediaVaultURL string, mappings []pathmap.Mapping, playbackVeto bool) (http.Handler, *metrics.Metrics, *partcache.Cache) {
	t.Helper()
	upstream, err := url.Parse(plexURL)
	if err != nil {
		t.Fatal(err)
	}
	mapper, err := pathmap.New(mappings)
	if err != nil {
		t.Fatal(err)
	}
	strmResolver, err := resolver.NewMediaVaultSTRMResolver(mediaVaultURL, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	registry := metrics.New()
	cache := partcache.New(time.Hour)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return New(Options{
		Upstream:        upstream,
		Logger:          logger,
		Tracer:          trace.New(false, logger),
		PartCache:       cache,
		PathMapper:      mapper,
		Resolver:        strmResolver,
		Metrics:         registry,
		CloudExtensions: []string{".strm"},
		PlaybackVeto:    playbackVeto,
	}), registry, cache
}

type cancelingResolver struct {
	cancel context.CancelFunc
}

type recordingPrewarmer struct {
	events chan<- prewarm.PlaybackContext
}

func (prewarmer recordingPrewarmer) TryEnqueue(event prewarm.PlaybackContext) bool {
	prewarmer.events <- event
	return true
}

func (r cancelingResolver) ReadTarget(string) (string, error) {
	return "http://public.invalid/redirect/pickcode", nil
}

func (r cancelingResolver) ResolveTarget(context.Context, string, resolver.PlaybackRequest) (resolver.DirectURL, error) {
	r.cancel()
	return resolver.DirectURL{URL: "https://cdn.invalid/Movie.mkv"}, nil
}
