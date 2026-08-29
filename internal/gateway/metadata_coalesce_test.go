package gateway

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"encoding/xml"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/InfinityPacer/plex-gateway/internal/metrics"
	"github.com/InfinityPacer/plex-gateway/internal/partcache"
	"github.com/InfinityPacer/plex-gateway/internal/trace"
)

type metadataCoalesceObservedRequest struct {
	Method     string
	Path       string
	RawQuery   string
	RemoteAddr string
	Header     http.Header
	Batch      bool
}

type metadataCoalesceTestUpstream struct {
	mu       sync.Mutex
	requests []metadataCoalesceObservedRequest
	serve    func(http.ResponseWriter, *http.Request)
}

func (upstream *metadataCoalesceTestUpstream) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	upstream.mu.Lock()
	upstream.requests = append(upstream.requests, metadataCoalesceObservedRequest{
		Method: request.Method, Path: request.URL.Path, RawQuery: request.URL.RawQuery,
		RemoteAddr: request.RemoteAddr, Header: request.Header.Clone(),
		Batch: isMetadataCoalesceUpstreamRequest(request),
	})
	upstream.mu.Unlock()
	upstream.serve(writer, request)
}

func (upstream *metadataCoalesceTestUpstream) snapshot() []metadataCoalesceObservedRequest {
	upstream.mu.Lock()
	defer upstream.mu.Unlock()
	result := make([]metadataCoalesceObservedRequest, len(upstream.requests))
	copy(result, upstream.requests)
	return result
}

func newMetadataCoalesceTestHandler(
	upstream *metadataCoalesceTestUpstream,
	window, timeout time.Duration,
) (http.Handler, *metrics.Metrics) {
	registry := metrics.New()
	return newMetadataCoalescer(MetadataCoalesceOptions{
		Enabled: true, Window: window, MaxItems: 8, Timeout: timeout,
	}, upstream, registry, slog.New(slog.NewTextHandler(io.Discard, nil))), registry
}

func newMetadataCoalesceTestRequest(method, ratingKey string) *http.Request {
	request := httptest.NewRequest(method, "http://gateway.test/library/metadata/"+ratingKey, nil)
	request.Header.Set("X-Plex-Token", "token-a")
	request.Header.Set("User-Agent", "metadata-client/1")
	request.RemoteAddr = "198.51.100.10:4000"
	return request
}

func runMetadataCoalesceCalls(t *testing.T, handler http.Handler, requests ...*http.Request) []*httptest.ResponseRecorder {
	t.Helper()
	start := make(chan struct{})
	type result struct {
		index    int
		response *httptest.ResponseRecorder
	}
	results := make(chan result, len(requests))
	for index, request := range requests {
		go func(index int, request *http.Request) {
			<-start
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			results <- result{index: index, response: response}
		}(index, request)
	}
	close(start)
	responses := make([]*httptest.ResponseRecorder, len(requests))
	deadline := time.After(2 * time.Second)
	for range requests {
		select {
		case result := <-results:
			responses[result.index] = result.response
		case <-deadline:
			t.Fatal("metadata coalescer request did not complete")
		}
	}
	return responses
}

func runMetadataCoalesceOrderedCalls(t *testing.T, coalescer *metadataCoalescer, handler http.Handler, requests ...*http.Request) []*httptest.ResponseRecorder {
	t.Helper()
	type result struct {
		index    int
		response *httptest.ResponseRecorder
	}
	results := make(chan result, len(requests))
	for index, request := range requests {
		go func(index int, request *http.Request) {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			results <- result{index: index, response: response}
		}(index, request)
		waitForMetadataCoalesceWaiters(t, coalescer, index+1)
	}
	responses := make([]*httptest.ResponseRecorder, len(requests))
	deadline := time.After(2 * time.Second)
	for range requests {
		select {
		case result := <-results:
			responses[result.index] = result.response
		case <-deadline:
			t.Fatal("ordered metadata coalescer request did not complete")
		}
	}
	return responses
}

func metadataCoalesceTestKeys(request *http.Request) []string {
	identifier := strings.TrimPrefix(request.URL.Path, "/library/metadata/")
	return strings.Split(identifier, ",")
}

func metadataCoalesceJSONBody(keys []string) []byte {
	items := make([]string, 0, len(keys))
	for _, key := range keys {
		items = append(items, `{"ratingKey":"`+key+`","title":"title-`+key+`","unknownItemField":"keep"}`)
	}
	return []byte(`{"MediaContainer":{"size":` + strconv.Itoa(len(keys)) + `,"unknownContainerField":{"keep":true},"Metadata":[` + strings.Join(items, ",") + `]}}`)
}

func metadataCoalesceXMLBody(keys []string) []byte {
	items := make([]string, 0, len(keys))
	for _, key := range keys {
		items = append(items, `<Video ratingKey="`+key+`" title="title-`+key+`" unknownItemField="keep"/>`)
	}
	return []byte(`<MediaContainer size="` + strconv.Itoa(len(keys)) + `" unknownContainerField="keep"><Context value="keep"/>` + strings.Join(items, "") + `</MediaContainer>`)
}

func gzipMetadataCoalesceTestBody(body []byte) []byte {
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	_, _ = writer.Write(body)
	_ = writer.Close()
	return compressed.Bytes()
}

