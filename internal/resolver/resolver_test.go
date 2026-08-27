package resolver

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestDefaultClientDoesNotUseEnvironmentProxy(t *testing.T) {
	resolver, err := NewMediaVaultSTRMResolver("http://mediavault.invalid:7811", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	transport, ok := resolver.client.Transport.(*http.Transport)
	if !ok || transport.Proxy != nil {
		t.Fatal("MediaVault transport inherited an environment proxy")
	}
}

func TestResolveMediaVaultFormats(t *testing.T) {
	t.Parallel()
	var requests atomic.Int32
	var requestChecks []func(*testing.T, *http.Request)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		index := int(requests.Add(1)) - 1
		if r.URL.Path != "/redirect" && !strings.HasPrefix(r.URL.Path, "/redirect/") {
			t.Errorf("request path = %q", r.URL.Path)
		}
		if index >= 0 && index < len(requestChecks) {
			requestChecks[index](t, r)
		}
		w.Header().Set("Location", "https://cdn.invalid/video.mkv?signature=secret")
		w.WriteHeader(http.StatusFound)
	}))
	defer server.Close()

	resolver := newResolver(t, server.URL, nil, 0)
	tests := []struct {
		name   string
		target string
		check  func(*testing.T, *http.Request)
	}{
		{
			name:   "query",
			target: "http://untrusted.invalid/redirect?path=%2Fmedia%2Fcloud%2Fmovie.mkv&pickcode=pick-query",
			check: func(t *testing.T, request *http.Request) {
				if request.URL.Path != "/redirect" || request.URL.Query().Get("pickcode") != "pick-query" {
					t.Fatalf("query request = %s", request.URL.RequestURI())
				}
			},
		},
		{
			name:   "pickcode",
			target: "https://untrusted.invalid/redirect/pick-path",
			check: func(t *testing.T, request *http.Request) {
				if request.URL.Path != "/redirect/pick-path" || request.URL.RawQuery != "" {
					t.Fatalf("pickcode request = %s", request.URL.RequestURI())
				}
			},
		},
		{
			name:   "filename",
			target: "http://untrusted.invalid/redirect/pick-file/episode.mkv",
			check: func(t *testing.T, request *http.Request) {
				if request.URL.Path != "/redirect/pick-file/episode.mkv" {
					t.Fatalf("filename request = %s", request.URL.RequestURI())
				}
			},
		},
		{
			name:   "share",
			target: "http://untrusted.invalid/redirect?path=%2Fshare%2Fmovie&fid=42&source=share%3Ashare-code",
			check: func(t *testing.T, request *http.Request) {
				query := request.URL.Query()
				if query.Get("fid") != "42" || query.Get("source") != "share:share-code" {
					t.Fatalf("share request = %s", request.URL.RequestURI())
				}
			},
		},
	}
	for _, test := range tests {
		requestChecks = append(requestChecks, test.check)
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := writeSTRM(t, test.target)
			got, err := resolveSTRM(resolver, context.Background(), path, PlaybackRequest{})
			if err != nil {
				t.Fatal(err)
			}
			if got.URL != "https://cdn.invalid/video.mkv?signature=secret" {
				t.Fatalf("direct URL = %q", got.URL)
			}
		})
	}

	if requests.Load() != int32(len(tests)) {
		t.Fatalf("MediaVault requests = %d, want %d", requests.Load(), len(tests))
	}
}

func TestResolvePassesPlaybackRequestThrough(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodHead {
			t.Fatalf("method = %q", request.Method)
		}
		for name, want := range map[string]string{
			"User-Agent":            "Client-Test/1.0",
			"Range":                 "bytes=100-200",
			"Accept":                "video/*",
			"X-Plex-Token":          "plex-token",
			"Authorization":         "Bearer client-credential",
			"Cookie":                "session=client-cookie",
			"X-Playback-Session-Id": "playback-session",
		} {
			if got := request.Header.Get(name); got != want {
				t.Errorf("%s = %q, want %q", name, got, want)
			}
		}
		w.Header().Set("Location", "https://cdn.invalid/video.mkv?signature=secret")
		w.WriteHeader(http.StatusFound)
	}))
	defer server.Close()

	playback := PlaybackRequest{
		Method: http.MethodHead,
		Header: http.Header{
			"User-Agent":            {"Client-Test/1.0"},
			"Range":                 {"bytes=100-200"},
			"Accept":                {"video/*"},
			"X-Plex-Token":          {"plex-token"},
			"Authorization":         {"Bearer client-credential"},
			"Cookie":                {"session=client-cookie"},
			"X-Playback-Session-Id": {"playback-session"},
		},
	}
	resolver := newResolver(t, server.URL, nil, 0)
	directURL, err := resolveSTRM(resolver, context.Background(), writeSTRM(t, server.URL+"/redirect/pick"), playback)
	if err != nil {
		t.Fatal(err)
	}
	if directURL.String() != "https://cdn.invalid/video.mkv?signature=secret" {
		t.Fatalf("direct URL = %q", directURL.String())
	}
}

