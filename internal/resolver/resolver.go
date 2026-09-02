// Package resolver converts STRM files into direct playback URLs.
package resolver

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const (
	// DefaultMaxLineBytes bounds the amount of untrusted STRM content read for
	// one target. STRM files are one-line control files, not media streams.
	DefaultMaxLineBytes = 64 << 10

	// DefaultHTTPTimeout bounds a resolver request when its caller does not
	// provide a client with a timeout.
	DefaultHTTPTimeout = 15 * time.Second

	// maxInternalRedirectHops permits a short MediaVault redirect chain while
	// preventing a redirect loop from turning playback into an unbounded
	// outbound request sequence.
	maxInternalRedirectHops = 3
)

var (
	errInvalidBaseURL       = errors.New("MediaVault base URL is invalid")
	errInvalidTarget        = errors.New("STRM target is invalid")
	errSTRMRead             = errors.New("STRM target could not be read")
	errSTRMEmpty            = errors.New("STRM file has no target")
	errSTRMTooLong          = errors.New("STRM target exceeds the maximum length")
	errRequest              = errors.New("MediaVault redirect request failed")
	errNonRedirectResponse  = errors.New("MediaVault redirect returned a non-redirect response")
	errMissingLocation      = errors.New("MediaVault redirect response has no Location")
	errInvalidLocation      = errors.New("MediaVault redirect Location is invalid")
	errRedirectLoop         = errors.New("MediaVault redirect loop detected")
	errRedirectLimit        = errors.New("MediaVault redirect chain is too long")
	errSameOriginLocation   = errors.New("MediaVault redirect Location is outside /redirect")
	errUnsupportedTargetURL = errors.New("STRM target URL scheme is unsupported")
)

// DirectURL is the final URL a client should play directly.
//
// It intentionally contains no response metadata. A caller must not use the
// Gateway as a media proxy or retain the URL beyond a bounded control exchange.
type DirectURL struct {
	URL string
}

// String returns the URL in the form supplied by the trusted resolver.
func (u DirectURL) String() string {
	return u.URL
}

// PlaybackRequest carries the client request semantics that MediaVault may use
// when generating a client-compatible direct URL. Header is cloned and
// forwarded without filtering, so the configured MediaVault origin is part of
// the trusted Plex request boundary.
type PlaybackRequest struct {
	Method string
	Header http.Header
}

// ControlResolver exposes the two production boundaries of STRM playback: a
// local control target is read first, then resolved only after Plex authorizes
// that exact target.
type ControlResolver interface {
	ReadTarget(strmPath string) (string, error)
	ResolveTarget(ctx context.Context, target string, playback PlaybackRequest) (DirectURL, error)
}

// MediaVaultSTRMResolver reads MediaVault-compatible STRM files and resolves
// their /redirect endpoint without downloading media bytes.
type MediaVaultSTRMResolver struct {
	baseOrigin   url.URL
	client       *http.Client
	maxLineBytes int
}

// NewMediaVaultSTRMResolver validates a MediaVault origin and constructs a
// resolver. The client is cloned so its redirect policy cannot be changed for
// callers that share it with another subsystem; every resolver request uses
// GET and stops automatic redirect following at the first response.
func NewMediaVaultSTRMResolver(rawBaseURL string, client *http.Client, maxLineBytes int) (*MediaVaultSTRMResolver, error) {
	baseOrigin, err := parseBaseOrigin(rawBaseURL)
	if err != nil {
		return nil, err
	}
	if maxLineBytes <= 0 {
		maxLineBytes = DefaultMaxLineBytes
	}

	if client == nil {
		client = &http.Client{Transport: directTransport(), Timeout: DefaultHTTPTimeout}
	} else {
		clientCopy := *client
		client = &clientCopy
		if client.Transport == nil {
			client.Transport = directTransport()
		}
		if client.Timeout <= 0 {
			client.Timeout = DefaultHTTPTimeout
		}
	}
	// Resolver requests use only headers supplied by the active playback request.
	// Discarding a shared cookie jar prevents ambient client state from being
	// added by the configured HTTP client.
	client.Jar = nil
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}

	return &MediaVaultSTRMResolver{
		baseOrigin:   baseOrigin,
		client:       client,
		maxLineBytes: maxLineBytes,
	}, nil
}