func writeMetadataCoalesceTestResponse(
	writer http.ResponseWriter,
	request *http.Request,
	contentType, encoding string,
	status int,
	body []byte,
	headers http.Header,
) {
	for name, values := range headers {
		for _, value := range values {
			writer.Header().Add(name, value)
		}
	}
	wireBody := body
	if encoding == "gzip" {
		wireBody = gzipMetadataCoalesceTestBody(body)
		writer.Header().Set("Content-Encoding", "gzip")
	} else if encoding != "" {
		writer.Header().Set("Content-Encoding", encoding)
	}
	writer.Header().Set("Content-Type", contentType)
	writer.Header().Set("Content-Length", strconv.Itoa(len(wireBody)))
	writer.WriteHeader(status)
	if request.Method != http.MethodHead {
		_, _ = writer.Write(wireBody)
	}
}

func metadataCoalesceResponseBody(t *testing.T, response *httptest.ResponseRecorder) []byte {
	t.Helper()
	body := response.Body.Bytes()
	if response.Header().Get("Content-Encoding") != "gzip" {
		return append([]byte(nil), body...)
	}
	reader, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	decompressed, err := io.ReadAll(reader)
	_ = reader.Close()
	if err != nil {
		t.Fatal(err)
	}
	return decompressed
}

func waitForMetadataCoalesceWaiters(t *testing.T, coalescer *metadataCoalescer, waiters int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		coalescer.mu.Lock()
		for _, group := range coalescer.groups {
			if group.waiters >= waiters {
				coalescer.mu.Unlock()
				return
			}
		}
		coalescer.mu.Unlock()
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("metadata coalescer did not receive %d waiters", waiters)
}

func TestMetadataCoalescerBatchesEquivalentGetsAndFansOutDuplicate(t *testing.T) {
	upstream := &metadataCoalesceTestUpstream{}
	upstream.serve = func(writer http.ResponseWriter, request *http.Request) {
		keys := metadataCoalesceTestKeys(request)
		if isMetadataCoalesceUpstreamRequest(request) {
			for left, right := 0, len(keys)-1; left < right; left, right = left+1, right-1 {
				keys[left], keys[right] = keys[right], keys[left]
			}
		}
		writeMetadataCoalesceTestResponse(writer, request, "application/json", "", http.StatusOK, metadataCoalesceJSONBody(keys), nil)
	}
	handler, registry := newMetadataCoalesceTestHandler(upstream, 30*time.Millisecond, 200*time.Millisecond)
	responses := runMetadataCoalesceCalls(t, handler,
		newMetadataCoalesceTestRequest(http.MethodGet, "1"),
		newMetadataCoalesceTestRequest(http.MethodGet, "2"),
		newMetadataCoalesceTestRequest(http.MethodGet, "1"),
	)

	requests := upstream.snapshot()
	if len(requests) != 1 || !requests[0].Batch {
		t.Fatalf("upstream requests = %#v, want one batch request", requests)
	}
	if got := strings.Split(strings.TrimPrefix(requests[0].Path, "/library/metadata/"), ","); len(got) != 2 || !((got[0] == "1" && got[1] == "2") || (got[0] == "2" && got[1] == "1")) {
		t.Fatalf("batch path = %q, want keys 1 and 2", requests[0].Path)
	}
	if responses[0].Code != http.StatusOK || responses[1].Code != http.StatusOK || responses[2].Code != http.StatusOK {
		t.Fatalf("response statuses = %d, %d, %d", responses[0].Code, responses[1].Code, responses[2].Code)
	}
	if !bytes.Equal(responses[0].Body.Bytes(), responses[2].Body.Bytes()) {
		t.Fatal("duplicate ratingKey callers did not receive identical fan-out bodies")
	}
	for index, key := range []string{"1", "2"} {
		var envelope struct {
			MediaContainer struct {
				Size     int `json:"size"`
				Metadata []struct {
					RatingKey string `json:"ratingKey"`
				} `json:"Metadata"`
			} `json:"MediaContainer"`
		}
		if err := json.Unmarshal(responses[index].Body.Bytes(), &envelope); err != nil {
			t.Fatal(err)
		}
		if envelope.MediaContainer.Size != 1 || len(envelope.MediaContainer.Metadata) != 1 || envelope.MediaContainer.Metadata[0].RatingKey != key {
			t.Fatalf("response[%d] = %#v, want one item %s", index, envelope, key)
		}
	}
	snapshot := registry.Snapshot()
	if snapshot.MetadataCoalesceOfferedTotal != 3 || snapshot.MetadataCoalesceBatchesTotal != 1 || snapshot.MetadataCoalesceItemsTotal != 2 || snapshot.MetadataCoalesceFallbacksTotal != 0 {
		t.Fatalf("coalescer metrics = %#v", snapshot)
	}
}

