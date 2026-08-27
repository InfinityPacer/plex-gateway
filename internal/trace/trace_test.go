package trace

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMiddlewareDoesNotLogQueryValuesOrToken(t *testing.T) {
	var output bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&output, nil))
	handler := New(true, logger).Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("ok"))
	}))

	request := httptest.NewRequest(http.MethodGet, "/library/metadata/1?X-Plex-Token=secret&includeMarkers=1", nil)
	request.Header.Set("X-Plex-Product", "Infuse")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	logged := output.String()
	if strings.Contains(logged, "secret") || strings.Contains(logged, "includeMarkers=1") {
		t.Fatalf("trace leaked query value: %s", logged)
	}
	for _, expected := range []string{"X-Plex-Token", "includeMarkers", "Infuse", `"status":201`} {
		if !strings.Contains(logged, expected) {
			t.Fatalf("trace missing %q: %s", expected, logged)
		}
	}
}
