package gateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// ValidatePlexTrustBoundary verifies that Plex authenticates clients arriving
// from the gateway network. If Plex accepts a fresh invalid Token, transparent
// proxying would inherit network-level access and must not start.
func ValidatePlexTrustBoundary(ctx context.Context, upstream *url.URL, timeout time.Duration) error {
	if upstream == nil {
		return errors.New("Plex trust-boundary probe requires an upstream")
	}
	if timeout <= 0 {
		timeout = defaultPartProbeTimeout
	}
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		return errors.New("create Plex trust-boundary token")
	}
	token := "plex-gateway-invalid-" + hex.EncodeToString(tokenBytes)
	target := *upstream
	target.Path = joinURLPath(target.Path, "/library/sections")
	target.RawPath = ""
	target.RawQuery = url.Values{"X-Plex-Token": []string{token}}.Encode()
	target.Fragment = ""

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return fmt.Errorf("create Plex trust-boundary probe: %w", err)
	}
	request.Header.Set("X-Plex-Token", token)
	request.Header.Set("Accept-Encoding", "identity")
	request.Host = upstream.Host
	client := &http.Client{
		Transport: newTransport(),
		Timeout:   timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("run Plex trust-boundary probe: %w", err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized && response.StatusCode != http.StatusForbidden {
		return fmt.Errorf("Plex accepted an unauthenticated gateway-network request with HTTP %d", response.StatusCode)
	}
	return nil
}