func TestMetadataCoalescerSupportsJSONXMLIdentityAndGzip(t *testing.T) {
	for _, test := range []struct {
		name        string
		contentType string
		encoding    string
	}{
		{name: "JSON identity", contentType: "application/json", encoding: ""},
		{name: "JSON explicit identity", contentType: "application/json", encoding: "identity"},
		{name: "JSON gzip", contentType: "application/json", encoding: "gzip"},
		{name: "XML identity", contentType: "application/xml", encoding: ""},
		{name: "XML gzip", contentType: "application/xml", encoding: "gzip"},
	} {
		t.Run(test.name, func(t *testing.T) {
			upstream := &metadataCoalesceTestUpstream{}
			upstream.serve = func(writer http.ResponseWriter, request *http.Request) {
				keys := metadataCoalesceTestKeys(request)
				if isMetadataCoalesceUpstreamRequest(request) {
					for left, right := 0, len(keys)-1; left < right; left, right = left+1, right-1 {
						keys[left], keys[right] = keys[right], keys[left]
					}
				}
				body := metadataCoalesceJSONBody(keys)
				if test.contentType == "application/xml" {
					body = metadataCoalesceXMLBody(keys)
				}
				writeMetadataCoalesceTestResponse(writer, request, test.contentType, test.encoding, http.StatusOK, body, http.Header{
					"Cache-Control":  []string{"public, max-age=60"},
					"ETag":           []string{`"batch-etag"`},
					"Last-Modified":  []string{"Wed, 21 Oct 2015 07:28:00 GMT"},
					"Content-MD5":    []string{"stale-md5"},
					"Digest":         []string{"stale-digest"},
					"Content-Digest": []string{"stale-content-digest"},
					"Repr-Digest":    []string{"stale-repr-digest"},
					"Vary":           []string{"Accept"},
				})
			}
			handler, _ := newMetadataCoalesceTestHandler(upstream, 30*time.Millisecond, 200*time.Millisecond)
			responses := runMetadataCoalesceOrderedCalls(t, handler.(*metadataCoalescer), handler,
				newMetadataCoalesceTestRequest(http.MethodGet, "1"),
				newMetadataCoalesceTestRequest(http.MethodGet, "2"),
			)
			if requests := upstream.snapshot(); len(requests) != 1 || !requests[0].Batch {
				t.Fatalf("upstream requests = %#v, want one batch", requests)
			}
			for index, key := range []string{"1", "2"} {
				if responses[index].Code != http.StatusOK {
					t.Fatalf("response[%d] status = %d", index, responses[index].Code)
				}
				body := metadataCoalesceResponseBody(t, responses[index])
				if test.contentType == "application/json" {
					var envelope struct {
						MediaContainer struct {
							Size     int `json:"size"`
							Metadata []struct {
								RatingKey string `json:"ratingKey"`
							} `json:"Metadata"`
						} `json:"MediaContainer"`
					}
					if err := json.Unmarshal(body, &envelope); err != nil {
						t.Fatal(err)
					}
					if envelope.MediaContainer.Size != 1 || len(envelope.MediaContainer.Metadata) != 1 || envelope.MediaContainer.Metadata[0].RatingKey != key {
						t.Fatalf("JSON response[%d] = %#v", index, envelope)
					}
				} else {
					var envelope struct {
						XMLName xml.Name `xml:"MediaContainer"`
						Size    int      `xml:"size,attr"`
						Videos  []struct {
							RatingKey string `xml:"ratingKey,attr"`
						} `xml:"Video"`
					}
					if err := xml.Unmarshal(body, &envelope); err != nil {
						t.Fatal(err)
					}
					if envelope.Size != 1 || len(envelope.Videos) != 1 || envelope.Videos[0].RatingKey != key {
						t.Fatalf("XML response[%d] = %#v", index, envelope)
					}
				}
				if test.encoding != "" && responses[index].Header().Get("Content-Encoding") != test.encoding {
					t.Fatalf("response[%d] Content-Encoding = %q", index, responses[index].Header().Get("Content-Encoding"))
				}
				if test.encoding == "" && responses[index].Header().Get("Content-Encoding") != "" {
					t.Fatalf("response[%d] unexpectedly encoded: %q", index, responses[index].Header().Get("Content-Encoding"))
				}
				if got := responses[index].Header().Get("Content-Length"); got != strconv.Itoa(responses[index].Body.Len()) {
					t.Fatalf("response[%d] Content-Length = %q, body length = %d", index, got, responses[index].Body.Len())
				}
				if got := responses[index].Header().Get("Cache-Control"); got != "private, no-cache" {
					t.Fatalf("response[%d] Cache-Control = %q", index, got)
				}
				if got := responses[index].Header().Get("Vary"); got != "Accept" {
					t.Fatalf("response[%d] Vary = %q", index, got)
				}
				for _, name := range []string{"ETag", "Last-Modified", "Content-MD5", "Digest", "Content-Digest", "Repr-Digest", "Content-Range", "Trailer"} {
					if got := responses[index].Header().Get(name); got != "" {
						t.Fatalf("response[%d] stale %s = %q", index, name, got)
					}
				}
			}
		})
	}
}

func TestMetadataCoalescerDoesNotMergeDifferentRequestIdentity(t *testing.T) {
	variants := []struct {
		name   string
		modify func(*http.Request)
	}{
		{name: "Token", modify: func(request *http.Request) { request.Header.Set("X-Plex-Token", "token-b") }},
		{name: "Cookie", modify: func(request *http.Request) { request.Header.Set("Cookie", "session=b") }},
		{name: "Authorization", modify: func(request *http.Request) { request.Header.Set("Authorization", "Bearer b") }},
		{name: "client header", modify: func(request *http.Request) { request.Header.Set("X-Plex-Client-Identifier", "client-b") }},
		{name: "RawQuery", modify: func(request *http.Request) { request.URL.RawQuery = "includeMarkers=2" }},
		{name: "RemoteAddr", modify: func(request *http.Request) { request.RemoteAddr = "198.51.100.11:4000" }},
	}
	for _, test := range variants {
		t.Run(test.name, func(t *testing.T) {
			upstream := &metadataCoalesceTestUpstream{}
			upstream.serve = func(writer http.ResponseWriter, request *http.Request) {
				writeMetadataCoalesceTestResponse(writer, request, "application/json", "", http.StatusOK, metadataCoalesceJSONBody(metadataCoalesceTestKeys(request)), nil)
			}
			handler, registry := newMetadataCoalesceTestHandler(upstream, 30*time.Millisecond, 200*time.Millisecond)
			requestA := newMetadataCoalesceTestRequest(http.MethodGet, "1")
			requestB := newMetadataCoalesceTestRequest(http.MethodGet, "2")
			test.modify(requestB)
			responses := runMetadataCoalesceCalls(t, handler, requestA, requestB)
			requests := upstream.snapshot()
			if len(requests) != 2 || requests[0].Batch || requests[1].Batch {
				t.Fatalf("upstream requests = %#v, want two direct requests", requests)
			}
			for index, response := range responses {
				if response.Code != http.StatusOK {
					t.Fatalf("response[%d] status = %d", index, response.Code)
				}
			}
			if got := registry.Snapshot().MetadataCoalesceBatchesTotal; got != 0 {
				t.Fatalf("batch metric = %d, want 0", got)
			}
		})
	}
}

