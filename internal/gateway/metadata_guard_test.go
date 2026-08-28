package gateway

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/InfinityPacer/plex-gateway/internal/metrics"
)

func TestMetadataGuardLimitsOneClientWithoutReservingGlobalCapacity(t *testing.T) {
	started := make(chan string, 4)
	unblock := make(chan struct{})
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started <- r.Header.Get("X-Plex-Client-Identifier")
		<-unblock
		w.WriteHeader(http.StatusNoContent)
	})
	registry := metrics.New()
	handler := newMetadataGuard(MetadataGuardOptions{
		Enabled:              true,
		GlobalConcurrency:    3,
		PerClientConcurrency: 2,
		QueueTimeout:         time.Second,
	}, next, registry, slog.New(slog.NewTextHandler(io.Discard, nil)))

	results := make(chan int, 4)
	startRequest := func(client string) {
		go func() {
			request := httptest.NewRequest(http.MethodGet, "/library/metadata/42", nil)
			request.Header.Set("X-Plex-Client-Identifier", client)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			results <- response.Code
		}()
	}

	startRequest("apple-tv")
	startRequest("apple-tv")
	startRequest("apple-tv")
	for range 2 {
		if client := receiveString(t, started); client != "apple-tv" {
			t.Fatalf("started client = %q, want apple-tv", client)
		}
	}
	select {
	case client := <-started:
		t.Fatalf("third request for same client started early: %q", client)
	case <-time.After(30 * time.Millisecond):
	}

	startRequest("infuse")
	if client := receiveString(t, started); client != "infuse" {
		t.Fatalf("started client = %q, want infuse", client)
	}
	snapshot := registry.Snapshot()
	if snapshot.MetadataGuardActive != 3 || snapshot.MetadataGuardQueued != 1 {
		t.Fatalf("guard snapshot while blocked = %#v", snapshot)
	}

	close(unblock)
	for range 4 {
		select {
		case status := <-results:
			if status != http.StatusNoContent {
				t.Fatalf("status = %d, want %d", status, http.StatusNoContent)
			}
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for guarded request")
		}
	}
	snapshot = registry.Snapshot()
	if snapshot.MetadataGuardAdmittedTotal != 4 || snapshot.MetadataGuardActive != 0 || snapshot.MetadataGuardQueued != 0 {
		t.Fatalf("final guard snapshot = %#v", snapshot)
	}
}

func TestMetadataBatchGuardUsesIndependentPool(t *testing.T) {
	started := make(chan string, 3)
	unblock := make(chan struct{})
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started <- r.URL.Path
		<-unblock
		w.WriteHeader(http.StatusNoContent)
	})
	registry := metrics.New()
	handler := newMetadataGuard(MetadataGuardOptions{
		Enabled:              true,
		GlobalConcurrency:    1,
		PerClientConcurrency: 1,
		BatchEnabled:         true,
		BatchConcurrency:     1,
		QueueTimeout:         time.Second,
	}, next, registry, slog.New(slog.NewTextHandler(io.Discard, nil)))

	results := make(chan int, 3)
	startRequest := func(path string) {
		go func() {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
			results <- response.Code
		}()
	}

	startRequest("/library/metadata/42")
	startRequest("/library/metadata/42,43")
	for range 2 {
		receiveString(t, started)
	}
	startRequest("/library/metadata/44,45")
	select {
	case path := <-started:
		t.Fatalf("second batch request started early: %q", path)
	case <-time.After(30 * time.Millisecond):
	}

	snapshot := registry.Snapshot()
	if snapshot.MetadataGuardActive != 1 || snapshot.MetadataBatchGuardActive != 1 || snapshot.MetadataBatchGuardQueued != 1 {
		t.Fatalf("guard snapshot while blocked = %#v", snapshot)
	}

	close(unblock)
	for range 3 {
		select {
		case status := <-results:
			if status != http.StatusNoContent {
				t.Fatalf("status = %d, want %d", status, http.StatusNoContent)
			}
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for guarded request")
		}
	}
	snapshot = registry.Snapshot()
	if snapshot.MetadataBatchGuardAdmittedTotal != 2 || snapshot.MetadataBatchGuardActive != 0 || snapshot.MetadataBatchGuardQueued != 0 {
		t.Fatalf("final batch guard snapshot = %#v", snapshot)
	}
}

