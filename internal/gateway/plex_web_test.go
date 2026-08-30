package gateway

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

func TestPlexWebCompatibilityInjectsBeforePlexScripts(t *testing.T) {
	var upstreamEncoding string
	var upstreamValidator string
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		upstreamEncoding = request.Header.Get("Accept-Encoding")
		upstreamValidator = request.Header.Get("If-None-Match")
		body := `<html><head><title>Plex</title></head><body><script src="/web/main.js"></script></body></html>`
		writer.Header().Set("Content-Type", "text/html")
		writer.Header().Set("ETag", `"plex-shell"`)
		writer.Header().Set("Accept-Ranges", "bytes")
		writer.Header().Set("Content-Range", "bytes 0-10/100")
		writer.Header().Set("Trailer", "Digest")
		writer.Header().Set("Content-Length", strconv.Itoa(len(body)))
		_, _ = io.WriteString(writer, body)
	}))
	defer upstream.Close()

	request := httptest.NewRequest(http.MethodGet, "http://gateway/web/index.html", nil)
	request.Header.Set("Accept-Encoding", "gzip, br")
	request.Header.Set("If-None-Match", `"plex-shell"`)
	response := httptest.NewRecorder()
	New(Options{Upstream: mustParseGatewayTestURL(t, upstream.URL), PlexWebDirectPlay: true}).ServeHTTP(response, request)

	if upstreamEncoding != "identity" || upstreamValidator != "" {
		t.Fatalf("upstream headers: Accept-Encoding=%q If-None-Match=%q", upstreamEncoding, upstreamValidator)
	}
	body := response.Body.String()
	tag := string(plexWebScriptTag)
	if strings.Count(body, tag) != 1 || strings.Index(body, tag) > strings.Index(body, `</head>`) {
		t.Fatalf("compatibility script was not injected before </head>: %q", body)
	}
	if response.Header().Get("X-Plex-Gateway-Web-Compat") != "direct-play-v2" ||
		response.Header().Get("Content-Length") != strconv.Itoa(len(body)) {
		t.Fatalf("unexpected response headers: %#v", response.Header())
	}
	for _, name := range []string{"Accept-Ranges", "Content-Range", "ETag", "Last-Modified", "Trailer"} {
		if value := response.Header().Get(name); value != "" {
			t.Fatalf("%s = %q", name, value)
		}
	}
}

func TestPlexWebCompatibilityScriptIsVersionedAndScopedToPartMedia(t *testing.T) {
	upstream := httptest.NewServer(http.NotFoundHandler())
	defer upstream.Close()
	handler := New(Options{Upstream: mustParseGatewayTestURL(t, upstream.URL), PlexWebDirectPlay: true})

	for _, method := range []string{http.MethodGet, http.MethodHead} {
		request := httptest.NewRequest(method, "http://gateway"+plexWebScriptPath, nil)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)

		if response.Code != http.StatusOK || response.Header().Get("Content-Type") != "text/javascript; charset=utf-8" ||
			!strings.Contains(response.Header().Get("Cache-Control"), "immutable") {
			t.Fatalf("%s response: status=%d headers=%#v", method, response.Code, response.Header())
		}
		if method == http.MethodHead && response.Body.Len() != 0 {
			t.Fatalf("HEAD body length = %d", response.Body.Len())
		}
	}

	script := string(plexWebScript)
	if !strings.HasSuffix(plexWebScriptPath, "web-direct-play-v2.js") ||
		!strings.Contains(script, `const marker = "__plexGatewayDirectPlayV2"`) {
		t.Fatalf("helper cache identity was not advanced with its behavior: path=%q", plexWebScriptPath)
	}
	for _, contract := range []string{`path.startsWith("/library/parts/")`, `path.endsWith("/file")`, `path.endsWith(".strm")`, `target.origin === window.location.origin`, `const preservedCrossOrigin = new WeakMap()`, `preservedCrossOrigin.set(this, value == null ? null : String(value))`, `restorePartCrossOrigin(this)`, `removeAttribute.call(element, "crossorigin")`} {
		if !strings.Contains(script, contract) {
			t.Fatalf("script is missing contract %q", contract)
		}
	}
	if !strings.Contains(script, `if (!suppressPartCrossOrigin(this, value)) {
          restorePartCrossOrigin(this);
        }
        source.set.call(this, value);`) {
		t.Fatal("helper does not restore the preserved CORS mode before assigning a non-cloud source")
	}
	if strings.Contains(script, "elementPrototype.setAttribute") || strings.Contains(script, "elementPrototype.setAttributeNS") {
		t.Fatal("script changes the global Element attribute methods")
	}
}

func TestPlexWebCompatibilityPreservesNoStore(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "http://gateway/web/index.html", nil)
	response := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/html"}, "Cache-Control": []string{"private, no-store"}},
		Body:       io.NopCloser(strings.NewReader(`<html><head></head><body></body></html>`)),
		Request:    request,
	}

	if err := newPlexWebCompatibility(true, nil).modifyResponse(response); err != nil {
		t.Fatal(err)
	}
	if got := response.Header.Get("Cache-Control"); got != "private, no-store" {
		t.Fatalf("Cache-Control = %q", got)
	}
}

func TestPlexWebCompatibilityPreservesUpstreamCacheRestrictions(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "private policy", in: "private, max-age=600", want: "private, max-age=600, no-cache"},
		{name: "existing no cache", in: "private, no-cache", want: "private, no-cache"},
		{name: "empty policy", want: "no-cache"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "http://gateway/web/index.html", nil)
			response := &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/html"}},
				Body:       io.NopCloser(strings.NewReader(`<html><head></head><body></body></html>`)),
				Request:    request,
			}
			if test.in != "" {
				response.Header.Set("Cache-Control", test.in)
			}

			if err := newPlexWebCompatibility(true, nil).modifyResponse(response); err != nil {
				t.Fatal(err)
			}
			if got := response.Header.Get("Cache-Control"); got != test.want {
				t.Fatalf("Cache-Control = %q, want %q", got, test.want)
			}
		})
	}
}