func TestMetadataCoalescerBypassesNonCoalescibleRequests(t *testing.T) {
	variants := []struct {
		name   string
		modify func(*http.Request)
	}{
		{name: "HEAD", modify: func(request *http.Request) {}},
		{name: "Range", modify: func(request *http.Request) { request.Header.Set("Range", "bytes=0-1") }},
		{name: "conditional", modify: func(request *http.Request) { request.Header.Set("If-None-Match", `"etag"`) }},
		{name: "no token", modify: func(request *http.Request) { request.Header.Del("X-Plex-Token") }},
		{name: "conflicting token", modify: func(request *http.Request) {
			request.URL.RawQuery = "X-Plex-Token=query-token"
			request.Header.Set("X-Plex-Token", "header-token")
		}},
		{name: "request body", modify: func(request *http.Request) {
			request.Body = io.NopCloser(strings.NewReader("body"))
			request.ContentLength = 4
		}},
	}
	for _, test := range variants {
		t.Run(test.name, func(t *testing.T) {
			upstream := &metadataCoalesceTestUpstream{}
			upstream.serve = func(writer http.ResponseWriter, request *http.Request) {
				writeMetadataCoalesceTestResponse(writer, request, "application/json", "", http.StatusOK, metadataCoalesceJSONBody(metadataCoalesceTestKeys(request)), nil)
			}
			handler, registry := newMetadataCoalesceTestHandler(upstream, 30*time.Millisecond, 200*time.Millisecond)
			requestA := newMetadataCoalesceTestRequest(http.MethodGet, "1")
			requestB := newMetadataCoalesceTestRequest(http.MethodGet, "2")
			if test.name == "HEAD" {
				requestA.Method = http.MethodHead
				requestB.Method = http.MethodHead
			}
			test.modify(requestA)
			test.modify(requestB)
			responses := runMetadataCoalesceCalls(t, handler, requestA, requestB)
			requests := upstream.snapshot()
			if len(requests) != 2 || requests[0].Batch || requests[1].Batch {
				t.Fatalf("upstream requests = %#v, want two direct requests", requests)
			}
			for index, response := range responses {
				if response.Code != http.StatusOK {
					t.Fatalf("response[%d] status = %d", index, response.Code)
				}
				if requestA.Method == http.MethodHead && response.Body.Len() != 0 {
					t.Fatalf("HEAD response[%d] body length = %d", index, response.Body.Len())
				}
			}
			if got := registry.Snapshot().MetadataCoalesceOfferedTotal; got != 0 {
				t.Fatalf("offered metric = %d, want 0", got)
			}
		})
	}
}

func TestMetadataCoalescerBatchMissingOrErrorFallsBack(t *testing.T) {
	variants := []struct {
		name  string
		serve func(http.ResponseWriter, *http.Request)
	}{
		{
			name: "missing item",
			serve: func(writer http.ResponseWriter, request *http.Request) {
				if isMetadataCoalesceUpstreamRequest(request) {
					writeMetadataCoalesceTestResponse(writer, request, "application/json", "", http.StatusOK, metadataCoalesceJSONBody([]string{"1"}), nil)
					return
				}
				writeMetadataCoalesceTestResponse(writer, request, "application/json", "", http.StatusOK, metadataCoalesceJSONBody(metadataCoalesceTestKeys(request)), nil)
			},
		},
		{
			name: "upstream error",
			serve: func(writer http.ResponseWriter, request *http.Request) {
				if isMetadataCoalesceUpstreamRequest(request) {
					writeMetadataCoalesceTestResponse(writer, request, "application/json", "", http.StatusBadGateway, []byte(`{"error":"upstream"}`), nil)
					return
				}
				writeMetadataCoalesceTestResponse(writer, request, "application/json", "", http.StatusOK, metadataCoalesceJSONBody(metadataCoalesceTestKeys(request)), nil)
			},
		},
		{
			name: "malformed response",
			serve: func(writer http.ResponseWriter, request *http.Request) {
				if isMetadataCoalesceUpstreamRequest(request) {
					writeMetadataCoalesceTestResponse(writer, request, "application/json", "", http.StatusOK, []byte("{"), nil)
					return
				}
				writeMetadataCoalesceTestResponse(writer, request, "application/json", "", http.StatusOK, metadataCoalesceJSONBody(metadataCoalesceTestKeys(request)), nil)
			},
		},
		{
			name: "unsupported encoding",
			serve: func(writer http.ResponseWriter, request *http.Request) {
				if isMetadataCoalesceUpstreamRequest(request) {
					writeMetadataCoalesceTestResponse(writer, request, "application/json", "br", http.StatusOK, metadataCoalesceJSONBody(metadataCoalesceTestKeys(request)), nil)
					return
				}
				writeMetadataCoalesceTestResponse(writer, request, "application/json", "", http.StatusOK, metadataCoalesceJSONBody(metadataCoalesceTestKeys(request)), nil)
			},
		},
	}
	for _, test := range variants {
		t.Run(test.name, func(t *testing.T) {
			upstream := &metadataCoalesceTestUpstream{serve: test.serve}
			handler, registry := newMetadataCoalesceTestHandler(upstream, 30*time.Millisecond, 200*time.Millisecond)
			responses := runMetadataCoalesceOrderedCalls(t, handler.(*metadataCoalescer), handler,
				newMetadataCoalesceTestRequest(http.MethodGet, "1"),
				newMetadataCoalesceTestRequest(http.MethodGet, "2"),
			)
			requests := upstream.snapshot()
			if len(requests) != 3 || !requests[0].Batch || requests[1].Batch || requests[2].Batch {
				t.Fatalf("upstream requests = %#v, want batch then direct fallbacks", requests)
			}
			if !(requests[1].Path == "/library/metadata/1" && requests[2].Path == "/library/metadata/2" ||
				requests[1].Path == "/library/metadata/2" && requests[2].Path == "/library/metadata/1") {
				t.Fatalf("fallback paths = %q, %q", requests[1].Path, requests[2].Path)
			}
			for index, key := range []string{"1", "2"} {
				if responses[index].Code != http.StatusOK || !bytes.Contains(responses[index].Body.Bytes(), []byte(`"ratingKey":"`+key+`"`)) {
					t.Fatalf("fallback response[%d] status=%d body=%s", index, responses[index].Code, responses[index].Body.Bytes())
				}
			}
			if got := registry.Snapshot().MetadataCoalesceFallbacksTotal; got != 1 {
				t.Fatalf("fallback metric = %d, want 1", got)
			}
		})
	}
}

