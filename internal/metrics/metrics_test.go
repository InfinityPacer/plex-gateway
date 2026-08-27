package metrics

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
	"time"
)

func TestSnapshotAndJSONHandler(t *testing.T) {
	registry := New()
	registry.IncPlexRequests()
	registry.IncCloudPartHits()
	registry.IncCloudPartMisses()
	registry.IncRedirectSuccess()
	registry.IncRedirectFailure()
	registry.IncPlexFallback()
	registry.ObserveResolverLatency(12 * time.Millisecond)
	registry.ObserveResolverLatency(7 * time.Millisecond)
	registry.ObserveRedirectLatency(30 * time.Millisecond)
	release := registry.BeginRequest()

	writer := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	registry.Handler().ServeHTTP(writer, request)

	if writer.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", writer.Code, http.StatusOK)
	}
	if got := writer.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Fatalf("content type = %q", got)
	}
	var got Snapshot
	if err := json.Unmarshal(writer.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode JSON: %v", err)
	}
	want := Snapshot{
		PlexRequestsTotal: 1,
		CloudPartHits:     1,
		CloudPartMisses:   1,
		RedirectSuccess:   1,
		RedirectFailure:   1,
		PlexFallbackTotal: 1,
		ActiveRequests:    1,

		ResolverLatencyMSTotal: 19,
		ResolverLatencySamples: 2,
		ResolverLatencyMSLast:  7,
		ResolverLatencyMSMax:   12,

		RedirectLatencyMSTotal: 30,
		RedirectLatencySamples: 1,
		RedirectLatencyMSLast:  30,
		RedirectLatencyMSMax:   30,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("snapshot = %#v, want %#v", got, want)
	}
	release()
	if got := registry.Snapshot().ActiveRequests; got != 0 {
		t.Fatalf("active requests after release = %d, want 0", got)
	}
}

func TestHandlerAllowsOnlyGET(t *testing.T) {
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		t.Run(method, func(t *testing.T) {
			writer := httptest.NewRecorder()
			request := httptest.NewRequest(method, "/metrics", nil)
			NewHandler(New()).ServeHTTP(writer, request)
			if writer.Code != http.StatusMethodNotAllowed {
				t.Fatalf("status = %d, want %d", writer.Code, http.StatusMethodNotAllowed)
			}
			if got := writer.Header().Get("Allow"); got != http.MethodGet {
				t.Fatalf("allow = %q, want %q", got, http.MethodGet)
			}
			var body map[string]string
			if err := json.Unmarshal(writer.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode JSON error: %v", err)
			}
			if body["error"] != "method_not_allowed" {
				t.Fatalf("error body = %#v", body)
			}
		})
	}
}

func TestZeroValueAndNilHandler(t *testing.T) {
	var registry Metrics
	if got := registry.Snapshot(); got != (Snapshot{}) {
		t.Fatalf("zero snapshot = %#v", got)
	}
	writer := httptest.NewRecorder()
	NewHandler(nil).ServeHTTP(writer, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if writer.Code != http.StatusOK {
		t.Fatalf("nil handler status = %d, want %d", writer.Code, http.StatusOK)
	}
	if _, err := io.ReadAll(writer.Result().Body); err != nil {
		t.Fatal(err)
	}
}

func TestActiveRequestReleaseIsIdempotentAndClamped(t *testing.T) {
	var registry Metrics
	registry.DecActiveRequests()
	registry.AddActiveRequests(-10)
	if got := registry.Snapshot().ActiveRequests; got != 0 {
		t.Fatalf("clamped active requests = %d, want 0", got)
	}
	release := registry.BeginRequest()
	release()
	release()
	if got := registry.Snapshot().ActiveRequests; got != 0 {
		t.Fatalf("active requests after duplicate release = %d, want 0", got)
	}
}

func TestCountersAreSafeForConcurrentUpdates(t *testing.T) {
	registry := New()
	const workers = 16
	const iterations = 1000
	var group sync.WaitGroup
	group.Add(workers)
	for range workers {
		go func() {
			defer group.Done()
			for range iterations {
				registry.IncPlexRequests()
				registry.IncCloudPartHits()
				registry.IncCloudPartMisses()
				registry.IncRedirectSuccess()
				registry.IncRedirectFailure()
				registry.IncPlexFallback()
				registry.ObserveResolverLatency(time.Millisecond)
				registry.ObserveRedirectLatency(2 * time.Millisecond)
				release := registry.BeginRequest()
				release()
			}
		}()
	}
	group.Wait()
	want := uint64(workers * iterations)
	got := registry.Snapshot()
	if got.PlexRequestsTotal != want || got.CloudPartHits != want || got.CloudPartMisses != want ||
		got.RedirectSuccess != want || got.RedirectFailure != want || got.PlexFallbackTotal != want || got.ActiveRequests != 0 ||
		got.ResolverLatencySamples != want || got.ResolverLatencyMSTotal != want ||
		got.RedirectLatencySamples != want || got.RedirectLatencyMSTotal != 2*want {
		t.Fatalf("concurrent snapshot = %#v, want all counters %d and no active requests", got, want)
	}
}
