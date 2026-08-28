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
	registry.IncMetadataGuardAdmitted()
	registry.IncMetadataGuardTimeouts()
	registry.IncMetadataGuardActive()
	registry.IncMetadataGuardQueued()
	registry.IncMetadataBatchGuardAdmitted()
	registry.IncMetadataBatchGuardTimeouts()
	registry.IncMetadataBatchGuardActive()
	registry.IncMetadataBatchGuardQueued()
	registry.IncMediaInfoCacheHits()
	registry.IncMediaInfoCacheMisses()
	registry.IncMediaInfoProbeQueued()
	registry.IncMediaInfoProbeSuccess()
	registry.IncMediaInfoProbeFailure()
	registry.IncMediaInfoStoreFailure()
	registry.IncMediaInfoProbeActive()
	registry.IncMediaInfoEnriched()
	registry.IncMediaInfoFailOpen()
	registry.IncMediaInfoWaitActive()
	registry.IncMediaInfoWaitRejected()
	registry.IncMediaInfoPrewarmTriggered()
	registry.IncMediaInfoPrewarmReplaced()
	registry.IncMediaInfoPrewarmDiscoverySuccess()
	registry.IncMediaInfoPrewarmDiscoveryFailure()
	registry.IncMediaInfoPrewarmFreshCache()
	registry.IncMediaInfoPrewarmJoinedFlight()
	registry.IncMediaInfoPrewarmQueued()
	registry.IncMediaInfoPrewarmRejected()
	registry.IncMediaInfoPrewarmSkipped()
	registry.ObserveMediaInfoProbeLatency(44 * time.Millisecond)
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

		MetadataGuardAdmittedTotal:            1,
		MetadataGuardTimeoutsTotal:            1,
		MetadataGuardActive:                   1,
		MetadataGuardQueued:                   1,
		MetadataBatchGuardAdmittedTotal:       1,
		MetadataBatchGuardTimeoutsTotal:       1,
		MetadataBatchGuardActive:              1,
		MetadataBatchGuardQueued:              1,
		MediaInfoCacheHitsTotal:               1,
		MediaInfoCacheMissesTotal:             1,
		MediaInfoProbeQueuedTotal:             1,
		MediaInfoProbeSuccessTotal:            1,
		MediaInfoProbeFailureTotal:            1,
		MediaInfoStoreFailureTotal:            1,
		MediaInfoProbeActive:                  1,
		MediaInfoEnrichedTotal:                1,
		MediaInfoFailOpenTotal:                1,
		MediaInfoWaitActive:                   1,
		MediaInfoWaitRejectedTotal:            1,
		MediaInfoPrewarmTriggeredTotal:        1,
		MediaInfoPrewarmReplacedTotal:         1,
		MediaInfoPrewarmDiscoverySuccessTotal: 1,
		MediaInfoPrewarmDiscoveryFailureTotal: 1,
		MediaInfoPrewarmFreshCacheTotal:       1,
		MediaInfoPrewarmJoinedFlightTotal:     1,
		MediaInfoPrewarmQueuedTotal:           1,
		MediaInfoPrewarmRejectedTotal:         1,
		MediaInfoPrewarmSkippedTotal:          1,

		ResolverLatencyMSTotal: 19,
		ResolverLatencySamples: 2,
		ResolverLatencyMSLast:  7,
		ResolverLatencyMSMax:   12,

		RedirectLatencyMSTotal: 30,
		RedirectLatencySamples: 1,
		RedirectLatencyMSLast:  30,
		RedirectLatencyMSMax:   30,

		MediaInfoProbeLatencyMSTotal: 44,
		MediaInfoProbeLatencySamples: 1,
		MediaInfoProbeLatencyMSLast:  44,
		MediaInfoProbeLatencyMSMax:   44,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("snapshot = %#v, want %#v", got, want)
	}
	release()
	registry.DecMetadataGuardActive()
	registry.DecMetadataGuardQueued()
	registry.DecMetadataBatchGuardActive()
	registry.DecMetadataBatchGuardQueued()
	registry.DecMediaInfoProbeActive()
	registry.DecMediaInfoWaitActive()
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
	registry.DecMetadataGuardActive()
	registry.DecMetadataGuardQueued()
	registry.DecMetadataBatchGuardActive()
	registry.DecMetadataBatchGuardQueued()
	registry.DecMediaInfoProbeActive()
	registry.DecMediaInfoWaitActive()
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
				registry.IncMetadataGuardAdmitted()
				registry.IncMetadataGuardTimeouts()
				registry.IncMetadataGuardActive()
				registry.DecMetadataGuardActive()
				registry.IncMetadataGuardQueued()
				registry.DecMetadataGuardQueued()
				registry.IncMetadataBatchGuardAdmitted()
				registry.IncMetadataBatchGuardTimeouts()
				registry.IncMetadataBatchGuardActive()
				registry.DecMetadataBatchGuardActive()
				registry.IncMetadataBatchGuardQueued()
				registry.DecMetadataBatchGuardQueued()
				registry.IncMediaInfoCacheHits()
				registry.IncMediaInfoCacheMisses()
				registry.IncMediaInfoProbeQueued()
				registry.IncMediaInfoProbeSuccess()
				registry.IncMediaInfoProbeFailure()
				registry.IncMediaInfoStoreFailure()
				registry.IncMediaInfoProbeActive()
				registry.DecMediaInfoProbeActive()
				registry.IncMediaInfoEnriched()
				registry.IncMediaInfoFailOpen()
				registry.IncMediaInfoWaitActive()
				registry.DecMediaInfoWaitActive()
				registry.IncMediaInfoWaitRejected()
				registry.IncMediaInfoPrewarmTriggered()
				registry.IncMediaInfoPrewarmReplaced()
				registry.IncMediaInfoPrewarmDiscoverySuccess()
				registry.IncMediaInfoPrewarmDiscoveryFailure()
				registry.IncMediaInfoPrewarmFreshCache()
				registry.IncMediaInfoPrewarmJoinedFlight()
				registry.IncMediaInfoPrewarmQueued()
				registry.IncMediaInfoPrewarmRejected()
				registry.IncMediaInfoPrewarmSkipped()
				registry.ObserveMediaInfoProbeLatency(3 * time.Millisecond)
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
		got.MetadataGuardAdmittedTotal != want || got.MetadataGuardTimeoutsTotal != want ||
		got.MetadataGuardActive != 0 || got.MetadataGuardQueued != 0 ||
		got.MetadataBatchGuardAdmittedTotal != want || got.MetadataBatchGuardTimeoutsTotal != want ||
		got.MetadataBatchGuardActive != 0 || got.MetadataBatchGuardQueued != 0 ||
		got.MediaInfoCacheHitsTotal != want || got.MediaInfoCacheMissesTotal != want ||
		got.MediaInfoProbeQueuedTotal != want || got.MediaInfoProbeSuccessTotal != want ||
		got.MediaInfoProbeFailureTotal != want || got.MediaInfoStoreFailureTotal != want || got.MediaInfoProbeActive != 0 ||
		got.MediaInfoEnrichedTotal != want || got.MediaInfoFailOpenTotal != want || got.MediaInfoWaitActive != 0 || got.MediaInfoWaitRejectedTotal != want ||
		got.MediaInfoPrewarmTriggeredTotal != want || got.MediaInfoPrewarmReplacedTotal != want ||
		got.MediaInfoPrewarmDiscoverySuccessTotal != want || got.MediaInfoPrewarmDiscoveryFailureTotal != want ||
		got.MediaInfoPrewarmFreshCacheTotal != want || got.MediaInfoPrewarmJoinedFlightTotal != want ||
		got.MediaInfoPrewarmQueuedTotal != want || got.MediaInfoPrewarmRejectedTotal != want || got.MediaInfoPrewarmSkippedTotal != want ||
		got.ResolverLatencySamples != want || got.ResolverLatencyMSTotal != want ||
		got.RedirectLatencySamples != want || got.RedirectLatencyMSTotal != 2*want ||
		got.MediaInfoProbeLatencySamples != want || got.MediaInfoProbeLatencyMSTotal != 3*want {
		t.Fatalf("concurrent snapshot = %#v, want all counters %d and no active requests", got, want)
	}
}