func TestMetadataCoalescerFailedBatchFallsBackOncePerDuplicateKey(t *testing.T) {
	upstream := &metadataCoalesceTestUpstream{}
	upstream.serve = func(writer http.ResponseWriter, request *http.Request) {
		if isMetadataCoalesceUpstreamRequest(request) {
			writeMetadataCoalesceTestResponse(writer, request, "application/json", "", http.StatusBadGateway, nil, nil)
			return
		}
		writeMetadataCoalesceTestResponse(writer, request, "application/json", "", http.StatusOK,
			metadataCoalesceJSONBody(metadataCoalesceTestKeys(request)), nil)
	}
	handler, registry := newMetadataCoalesceTestHandler(upstream, 30*time.Millisecond, 200*time.Millisecond)
	responses := runMetadataCoalesceOrderedCalls(t, handler.(*metadataCoalescer), handler,
		newMetadataCoalesceTestRequest(http.MethodGet, "1"),
		newMetadataCoalesceTestRequest(http.MethodGet, "1"),
		newMetadataCoalesceTestRequest(http.MethodGet, "2"),
	)
	requests := upstream.snapshot()
	if len(requests) != 3 || !requests[0].Batch {
		t.Fatalf("upstream requests = %#v, want one batch and one direct request per unique key", requests)
	}
	counts := map[string]int{}
	for _, request := range requests[1:] {
		if request.Batch {
			t.Fatalf("unexpected second batch: %#v", request)
		}
		counts[request.Path]++
	}
	if counts["/library/metadata/1"] != 1 || counts["/library/metadata/2"] != 1 {
		t.Fatalf("direct fallback counts = %#v", counts)
	}
	for index, response := range responses {
		if response.Code != http.StatusOK {
			t.Fatalf("response[%d] status = %d", index, response.Code)
		}
	}
	if !bytes.Equal(responses[0].Body.Bytes(), responses[1].Body.Bytes()) {
		t.Fatal("duplicate waiters did not receive the same fallback response")
	}
	if got := registry.Snapshot().MetadataCoalesceFallbacksTotal; got != 1 {
		t.Fatalf("fallback metric = %d, want 1", got)
	}
}

func TestMetadataCoalescerRecoversAbortFromDetachedBatch(t *testing.T) {
	upstream := &metadataCoalesceTestUpstream{}
	upstream.serve = func(writer http.ResponseWriter, request *http.Request) {
		if isMetadataCoalesceUpstreamRequest(request) {
			panic(http.ErrAbortHandler)
		}
		writeMetadataCoalesceTestResponse(writer, request, "application/json", "", http.StatusOK,
			metadataCoalesceJSONBody(metadataCoalesceTestKeys(request)), nil)
	}
	handler, _ := newMetadataCoalesceTestHandler(upstream, 30*time.Millisecond, 200*time.Millisecond)
	responses := runMetadataCoalesceOrderedCalls(t, handler.(*metadataCoalescer), handler,
		newMetadataCoalesceTestRequest(http.MethodGet, "1"),
		newMetadataCoalesceTestRequest(http.MethodGet, "2"),
	)
	for index, response := range responses {
		if response.Code != http.StatusOK {
			t.Fatalf("response[%d] status = %d", index, response.Code)
		}
	}
	requests := upstream.snapshot()
	if len(requests) != 3 || !requests[0].Batch {
		t.Fatalf("upstream requests = %#v, want aborted batch then two direct fallbacks", requests)
	}
}

