package gateway

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/InfinityPacer/plex-gateway/internal/metrics"
)

var metadataAnalysisParameters = map[string]struct{}{
	"asyncAugmentMetadata": {},
	"checkFiles":           {},
}

// newMetadataAnalysisFilter prevents detailed metadata reads from asking Plex
// to inspect media files on demand. Library scans remain responsible for local
// file freshness, while STRM entries can use the Gateway MediaInfo projection.
func newMetadataAnalysisFilter(enabled bool, next http.Handler, registry *metrics.Metrics) http.Handler {
	if !enabled {
		return next
	}
	if registry == nil {
		registry = metrics.New()
	}
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if classifyMetadataRequest(request) == metadataRequestNone || request.URL.RawQuery == "" {
			next.ServeHTTP(writer, request)
			return
		}

		rawQuery, changed := removeRawQueryParameters(request.URL.RawQuery, metadataAnalysisParameters)
		if !changed {
			next.ServeHTTP(writer, request)
			return
		}

		filtered := request.Clone(request.Context())
		requestURL := *request.URL
		requestURL.RawQuery = rawQuery
		filtered.URL = &requestURL
		registry.IncMetadataAnalysisParamsRemoved()
		next.ServeHTTP(writer, filtered)
	})
}

// removeRawQueryParameters preserves every retained query segment byte for
// byte. Plex clients use several equivalent escaping forms, and filtering two
// control parameters must not normalize credentials or client context.
func removeRawQueryParameters(rawQuery string, blocked map[string]struct{}) (string, bool) {
	segments := strings.Split(rawQuery, "&")
	kept := make([]string, 0, len(segments))
	changed := false
	for _, segment := range segments {
		key := segment
		if separator := strings.IndexByte(key, '='); separator >= 0 {
			key = key[:separator]
		}
		decoded, err := url.QueryUnescape(key)
		if err == nil {
			if _, remove := blocked[decoded]; remove {
				changed = true
				continue
			}
		}
		kept = append(kept, segment)
	}
	if !changed {
		return rawQuery, false
	}
	return strings.Join(kept, "&"), true
}