func TestMetadataGuardRejectsAfterQueueTimeout(t *testing.T) {
	started := make(chan struct{}, 1)
	unblock := make(chan struct{})
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		started <- struct{}{}
		<-unblock
		w.WriteHeader(http.StatusNoContent)
	})
	registry := metrics.New()
	handler := newMetadataGuard(MetadataGuardOptions{
		Enabled:              true,
		GlobalConcurrency:    1,
		PerClientConcurrency: 1,
		QueueTimeout:         30 * time.Millisecond,
	}, next, registry, slog.New(slog.NewTextHandler(io.Discard, nil)))

	firstDone := make(chan struct{})
	go func() {
		request := httptest.NewRequest(http.MethodGet, "/library/metadata/42", nil)
		request.Header.Set("X-Plex-Client-Identifier", "apple-tv")
		handler.ServeHTTP(httptest.NewRecorder(), request)
		close(firstDone)
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("first request did not enter downstream handler")
	}

	request := httptest.NewRequest(http.MethodGet, "/library/metadata/43", nil)
	request.Header.Set("X-Plex-Client-Identifier", "apple-tv")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusTooManyRequests)
	}
	if got := response.Header().Get("Retry-After"); got != "1" {
		t.Fatalf("Retry-After = %q, want 1", got)
	}
	snapshot := registry.Snapshot()
	if snapshot.MetadataGuardTimeoutsTotal != 1 || snapshot.MetadataGuardActive != 1 || snapshot.MetadataGuardQueued != 0 {
		t.Fatalf("timeout snapshot = %#v", snapshot)
	}

	close(unblock)
	select {
	case <-firstDone:
	case <-time.After(time.Second):
		t.Fatal("first request did not complete")
	}
}

func TestMetadataBatchGuardRejectsAfterQueueTimeout(t *testing.T) {
	started := make(chan struct{}, 1)
	unblock := make(chan struct{})
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		started <- struct{}{}
		<-unblock
		w.WriteHeader(http.StatusNoContent)
	})
	registry := metrics.New()
	handler := newMetadataGuard(MetadataGuardOptions{
		BatchEnabled:     true,
		BatchConcurrency: 1,
		QueueTimeout:     30 * time.Millisecond,
	}, next, registry, slog.New(slog.NewTextHandler(io.Discard, nil)))

	firstDone := make(chan struct{})
	go func() {
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/library/metadata/42,43", nil))
		close(firstDone)
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("first batch request did not enter downstream handler")
	}

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/library/metadata/44,45", nil))
	if response.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusTooManyRequests)
	}
	snapshot := registry.Snapshot()
	if snapshot.MetadataBatchGuardTimeoutsTotal != 1 || snapshot.MetadataBatchGuardActive != 1 || snapshot.MetadataBatchGuardQueued != 0 {
		t.Fatalf("batch timeout snapshot = %#v", snapshot)
	}

	close(unblock)
	select {
	case <-firstDone:
	case <-time.After(time.Second):
		t.Fatal("first batch request did not complete")
	}
}

func TestMetadataGuardClassifiesSupportedReads(t *testing.T) {
	tests := []struct {
		method string
		path   string
		want   metadataRequestKind
	}{
		{method: http.MethodGet, path: "/library/metadata/42", want: metadataRequestSingle},
		{method: http.MethodHead, path: "/library/metadata/42?includeMarkers=1", want: metadataRequestSingle},
		{method: http.MethodGet, path: "/library/metadata/42,43", want: metadataRequestBatch},
		{method: http.MethodHead, path: "/library/metadata/42,43,44?includeMarkers=1", want: metadataRequestBatch},
		{method: http.MethodGet, path: "/library/metadata/42/children", want: metadataRequestNone},
		{method: http.MethodGet, path: "/library/metadata/42,", want: metadataRequestNone},
		{method: http.MethodGet, path: "/library/metadata/42,abc", want: metadataRequestNone},
		{method: http.MethodGet, path: "/library/metadata/", want: metadataRequestNone},
		{method: http.MethodPost, path: "/library/metadata/42,43", want: metadataRequestNone},
		{method: http.MethodGet, path: "/library/parts/42/1/file", want: metadataRequestNone},
		{method: http.MethodGet, path: "/:/timeline", want: metadataRequestNone},
	}
	for _, test := range tests {
		t.Run(test.method+" "+test.path, func(t *testing.T) {
			request := httptest.NewRequest(test.method, test.path, nil)
			if got := classifyMetadataRequest(request); got != test.want {
				t.Fatalf("classifyMetadataRequest() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestDisabledMetadataGuardDoesNotLimitRequests(t *testing.T) {
	var calls int
	var mu sync.Mutex
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	handler := newMetadataGuard(MetadataGuardOptions{}, next, metrics.New(), nil)

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/library/metadata/42", nil))
	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusNoContent)
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Fatalf("calls = %d, want 1", calls)
	}
}

func receiveString(t *testing.T, values <-chan string) string {
	t.Helper()
	select {
	case value := <-values:
		return value
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for request to start")
		return ""
	}
}