func directTransport() *http.Transport {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	return transport
}

// ReadTarget reads and validates the first non-empty STRM control target
// without contacting MediaVault or any media origin.
func (r *MediaVaultSTRMResolver) ReadTarget(strmPath string) (string, error) {
	target, err := readSTRMTarget(strmPath, r.maxLineBytes)
	if err != nil {
		return "", err
	}
	if _, _, err := r.endpointForTarget(target); err != nil {
		return "", err
	}
	return target, nil
}

// ResolveTarget resolves a target previously accepted by ReadTarget. Callers
// can authorize that exact control target before any MediaVault request occurs.
func (r *MediaVaultSTRMResolver) ResolveTarget(ctx context.Context, target string, playback PlaybackRequest) (DirectURL, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	endpoint, requiresResolve, err := r.endpointForTarget(target)
	if err != nil {
		return DirectURL{}, err
	}
	if !requiresResolve {
		return DirectURL{URL: endpoint.String()}, nil
	}
	return r.resolveRedirect(ctx, endpoint, playback)
}

func parseBaseOrigin(rawBaseURL string) (url.URL, error) {
	base, err := url.Parse(strings.TrimSpace(rawBaseURL))
	if err != nil || base == nil || base.Host == "" || base.User != nil || base.Opaque != "" {
		return url.URL{}, errInvalidBaseURL
	}
	if !isHTTPURL(base) || base.Path != "" && base.Path != "/" {
		return url.URL{}, errInvalidBaseURL
	}
	if base.Hostname() == "" || base.RawQuery != "" || base.ForceQuery || base.Fragment != "" {
		return url.URL{}, errInvalidBaseURL
	}
	base.Path = ""
	base.RawPath = ""
	base.RawQuery = ""
	base.Fragment = ""
	return *base, nil
}