func TestMetadataCoalescerFallsBackAfterReverseProxyTruncatedBatch(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		mu.Lock()
		paths = append(paths, request.URL.Path)
		mu.Unlock()
		if strings.Contains(strings.TrimPrefix(request.URL.Path, "/library/metadata/"), ",") {
			writer.Header().Set("Content-Type", "application/json")
			writer.Header().Set("Content-Length", "1024")
			_, _ = writer.Write([]byte(`{"MediaContainer":`))
			return
		}
		writeMetadataCoalesceTestResponse(writer, request, "application/json", "", http.StatusOK,
			metadataCoalesceJSONBody(metadataCoalesceTestKeys(request)), nil)
	}))
	defer upstream.Close()
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	registry := metrics.New()
	proxy := httputil.NewSingleHostReverseProxy(upstreamURL)
	handler := newMetadataCoalescer(MetadataCoalesceOptions{
		Enabled: true, Window: 30 * time.Millisecond, MaxItems: 8, Timeout: 200 * time.Millisecond,
	}, proxy, registry, slog.New(slog.NewTextHandler(io.Discard, nil)))
	responses := runMetadataCoalesceOrderedCalls(t, handler.(*metadataCoalescer), handler,
		newMetadataCoalesceTestRequest(http.MethodGet, "1"),
		newMetadataCoalesceTestRequest(http.MethodGet, "2"),
	)
	for index, response := range responses {
		if response.Code != http.StatusOK {
			t.Fatalf("response[%d] status = %d", index, response.Code)
		}
	}
	mu.Lock()
	seen := append([]string(nil), paths...)
	mu.Unlock()
	if len(seen) != 3 || !strings.Contains(seen[0], ",") {
		t.Fatalf("upstream paths = %#v, want truncated batch then two direct fallbacks", seen)
	}
}

func TestMetadataCoalescerHandlesSinglePartialAndAllCancellation(t *testing.T) {
	tests := []struct {
		name       string
		requestNum int
		cancel     func([]context.CancelFunc)
		wantDirect int
	}{
		{
			name:       "single cancellation",
			requestNum: 1,
			cancel: func(cancels []context.CancelFunc) {
				cancels[0]()
			},
			wantDirect: 0,
		},
		{
			name:       "partial cancellation",
			requestNum: 2,
			cancel: func(cancels []context.CancelFunc) {
				cancels[0]()
			},
			wantDirect: 1,
		},
		{
			name:       "all cancellation",
			requestNum: 2,
			cancel: func(cancels []context.CancelFunc) {
				for _, cancel := range cancels {
					cancel()
				}
			},
			wantDirect: 0,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			upstream := &metadataCoalesceTestUpstream{}
			upstream.serve = func(writer http.ResponseWriter, request *http.Request) {
				writeMetadataCoalesceTestResponse(writer, request, "application/json", "", http.StatusOK, metadataCoalesceJSONBody(metadataCoalesceTestKeys(request)), nil)
			}
			handler, _ := newMetadataCoalesceTestHandler(upstream, 40*time.Millisecond, 200*time.Millisecond)
			requests := make([]*http.Request, test.requestNum)
			cancels := make([]context.CancelFunc, test.requestNum)
			for index := range requests {
				ctx, cancel := context.WithCancel(context.Background())
				cancels[index] = cancel
				requests[index] = newMetadataCoalesceTestRequest(http.MethodGet, strconv.Itoa(index+1))
				requests[index] = requests[index].WithContext(ctx)
			}
			start := make(chan struct{})
			var waitGroup sync.WaitGroup
			for _, request := range requests {
				waitGroup.Add(1)
				go func(request *http.Request) {
					defer waitGroup.Done()
					<-start
					handler.ServeHTTP(httptest.NewRecorder(), request)
				}(request)
			}
			close(start)
			waitForMetadataCoalesceWaiters(t, handler.(*metadataCoalescer), test.requestNum)
			test.cancel(cancels)
			finished := make(chan struct{})
			go func() {
				waitGroup.Wait()
				close(finished)
			}()
			select {
			case <-finished:
			case <-time.After(time.Second):
				t.Fatal("cancelled metadata coalescer request did not complete")
			}
			requestsSeen := upstream.snapshot()
			if len(requestsSeen) != test.wantDirect {
				t.Fatalf("upstream requests = %#v, want %d direct requests", requestsSeen, test.wantDirect)
			}
			for _, request := range requestsSeen {
				if request.Batch {
					t.Fatalf("cancelled request reached batch upstream: %#v", request)
				}
			}
		})
	}
}

func TestMetadataCoalescerBatchTimeoutFallsBack(t *testing.T) {
	batchFinished := make(chan struct{})
	upstream := &metadataCoalesceTestUpstream{}
	upstream.serve = func(writer http.ResponseWriter, request *http.Request) {
		if isMetadataCoalesceUpstreamRequest(request) {
			<-request.Context().Done()
			close(batchFinished)
			return
		}
		writeMetadataCoalesceTestResponse(writer, request, "application/json", "", http.StatusOK, metadataCoalesceJSONBody(metadataCoalesceTestKeys(request)), nil)
	}
	handler, registry := newMetadataCoalesceTestHandler(upstream, 5*time.Millisecond, 25*time.Millisecond)
	responses := runMetadataCoalesceOrderedCalls(t, handler.(*metadataCoalescer), handler,
		newMetadataCoalesceTestRequest(http.MethodGet, "1"),
		newMetadataCoalesceTestRequest(http.MethodGet, "2"),
	)
	select {
	case <-batchFinished:
	case <-time.After(time.Second):
		t.Fatal("batch upstream did not observe coalescer timeout")
	}
	requests := upstream.snapshot()
	if len(requests) != 3 || !requests[0].Batch || requests[1].Batch || requests[2].Batch {
		t.Fatalf("upstream requests = %#v, want timed-out batch then direct fallbacks", requests)
	}
	if !(requests[1].Path == "/library/metadata/1" && requests[2].Path == "/library/metadata/2" ||
		requests[1].Path == "/library/metadata/2" && requests[2].Path == "/library/metadata/1") {
		t.Fatalf("fallback paths = %q, %q", requests[1].Path, requests[2].Path)
	}
	for index, response := range responses {
		if response.Code != http.StatusOK {
			t.Fatalf("response[%d] status = %d", index, response.Code)
		}
	}
	if got := registry.Snapshot().MetadataCoalesceFallbacksTotal; got != 1 {
		t.Fatalf("fallback metric = %d, want 1", got)
	}
}