func TestResolveMediaVaultInternalRedirectIsBoundedAndNoFollow(t *testing.T) {
	var externalRequests atomic.Int32
	external := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		externalRequests.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer external.Close()

	var mediaVaultRequests atomic.Int32
	mediaVault := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") != "Client-Test/1.0" || r.Header.Get("X-Plex-Token") != "plex-token" {
			t.Fatalf("redirect hop headers were not preserved")
		}
		if mediaVaultRequests.Add(1) == 1 {
			w.Header().Set("Location", "/redirect?path=%2Fmovie&pickcode=pick&realplay=true")
		} else {
			w.Header().Set("Location", external.URL+"/final?signature=secret")
		}
		w.WriteHeader(http.StatusFound)
	}))
	defer mediaVault.Close()

	resolver := newResolver(t, mediaVault.URL, nil, 0)
	got, err := resolveSTRM(resolver,
		context.Background(),
		writeSTRM(t, "https://untrusted.invalid/redirect/pick"),
		PlaybackRequest{Header: http.Header{"User-Agent": {"Client-Test/1.0"}, "X-Plex-Token": {"plex-token"}}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if got.URL != external.URL+"/final?signature=secret" {
		t.Fatalf("direct URL = %q", got.URL)
	}
	if mediaVaultRequests.Load() != 2 {
		t.Fatalf("MediaVault requests = %d, want 2", mediaVaultRequests.Load())
	}
	if externalRequests.Load() != 0 {
		t.Fatalf("external CDN requests = %d, want 0", externalRequests.Load())
	}
}

func TestResolveRewritesAdvertisedMediaVaultOrigin(t *testing.T) {
	var requests atomic.Int32
	mediaVault := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if requests.Add(1) == 1 {
			w.Header().Set("Location", "https://public-mediavault.invalid:8443/redirect/pick?realplay=true")
		} else {
			w.Header().Set("Location", "https://cdn.invalid/final.mkv?signature=secret")
		}
		w.WriteHeader(http.StatusFound)
	}))
	defer mediaVault.Close()

	resolver := newResolver(t, mediaVault.URL, nil, 0)
	got, err := resolveSTRM(resolver, context.Background(), writeSTRM(t, "https://public-mediavault.invalid/redirect/pick"), PlaybackRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if got.URL != "https://cdn.invalid/final.mkv?signature=secret" || requests.Load() != 2 {
		t.Fatalf("direct URL = %q, MediaVault requests = %d", got.URL, requests.Load())
	}
}

func TestResolveThirdPartyDirectURLDoesNotMakeOutboundRequest(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	resolver := newResolver(t, "http://mediavault.invalid:7811", nil, 0)
	target := server.URL + "/openlist/file.mkv?signature=secret"
	got, err := resolveSTRM(resolver, context.Background(), writeSTRM(t, target), PlaybackRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if got.URL != target {
		t.Fatalf("direct URL = %q, want %q", got.URL, target)
	}
	if requests.Load() != 0 {
		t.Fatalf("third-party requests = %d, want 0", requests.Load())
	}
}

func TestResolveLocalAbsolutePathUsesMediaVaultRedirect(t *testing.T) {
	var gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Query().Get("path")
		w.Header().Set("Location", "https://cdn.invalid/local.mkv?signature=secret")
		w.WriteHeader(http.StatusFound)
	}))
	defer server.Close()

	resolver := newResolver(t, server.URL, nil, 0)
	localPath := "/media/local/movies/Season 01/episode.mkv"
	got, err := resolveSTRM(resolver, context.Background(), writeSTRM(t, "\n  \r\n"+localPath+"\nignored"), PlaybackRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if got.URL != "https://cdn.invalid/local.mkv?signature=secret" {
		t.Fatalf("direct URL = %q", got.URL)
	}
	if gotPath != localPath {
		t.Fatalf("MediaVault path = %q, want %q", gotPath, localPath)
	}
}