func (r *MediaVaultSTRMResolver) endpointForTarget(target string) (url.URL, bool, error) {
	if strings.HasPrefix(target, "/") {
		endpoint := r.baseOrigin
		endpoint.Path = "/redirect"
		endpoint.RawPath = ""
		values := endpoint.Query()
		values.Set("path", target)
		endpoint.RawQuery = values.Encode()
		return endpoint, true, nil
	}

	parsed, err := url.Parse(target)
	if err != nil || !parsed.IsAbs() || parsed.Host == "" || parsed.User != nil || parsed.Opaque != "" {
		return url.URL{}, false, errInvalidTarget
	}
	if !isHTTPURL(parsed) {
		return url.URL{}, false, errUnsupportedTargetURL
	}
	if hasRedirectPath(parsed.Path) {
		if hasDotPathSegment(parsed.Path) {
			return url.URL{}, false, errInvalidTarget
		}
		return r.mediaVaultEndpoint(parsed), true, nil
	}
	// The configured MediaVault origin is a control endpoint, not a generic
	// outbound proxy. A non-/redirect URL on that origin is therefore rejected;
	// other HTTP(S) templates are direct-play URLs and are never fetched here.
	if sameOrigin(parsed, &r.baseOrigin) {
		return url.URL{}, false, errInvalidTarget
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	parsed.Fragment = ""
	return *parsed, false, nil
}

func (r *MediaVaultSTRMResolver) mediaVaultEndpoint(source *url.URL) url.URL {
	endpoint := r.baseOrigin
	endpoint.Path = source.Path
	endpoint.RawPath = source.RawPath
	endpoint.RawQuery = source.RawQuery
	endpoint.Fragment = ""
	return endpoint
}

func (r *MediaVaultSTRMResolver) resolveRedirect(ctx context.Context, initial url.URL, playback PlaybackRequest) (DirectURL, error) {
	current := initial
	seen := make(map[string]struct{}, maxInternalRedirectHops)
	method := playback.Method
	if method == "" {
		method = http.MethodGet
	}

	for hop := 0; hop < maxInternalRedirectHops; hop++ {
		key := current.String()
		if _, exists := seen[key]; exists {
			return DirectURL{}, errRedirectLoop
		}
		seen[key] = struct{}{}

		request, err := http.NewRequestWithContext(ctx, method, current.String(), nil)
		if err != nil {
			return DirectURL{}, errRequest
		}
		request.Header = playback.Header.Clone()
		response, err := r.client.Do(request)
		if err != nil {
			return DirectURL{}, sanitizedError{message: errRequest.Error(), cause: err}
		}
		location := strings.TrimSpace(response.Header.Get("Location"))
		status := response.StatusCode
		_ = response.Body.Close()

		if status < http.StatusMultipleChoices || status >= http.StatusBadRequest {
			return DirectURL{}, fmt.Errorf("%w: HTTP %d", errNonRedirectResponse, status)
		}
		if location == "" {
			return DirectURL{}, errMissingLocation
		}

		next, err := resolveLocation(&current, location)
		if err != nil {
			return DirectURL{}, err
		}
		// MediaVault may advertise a public origin even when the gateway calls
		// its Docker-internal origin. Any bounded /redirect continuation is
		// rewritten back to the configured origin so internal or public control
		// addresses are never returned to the player.
		if hasRedirectPath(next.Path) {
			if hasDotPathSegment(next.Path) {
				return DirectURL{}, errSameOriginLocation
			}
			current = r.mediaVaultEndpoint(next)
			continue
		}
		if sameOrigin(next, &r.baseOrigin) {
			return DirectURL{}, errSameOriginLocation
		}
		return DirectURL{URL: next.String()}, nil
	}

	return DirectURL{}, errRedirectLimit
}

func resolveLocation(current *url.URL, rawLocation string) (*url.URL, error) {
	location, err := url.Parse(rawLocation)
	if err != nil || location == nil {
		return nil, errInvalidLocation
	}
	resolved := current.ResolveReference(location)
	if !isHTTPURL(resolved) || resolved.Host == "" || resolved.User != nil || resolved.Opaque != "" {
		return nil, errInvalidLocation
	}
	resolved.Scheme = strings.ToLower(resolved.Scheme)
	resolved.Fragment = ""
	return resolved, nil
}

func readSTRMTarget(path string, maxLineBytes int) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", errSTRMRead
	}
	defer file.Close()

	return readTarget(bufio.NewScanner(file), maxLineBytes)
}

func readTarget(scanner *bufio.Scanner, maxLineBytes int) (string, error) {
	scanner.Buffer(make([]byte, 4096), maxLineBytes)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line != "" {
			return line, nil
		}
	}
	if scanner.Err() != nil {
		return "", errSTRMTooLong
	}
	return "", errSTRMEmpty
}

func isHTTPURL(value *url.URL) bool {
	if value == nil {
		return false
	}
	scheme := strings.ToLower(value.Scheme)
	return scheme == "http" || scheme == "https"
}

func hasRedirectPath(path string) bool {
	return path == "/redirect" || strings.HasPrefix(path, "/redirect/")
}

func hasDotPathSegment(path string) bool {
	for _, segment := range strings.Split(path, "/") {
		if segment == "." || segment == ".." {
			return true
		}
	}
	return false
}

func sameOrigin(left, right *url.URL) bool {
	if left == nil || right == nil || !isHTTPURL(left) || !isHTTPURL(right) {
		return false
	}
	return strings.EqualFold(left.Scheme, right.Scheme) && strings.EqualFold(left.Host, right.Host)
}

// sanitizedError keeps errors.Is useful for context cancellation while its
// public message cannot expose a request URL or signed query string.
type sanitizedError struct {
	message string
	cause   error
}

func (e sanitizedError) Error() string {
	return e.message
}

func (e sanitizedError) Unwrap() error {
	return e.cause
}
