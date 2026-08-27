package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestValidatePlexTrustBoundary(t *testing.T) {
	for _, test := range []struct {
		name    string
		status  int
		wantErr bool
	}{
		{name: "invalid token rejected", status: http.StatusUnauthorized},
		{name: "invalid token forbidden", status: http.StatusForbidden},
		{name: "trusted network bypass", status: http.StatusOK, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			plex := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/library/sections" ||
					!strings.HasPrefix(r.URL.Query().Get("X-Plex-Token"), "plex-gateway-invalid-") ||
					r.Header.Get("X-Plex-Token") != r.URL.Query().Get("X-Plex-Token") {
					t.Fatalf("unexpected trust-boundary request")
				}
				w.WriteHeader(test.status)
			}))
			defer plex.Close()
			upstream, err := url.Parse(plex.URL)
			if err != nil {
				t.Fatal(err)
			}
			err = ValidatePlexTrustBoundary(context.Background(), upstream, time.Second)
			if (err != nil) != test.wantErr {
				t.Fatalf("error = %v, wantErr = %v", err, test.wantErr)
			}
		})
	}
}

func TestPlexTrustBoundaryProbeDoesNotUseEnvironmentProxy(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://proxy.invalid:8080")
	plex := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer plex.Close()
	upstream, _ := url.Parse(plex.URL)
	if err := ValidatePlexTrustBoundary(context.Background(), upstream, time.Second); err != nil {
		t.Fatal(err)
	}
}