func TestMetadataCoalescerCancelsDispatchedBatchAfterAllWaitersLeave(t *testing.T) {
	batchStarted := make(chan struct{})
	batchCanceled := make(chan struct{})
	unexpectedDirect := make(chan struct{}, 1)
	upstream := &metadataCoalesceTestUpstream{}
	upstream.serve = func(_ http.ResponseWriter, request *http.Request) {
		if !isMetadataCoalesceUpstreamRequest(request) {
			select {
			case unexpectedDirect <- struct{}{}:
			default:
			}
			return
		}
		close(batchStarted)
		<-request.Context().Done()
		close(batchCanceled)
	}
	handler, _ := newMetadataCoalesceTestHandler(upstream, 3*time.Millisecond, time.Second)
	requests := make([]*http.Request, 2)
	cancels := make([]context.CancelFunc, 2)
	for index := range requests {
		ctx, cancel := context.WithCancel(context.Background())
		cancels[index] = cancel
		requests[index] = newMetadataCoalesceTestRequest(http.MethodGet, strconv.Itoa(index+1)).WithContext(ctx)
	}
	finished := make(chan struct{}, 2)
	for _, request := range requests {
		go func(request *http.Request) {
			handler.ServeHTTP(httptest.NewRecorder(), request)
			finished <- struct{}{}
		}(request)
	}
	select {
	case <-batchStarted:
	case <-time.After(time.Second):
		t.Fatal("batch request did not start")
	}
	for _, cancel := range cancels {
		cancel()
	}
	for range requests {
		select {
		case <-finished:
		case <-time.After(time.Second):
			t.Fatal("canceled waiter did not return")
		}
	}
	select {
	case <-batchCanceled:
	case <-time.After(time.Second):
		t.Fatal("upstream batch was not canceled after every waiter left")
	}
	seen := upstream.snapshot()
	if len(seen) != 1 || !seen[0].Batch {
		t.Fatalf("upstream requests = %#v, want one canceled batch", seen)
	}
	select {
	case <-unexpectedDirect:
		t.Fatal("unexpected direct fallback after every waiter canceled")
	default:
	}
}

func TestMetadataCoalescerKeepsDispatchedBatchForRemainingWaiter(t *testing.T) {
	batchStarted := make(chan struct{})
	releaseBatch := make(chan struct{})
	upstream := &metadataCoalesceTestUpstream{}
	upstream.serve = func(writer http.ResponseWriter, request *http.Request) {
		if isMetadataCoalesceUpstreamRequest(request) {
			close(batchStarted)
			select {
			case <-releaseBatch:
			case <-request.Context().Done():
				return
			}
		}
		writeMetadataCoalesceTestResponse(writer, request, "application/json", "", http.StatusOK,
			metadataCoalesceJSONBody(metadataCoalesceTestKeys(request)), nil)
	}
	handler, _ := newMetadataCoalesceTestHandler(upstream, 3*time.Millisecond, time.Second)
	contexts := make([]context.CancelFunc, 2)
	requests := make([]*http.Request, 2)
	for index := range requests {
		ctx, cancel := context.WithCancel(context.Background())
		contexts[index] = cancel
		requests[index] = newMetadataCoalesceTestRequest(http.MethodGet, strconv.Itoa(index+1)).WithContext(ctx)
	}
	responses := make(chan *httptest.ResponseRecorder, 2)
	for _, request := range requests {
		go func(request *http.Request) {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			responses <- response
		}(request)
	}
	select {
	case <-batchStarted:
	case <-time.After(time.Second):
		t.Fatal("batch request did not start")
	}
	contexts[0]()
	time.Sleep(10 * time.Millisecond)
	close(releaseBatch)
	var successful int
	for range requests {
		select {
		case response := <-responses:
			if response.Code == http.StatusOK && response.Body.Len() != 0 {
				successful++
			}
		case <-time.After(time.Second):
			t.Fatal("metadata waiter did not complete")
		}
	}
	if successful != 1 {
		t.Fatalf("successful responses = %d, want one remaining waiter", successful)
	}
	seen := upstream.snapshot()
	if len(seen) != 1 || !seen[0].Batch {
		t.Fatalf("upstream requests = %#v, want one completed batch", seen)
	}
}

