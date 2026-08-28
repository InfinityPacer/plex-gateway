package gateway

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	plexIdentityMaxBytes = 64 << 10
	plexIdentityMaxID    = 256
)

type plexIdentityResponse struct {
	MachineIdentifier string `xml:"machineIdentifier,attr"`
}

// ReadPlexServerIdentity obtains the stable PMS machine identifier without a
// management Token. Failure disables only consumers that require stable cache
// ownership; it does not weaken the proxy authentication boundary.
func ReadPlexServerIdentity(ctx context.Context, upstream *url.URL, timeout time.Duration) (string, error) {
	if upstream == nil {
		return "", errors.New("Plex identity probe requires an upstream")
	}
	if timeout <= 0 {
		timeout = defaultPartProbeTimeout
	}
	target := *upstream
	target.Path = joinURLPath(target.Path, "/identity")
	target.RawPath = ""
	target.RawQuery = ""
	target.Fragment = ""
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return "", fmt.Errorf("create Plex identity probe: %w", err)
	}
	request.Header.Set("Accept", "application/xml")
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
		return "", fmt.Errorf("run Plex identity probe: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("Plex identity probe returned HTTP %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, plexIdentityMaxBytes+1))
	if err != nil {
		return "", errors.New("read Plex identity response")
	}
	if len(body) > plexIdentityMaxBytes {
		return "", errors.New("Plex identity response exceeds the limit")
	}
	var identity plexIdentityResponse
	if err := xml.Unmarshal(body, &identity); err != nil {
		return "", errors.New("parse Plex identity response")
	}
	machineIdentifier := strings.TrimSpace(identity.MachineIdentifier)
	if machineIdentifier == "" || len(machineIdentifier) > plexIdentityMaxID ||
		strings.ContainsAny(machineIdentifier, "\x00\r\n") {
		return "", errors.New("Plex identity response has no valid machine identifier")
	}
	return machineIdentifier, nil
}