func TestResolveErrorsAreSanitized(t *testing.T) {
	const secret = "signature=super-secret"
	tests := []struct {
		name       string
		handler    http.Handler
		client     *http.Client
		target     string
		maxLine    int
		wantIs     error
		wantSecret string
	}{
		{
			name: "status 200",
			handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(secret))
			}),
			target: "http://untrusted.invalid/redirect?path=%2Fmovie&" + secret,
		},
		{
			name: "missing location",
			handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusFound)
			}),
			target: "http://untrusted.invalid/redirect/pick?" + secret,
		},
		{
			name: "invalid location scheme",
			handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Location", "ftp://cdn.invalid/file?"+secret)
				w.WriteHeader(http.StatusFound)
			}),
			target: "http://untrusted.invalid/redirect/pick",
		},
		{
			name: "timeout",
			handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				<-r.Context().Done()
			}),
			client: &http.Client{Timeout: 20 * time.Millisecond},
			target: "http://untrusted.invalid/redirect/pick",
			wantIs: context.DeadlineExceeded,
		},
		{
			name:       "oversized line",
			handler:    http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("server should not be reached") }),
			target:     "http://untrusted.invalid/redirect?" + strings.Repeat("x", 64),
			maxLine:    16,
			wantSecret: "x",
		},
		{
			name:       "unsupported input scheme",
			handler:    http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("server should not be reached") }),
			target:     "ftp://cdn.invalid/file?" + secret,
			wantSecret: secret,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(test.handler)
			defer server.Close()
			base := server.URL
			if test.name == "oversized line" || test.name == "unsupported input scheme" {
				base = "http://mediavault.invalid:7811"
			}
			resolver := newResolver(t, base, test.client, test.maxLine)
			_, err := resolveSTRM(resolver, context.Background(), writeSTRM(t, test.target), PlaybackRequest{})
			if err == nil {
				t.Fatal("Resolve succeeded, want error")
			}
			if test.wantIs != nil && !errors.Is(err, test.wantIs) {
				t.Fatalf("error = %v, errors.Is(%v) = false", err, test.wantIs)
			}
			for _, value := range []string{secret, test.wantSecret} {
				if value != "" && strings.Contains(err.Error(), value) {
					t.Fatalf("error contains sensitive value %q: %v", value, err)
				}
			}
		})
	}
}

func TestResolvePropagatesCallerCancellationWithoutLeakingTarget(t *testing.T) {
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
	}))
	defer server.Close()
	resolver := newResolver(t, server.URL, nil, 0)
	const secret = "signature=private"
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		directURL, err := resolver.ResolveTarget(ctx, server.URL+"/redirect/pick?"+secret, PlaybackRequest{})
		if directURL.String() != "" {
			result <- fmt.Errorf("direct URL = %q", directURL.String())
			return
		}
		result <- err
	}()

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("MediaVault request did not start")
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, errors.Is(context.Canceled) = false", err)
		}
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("error leaked target query: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled resolver did not return")
	}
}

func TestResolveRejectsRedirectPathTraversalAndLoop(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", "/redirect?loop=true")
		w.WriteHeader(http.StatusFound)
	}))
	defer server.Close()

	resolver := newResolver(t, server.URL, nil, 0)
	if _, err := resolveSTRM(resolver, context.Background(), writeSTRM(t, server.URL+"/redirect/../admin"), PlaybackRequest{}); err == nil {
		t.Fatal("path traversal was accepted")
	}
	if _, err := resolveSTRM(resolver, context.Background(), writeSTRM(t, server.URL+"/redirect"), PlaybackRequest{}); !errors.Is(err, errRedirectLoop) {
		t.Fatalf("loop error = %v", err)
	}
}

func TestNewMediaVaultSTRMResolverValidatesOrigin(t *testing.T) {
	for _, raw := range []string{
		"",
		"mediavault:7811",
		"ftp://mediavault:7811",
		"http://user:pass@mediavault:7811",
		"http://mediavault:7811/base",
		"http://mediavault:7811/?token=secret",
	} {
		if _, err := NewMediaVaultSTRMResolver(raw, nil, 0); err == nil {
			t.Errorf("NewMediaVaultSTRMResolver(%q) succeeded", raw)
		}
	}
}

func newResolver(t *testing.T, baseURL string, client *http.Client, maxLineBytes int) *MediaVaultSTRMResolver {
	t.Helper()
	resolver, err := NewMediaVaultSTRMResolver(baseURL, client, maxLineBytes)
	if err != nil {
		t.Fatal(err)
	}
	return resolver
}

func resolveSTRM(resolver *MediaVaultSTRMResolver, ctx context.Context, path string, playback PlaybackRequest) (DirectURL, error) {
	target, err := resolver.ReadTarget(path)
	if err != nil {
		return DirectURL{}, err
	}
	return resolver.ResolveTarget(ctx, target, playback)
}

func writeSTRM(t *testing.T, target string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sample.strm")
	if err := os.WriteFile(path, []byte(target), 0o600); err != nil {
		t.Fatal(fmt.Errorf("write test STRM: %w", err))
	}
	return path
}