func TestMetadataCoalescerResponseLimitFallsBackInOrder(t *testing.T) {
	upstream := &metadataCoalesceTestUpstream{}
	upstream.serve = func(writer http.ResponseWriter, request *http.Request) {
		writeMetadataCoalesceTestResponse(writer, request, "application/json", "", http.StatusOK,
			metadataCoalesceJSONBody(metadataCoalesceTestKeys(request)), nil)
	}
	handler, registry := newMetadataCoalesceTestHandler(upstream, 30*time.Millisecond, time.Second)
	coalescer := handler.(*metadataCoalescer)
	coalescer.bodyLimit = 64
	responses := runMetadataCoalesceOrderedCalls(t, coalescer, handler,
		newMetadataCoalesceTestRequest(http.MethodGet, "1"),
		newMetadataCoalesceTestRequest(http.MethodGet, "2"),
	)
	seen := upstream.snapshot()
	if len(seen) != 3 || !seen[0].Batch || seen[1].Batch || seen[2].Batch {
		t.Fatalf("upstream requests = %#v, want oversized batch then two direct fallbacks", seen)
	}
	for index, response := range responses {
		if response.Code != http.StatusOK {
			t.Fatalf("response[%d] status = %d", index, response.Code)
		}
	}
	if got := registry.Snapshot().MetadataCoalesceFallbacksTotal; got != 1 {
		t.Fatalf("fallback metric = %d, want 1", got)
	}
}

func TestGatewayMetadataCoalescerUsesBatchGuardAndObservesEveryPart(t *testing.T) {
	var mu sync.Mutex
	var upstreamPaths []string
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		mu.Lock()
		upstreamPaths = append(upstreamPaths, request.URL.Path)
		mu.Unlock()
		keys := metadataCoalesceTestKeys(request)
		items := make([]string, 0, len(keys))
		for _, key := range keys {
			items = append(items, `{"ratingKey":"`+key+`","Media":[{"Part":[{"id":"part-`+key+`","key":"/library/parts/part-`+key+`/stamp/file.strm","file":"/media/cloud/`+key+`.strm"}]}]}`)
		}
		writeMetadataCoalesceTestResponse(writer, request, "application/json", "", http.StatusOK,
			[]byte(`{"MediaContainer":{"size":`+strconv.Itoa(len(keys))+`,"Metadata":[`+strings.Join(items, ",")+`]}}`), nil)
	}))
	defer upstream.Close()
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	registry := metrics.New()
	parts := partcache.New(time.Hour)
	handler := New(Options{
		Upstream: upstreamURL, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Tracer: trace.New(false, nil), Metrics: registry, PartCache: parts,
		ObserveMaxBytes: 8 << 20,
		MetadataGuard: MetadataGuardOptions{
			Enabled: true, GlobalConcurrency: 8, PerClientConcurrency: 4,
			BatchEnabled: true, BatchConcurrency: 1, QueueTimeout: time.Second,
		},
		MetadataCoalesce: MetadataCoalesceOptions{
			Enabled: true, Window: 30 * time.Millisecond, MaxItems: 32, Timeout: time.Second,
		},
	})
	requests := make([]*http.Request, 32)
	for index := range requests {
		requests[index] = newMetadataCoalesceTestRequest(http.MethodGet, strconv.Itoa(index+1))
		requests[index].Header.Set("X-Plex-Client-Identifier", "apple-tv-client")
	}
	responses := runMetadataCoalesceCalls(t, handler, requests...)
	for index, response := range responses {
		if response.Code != http.StatusOK {
			t.Fatalf("response[%d] status = %d", index, response.Code)
		}
	}
	mu.Lock()
	paths := append([]string(nil), upstreamPaths...)
	mu.Unlock()
	if len(paths) != 1 || len(strings.Split(strings.TrimPrefix(paths[0], "/library/metadata/"), ",")) != 32 {
		t.Fatalf("upstream paths = %#v, want one 32-item batch", paths)
	}
	for index := 1; index <= 32; index++ {
		key := strconv.Itoa(index)
		part, found := parts.Get("part-" + key)
		if !found || part.RatingKey != key || part.PlexFilePath != "/media/cloud/"+key+".strm" {
			t.Fatalf("cached part %s = %#v, found=%v", key, part, found)
		}
	}
	snapshot := registry.Snapshot()
	if snapshot.PlexRequestsTotal != 1 || snapshot.MetadataCoalesceBatchesTotal != 1 || snapshot.MetadataCoalesceItemsTotal != 32 ||
		snapshot.MetadataBatchGuardAdmittedTotal != 1 || snapshot.MetadataGuardAdmittedTotal != 0 {
		t.Fatalf("gateway coalesce metrics = %#v", snapshot)
	}
}

func BenchmarkMetadataCoalescerBurst32(b *testing.B) {
	newUpstream := func() http.Handler {
		return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			writeMetadataCoalesceTestResponse(writer, request, "application/json", "", http.StatusOK,
				metadataCoalesceJSONBody(metadataCoalesceTestKeys(request)), nil)
		})
	}
	benchmarks := []struct {
		name    string
		handler func() http.Handler
	}{
		{name: "direct", handler: newUpstream},
		{name: "coalesced", handler: func() http.Handler {
			return newMetadataCoalescer(MetadataCoalesceOptions{
				Enabled: true, Window: 3 * time.Millisecond, MaxItems: 32, Timeout: time.Second,
			}, newUpstream(), metrics.New(), slog.New(slog.NewTextHandler(io.Discard, nil)))
		}},
	}
	for _, benchmark := range benchmarks {
		b.Run(benchmark.name, func(b *testing.B) {
			handler := benchmark.handler()
			b.ReportAllocs()
			for b.Loop() {
				var waitGroup sync.WaitGroup
				waitGroup.Add(32)
				for index := 1; index <= 32; index++ {
					request := newMetadataCoalesceTestRequest(http.MethodGet, strconv.Itoa(index))
					go func(request *http.Request) {
						defer waitGroup.Done()
						handler.ServeHTTP(httptest.NewRecorder(), request)
					}(request)
				}
				waitGroup.Wait()
			}
		})
	}
}