func TestPlexWebCompatibilityDisabledIsTransparent(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/html")
		writer.WriteHeader(http.StatusTeapot)
		_, _ = io.WriteString(writer, request.URL.Path)
	}))
	defer upstream.Close()
	handler := New(Options{Upstream: mustParseGatewayTestURL(t, upstream.URL)})

	for _, path := range []string{"/web/index.html", plexWebScriptPath} {
		request := httptest.NewRequest(http.MethodGet, "http://gateway"+path, nil)
		request.Header.Set("Accept-Encoding", "br")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusTeapot || response.Body.String() != path || response.Header().Get("X-Plex-Gateway-Web-Compat") != "" {
			t.Fatalf("%s was not transparent: status=%d body=%q headers=%#v", path, response.Code, response.Body.String(), response.Header())
		}
	}
}

func TestPlexWebCompatibilityEnabledLeavesNonShellTrafficTransparent(t *testing.T) {
	compatibility := newPlexWebCompatibility(true, nil)
	paths := []string{
		"/",
		"/library/metadata/1",
		"/library/parts/123/456/file",
		"/library/parts/123/456/file.mp4",
		"/video/:/transcode/universal/decision",
		"/video/:/transcode/universal/start.mpd",
	}

	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			in := httptest.NewRequest(http.MethodGet, "http://gateway"+path, nil)
			in.Header.Set("Accept-Encoding", "br")
			in.Header.Set("If-None-Match", `"client-validator"`)
			out := in.Clone(in.Context())
			compatibility.prepareProxyRequest(&httputil.ProxyRequest{In: in, Out: out})
			if got := out.Header.Get("Accept-Encoding"); got != "br" {
				t.Fatalf("Accept-Encoding = %q", got)
			}
			if got := out.Header.Get("If-None-Match"); got != `"client-validator"` {
				t.Fatalf("If-None-Match = %q", got)
			}

			body := "upstream:" + path
			response := &http.Response{
				StatusCode: http.StatusPartialContent,
				Header: http.Header{
					"Cache-Control": []string{"private, max-age=600"},
					"Content-Range": []string{"bytes 0-9/100"},
					"Content-Type":  []string{"application/octet-stream"},
					"Etag":          []string{`"upstream-validator"`},
				},
				Body:    io.NopCloser(strings.NewReader(body)),
				Request: in,
			}
			if err := compatibility.modifyResponse(response); err != nil {
				t.Fatal(err)
			}
			gotBody, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatal(err)
			}
			if response.StatusCode != http.StatusPartialContent || string(gotBody) != body ||
				response.Header.Get("Cache-Control") != "private, max-age=600" ||
				response.Header.Get("Content-Range") != "bytes 0-9/100" ||
				response.Header.Get("ETag") != `"upstream-validator"` ||
				response.Header.Get("X-Plex-Gateway-Web-Compat") != "" {
				t.Fatalf("non-shell response changed: status=%d headers=%#v body=%q", response.StatusCode, response.Header, gotBody)
			}
		})
	}
}

func TestPlexWebCompatibilityFailsOpenForUnsupportedRepresentation(t *testing.T) {
	original := `<html><head></head><body>` + strings.Repeat("x", 32) + `</body></html>`
	request := httptest.NewRequest(http.MethodGet, "http://gateway/web/index.html", nil)

	tests := []struct {
		name        string
		encoding    string
		bodyLimit   int64
		contentType string
	}{
		{name: "unsupported encoding", encoding: "br", bodyLimit: plexWebBodyMaxBytes, contentType: "text/html"},
		{name: "oversized body", bodyLimit: 16, contentType: "text/html"},
		{name: "non HTML", bodyLimit: plexWebBodyMaxBytes, contentType: "application/json"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{test.contentType}, "Content-Encoding": []string{test.encoding}},
				Body:       io.NopCloser(strings.NewReader(original)),
				Request:    request,
			}
			compatibility := newPlexWebCompatibility(true, nil)
			compatibility.maxBodyBytes = test.bodyLimit
			if err := compatibility.modifyResponse(response); err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatal(err)
			}
			if string(body) != original || bytesContain(body, plexWebScriptTag) {
				t.Fatalf("response changed: %q", body)
			}
		})
	}
}

func mustParseGatewayTestURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

func bytesContain(body, fragment []byte) bool {
	return strings.Contains(string(body), string(fragment))
}

func BenchmarkPlexWebCompatibility(b *testing.B) {
	compatibility := newPlexWebCompatibility(true, nil)
	webRequest := httptest.NewRequest(http.MethodGet, "http://gateway/web/index.html", nil)
	apiRequest := httptest.NewRequest(http.MethodGet, "http://gateway/library/metadata/1", nil)
	webBody := `<html><head><title>Plex</title></head><body>` + strings.Repeat("x", 30<<10) + `</body></html>`

	b.Run("non web response", func(b *testing.B) {
		response := &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Request: apiRequest}
		b.ReportAllocs()
		for b.Loop() {
			if err := compatibility.modifyResponse(response); err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("30 KiB web shell", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			response := &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/html"}},
				Body:       io.NopCloser(strings.NewReader(webBody)),
				Request:    webRequest,
			}
			if err := compatibility.modifyResponse(response); err != nil {
				b.Fatal(err)
			}
			_ = response.Body.Close()
		}
	})
}
