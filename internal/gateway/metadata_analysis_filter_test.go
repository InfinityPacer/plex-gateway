package gateway

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/InfinityPacer/plex-gateway/internal/metrics"
)

func TestMetadataAnalysisFilterRemovesOnlyAnalysisParameters(t *testing.T) {
	registry := metrics.New()
	var received string
	handler := newMetadataAnalysisFilter(true, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		received = request.URL.RawQuery
		writer.WriteHeader(http.StatusNoContent)
	}), registry)
	originalQuery := "z=last&checkFiles=1&X-Plex-Token=a%2Bb&asyncAugmentMetadata=1&a=first+value"
	request := httptest.NewRequest(http.MethodGet, "/library/metadata/42?"+originalQuery, nil)
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusNoContent)
	}
	if received != "z=last&checkFiles=1&X-Plex-Token=a%2Bb&a=first+value" {
		t.Fatalf("filtered query = %q", received)
	}
	if request.URL.RawQuery != originalQuery {
		t.Fatalf("original request query mutated to %q", request.URL.RawQuery)
	}
	if got := registry.Snapshot().MetadataAnalysisParamsRemovedTotal; got != 1 {
		t.Fatalf("removed total = %d, want 1", got)
	}
}

func TestMetadataAnalysisFilterAppliesToSupportedReads(t *testing.T) {
	tests := []struct {
		name   string
		method string
		path   string
		want   string
	}{
		{name: "head", method: http.MethodHead, path: "/library/metadata/42?asyncAugmentMetadata=1&checkFiles=1&keep=yes", want: "checkFiles=1&keep=yes"},
		{name: "batch", method: http.MethodGet, path: "/library/metadata/42,43?asyncAugmentMetadata&keep=yes", want: "keep=yes"},
		{name: "children", method: http.MethodGet, path: "/library/metadata/42/children?asyncAugmentMetadata=1", want: "asyncAugmentMetadata=1"},
		{name: "write", method: http.MethodPut, path: "/library/metadata/42?asyncAugmentMetadata=1", want: "asyncAugmentMetadata=1"},
		{name: "non_numeric", method: http.MethodGet, path: "/library/metadata/abc?asyncAugmentMetadata=1", want: "asyncAugmentMetadata=1"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var received string
			handler := newMetadataAnalysisFilter(true, http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
				received = request.URL.RawQuery
			}), metrics.New())

			handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(test.method, test.path, nil))

			if received != test.want {
				t.Fatalf("query = %q, want %q", received, test.want)
			}
		})
	}
}

func TestMetadataAnalysisFilterPreservesMalformedAndDisabledQueries(t *testing.T) {
	tests := []struct {
		name    string
		enabled bool
		query   string
	}{
		{name: "disabled", enabled: false, query: "asyncAugmentMetadata=1&keep=yes"},
		{name: "malformed_key", enabled: true, query: "asyncAugmentMetadata%ZZ=1&keep=yes"},
		{name: "unrelated", enabled: true, query: "asyncAugmentMetadataExtra=1&keep=yes"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var received string
			handler := newMetadataAnalysisFilter(test.enabled, http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
				received = request.URL.RawQuery
			}), metrics.New())

			handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/library/metadata/42?"+test.query, nil))

			if received != test.query {
				t.Fatalf("query = %q, want %q", received, test.query)
			}
		})
	}
}

func BenchmarkMetadataAnalysisFilter(b *testing.B) {
	tests := []struct {
		name string
		path string
	}{
		{name: "non_metadata", path: "/library/sections/49/all?type=4"},
		{name: "metadata_clean", path: "/library/metadata/42?includeMarkers=1"},
		{name: "metadata_filtered", path: "/library/metadata/42?checkFiles=1&asyncAugmentMetadata=1&includeMarkers=1"},
	}
	for _, test := range tests {
		b.Run(test.name, func(b *testing.B) {
			handler := newMetadataAnalysisFilter(true, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), metrics.New())
			request := httptest.NewRequest(http.MethodGet, test.path, nil)
			response := httptest.NewRecorder()
			b.ReportAllocs()
			for b.Loop() {
				handler.ServeHTTP(response, request)
			}
		})
	}
}
