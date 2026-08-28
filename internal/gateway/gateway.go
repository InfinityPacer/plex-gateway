package gateway

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/InfinityPacer/plex-gateway/internal/metrics"
	"github.com/InfinityPacer/plex-gateway/internal/observe"
	"github.com/InfinityPacer/plex-gateway/internal/partcache"
	"github.com/InfinityPacer/plex-gateway/internal/pathmap"
	"github.com/InfinityPacer/plex-gateway/internal/playback"
	"github.com/InfinityPacer/plex-gateway/internal/resolver"
	"github.com/InfinityPacer/plex-gateway/internal/trace"
)

const defaultPartProbeTimeout = 15 * time.Second

// Options describes the optional cloud playback plane around the transparent
// Plex proxy. Nil cloud components keep the handler in proxy-only mode.
type Options struct {
	Upstream         *url.URL
	Logger           *slog.Logger
	Tracer           *trace.Tracer
	PartCache        *partcache.Cache
	PathMapper       *pathmap.Mapper
	Resolver         resolver.ControlResolver
	Metrics          *metrics.Metrics
	CloudExtensions  []string
	ObserveMaxBytes  int64
	PartProbeTimeout time.Duration
	MetadataGuard    MetadataGuardOptions
}

// New builds the fail-open Plex proxy and optional Direct Play interceptor.
// Metadata observation never mutates the Plex response body.
func New(options Options) http.Handler {
	logger := options.Logger
	if logger == nil {
		logger = slog.Default()
	}
	registry := options.Metrics
	if registry == nil {
		registry = metrics.New()
	}
	tracer := options.Tracer
	if tracer == nil {
		tracer = trace.New(false, logger)
	}

	var cloudPlayback *playback.Service
	transport := http.RoundTripper(newTransport())
	if options.PartCache != nil {
		transport = observe.NewRoundTripper(transport, observe.Config{
			MetadataPaths: []string{
				"/library/metadata",
				"/library/sections",
				"/hubs",
				"/playQueues",
			},
			MaxBodyBytes: options.ObserveMaxBytes,
			OnParts: func(observation observe.Observation) {
				for _, part := range observation.Parts {
					if cloudPlayback != nil {
						cloudPlayback.Remember(playback.PreparedPart{Part: part})
					}
				}
			},
		})
	}

	proxy := &httputil.ReverseProxy{
		Rewrite: func(request *httputil.ProxyRequest) {
			request.SetURL(options.Upstream)
			request.Out.Host = options.Upstream.Host
			request.SetXForwarded()
		},
		Transport:     transport,
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if !errors.Is(err, context.Canceled) {
				logger.Error("plex_proxy_error", "method", r.Method, "path", r.URL.EscapedPath(), "error_kind", errorKind(err))
			}
			http.Error(w, http.StatusText(http.StatusBadGateway), http.StatusBadGateway)
		},
	}
	plex := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		registry.IncPlexRequests()
		proxy.ServeHTTP(w, r)
	})
	probeTimeout := options.PartProbeTimeout
	if probeTimeout <= 0 {
		probeTimeout = defaultPartProbeTimeout
	}

	partAuthorizer := &partProbe{
		upstream: options.Upstream,
		client: &http.Client{
			Transport: transport,
			Timeout:   probeTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
	cloudPlayback = playback.New(playback.Options{
		Cache:           options.PartCache,
		Mapper:          options.PathMapper,
		Resolver:        options.Resolver,
		AuthorizePart:   partAuthorizer.Authorize,
		CloudExtensions: options.CloudExtensions,
	})
	partPlayback := &playbackHandler{
		service: cloudPlayback,
		plex:    plex,
		logger:  logger,
		metrics: registry,
	}
	decision := &decisionHandler{
		plex:    plex,
		service: cloudPlayback,
		logger:  logger,
		grants:  playback.NewGrantStore(defaultDecisionGrantTTL, defaultDecisionGrantLimit),
		probe: &decisionMetadataProbe{
			upstream: options.Upstream,
			client: &http.Client{
				Transport: transport,
				Timeout:   probeTimeout,
				CheckRedirect: func(*http.Request, []*http.Request) error {
					return http.ErrUseLastResponse
				},
			},
			maxBytes: defaultDecisionMetadataMaxBytes,
		},
	}
	universalStart := &universalStartHandler{grants: decision.grants, playback: partPlayback}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("{\"status\":\"ok\"}\n"))
	})
	mux.Handle("GET /metrics", registry.Handler())
	mux.Handle("GET /video/:/transcode/universal/decision", decision)
	mux.Handle("HEAD /video/:/transcode/universal/decision", decision)
	for _, route := range []string{
		"/video/:/transcode/universal/start",
		"/video/:/transcode/universal/start.mpd",
		"/video/:/transcode/universal/start.m3u8",
	} {
		mux.Handle("GET "+route, universalStart)
		mux.Handle("HEAD "+route, universalStart)
	}
	mux.Handle("GET /library/parts/{partID}/{rest...}", partPlayback)
	mux.Handle("HEAD /library/parts/{partID}/{rest...}", partPlayback)
	mux.Handle("/", newMetadataGuard(options.MetadataGuard, plex, registry, logger))

	withActiveRequests := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		release := registry.BeginRequest()
		defer release()
		mux.ServeHTTP(w, r)
	})
	return tracer.Middleware(withActiveRequests)
}

type playbackHandler struct {
	service *playback.Service
	plex    http.Handler
	logger  *slog.Logger
	metrics *metrics.Metrics
}

func (h *playbackHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	partID := r.PathValue("partID")
	preparation := h.service.PrepareCached(partID)
	switch preparation.State {
	case playback.PreparationUnavailable:
		h.fallback(w, r, partID, preparation.Reason)
		return
	case playback.PreparationMissing:
		h.metrics.IncCloudPartMisses()
		h.fallback(w, r, partID, preparation.Reason)
		return
	case playback.PreparationLocal:
		h.plex.ServeHTTP(w, r)
		return
	case playback.PreparationFailed:
		if preparation.Cloud {
			h.metrics.IncCloudPartHits()
		}
		h.metrics.IncRedirectFailure()
		h.fallback(w, r, partID, preparation.Reason)
		return
	case playback.PreparationReady:
		h.metrics.IncCloudPartHits()
	default:
		h.fallback(w, r, partID, "preparation_unknown")
		return
	}
	if !hasPlexToken(r) {
		h.fallback(w, r, partID, "missing_plex_token")
		return
	}
	h.servePrepared(w, r, preparation.Part, r.URL.RequestURI(), false, started, "cache")
}

func (h *playbackHandler) servePrepared(
	w http.ResponseWriter,
	r *http.Request,
	part playback.PreparedPart,
	partReference string,
	refreshCache bool,
	started time.Time,
	source string,
) {
	result, err := h.service.Play(playback.PlayInput{
		Request:                 r,
		Part:                    part,
		PartReference:           partReference,
		RefreshCacheOnAuthorize: refreshCache,
	})
	if err != nil {
		if requestCanceled(r, err) {
			return
		}
		reason := "playback"
		var failure *playback.Failure
		if errors.As(err, &failure) {
			reason = string(failure.Kind)
			if failure.Kind == playback.FailureResolver || failure.Kind == playback.FailureEmptyLocation {
				h.metrics.ObserveResolverLatency(result.ResolverLatency)
			}
		}
		h.metrics.IncRedirectFailure()
		h.fallback(w, r, part.Part.ID, reason)
		return
	}
	if requestFinished(r) {
		return
	}
	h.metrics.ObserveResolverLatency(result.ResolverLatency)
	h.redirect(w, part.Part.ID, result.DirectURL, started, source)
}

// requestCanceled separates downstream disconnects from failures in Plex or
// MediaVault. Both the request context and the operation error must identify
// the same cancellation so an unrelated upstream failure remains observable.
func requestCanceled(r *http.Request, operationErr error) bool {
	if r == nil || operationErr == nil {
		return false
	}
	cause := r.Context().Err()
	return cause != nil && errors.Is(operationErr, cause)
}

func requestFinished(r *http.Request) bool {
	return r != nil && r.Context().Err() != nil
}

func (h *playbackHandler) redirect(w http.ResponseWriter, partID string, directURL resolver.DirectURL, started time.Time, source string) {
	latency := time.Since(started)
	h.metrics.IncRedirectSuccess()
	h.metrics.ObserveRedirectLatency(latency)
	w.Header().Set("Location", directURL.String())
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusFound)
	h.logger.Info("play",
		"part", partID,
		"source", source,
		"resolver", "mediavault",
		"result", http.StatusFound,
		"latency_ms", latency.Milliseconds(),
	)
}

func (h *playbackHandler) fallback(w http.ResponseWriter, r *http.Request, partID, reason string) {
	h.metrics.IncPlexFallback()
	h.logger.Info("play_fallback", "part", partID, "reason", reason)
	h.plex.ServeHTTP(w, r)
}

type partProbe struct {
	upstream *url.URL
	client   *http.Client
}

// Authorize asks Plex to authorize one Part with the caller's credentials.
// It never reads the response body. Redirect authorization is valid only when
// Plex points at the exact control target read from the mapped STRM file.
func (p *partProbe) Authorize(original *http.Request, partReference, expectedTarget string) (bool, error) {
	if p == nil || p.upstream == nil || p.client == nil || original == nil || expectedTarget == "" {
		return false, errors.New("part probe is unavailable")
	}
	reference, err := url.Parse(partReference)
	if err != nil || reference.IsAbs() || reference.Host != "" || !strings.HasPrefix(reference.Path, "/library/parts/") {
		return false, errors.New("part probe reference is invalid")
	}
	target := *p.upstream
	target.Path = joinURLPath(target.Path, reference.Path)
	target.RawPath = ""
	query := reference.Query()
	for name, values := range original.URL.Query() {
		if _, present := query[name]; present {
			continue
		}
		for _, value := range values {
			query.Add(name, value)
		}
	}
	target.RawQuery = query.Encode()
	target.Fragment = ""

	request, err := http.NewRequestWithContext(original.Context(), http.MethodGet, target.String(), nil)
	if err != nil {
		return false, err
	}
	request.Header = original.Header.Clone()
	removeHopByHopHeaders(request.Header)
	removeForwardingHeaders(request.Header)
	request.Header.Set("Range", "bytes=0-0")
	request.Header.Set("Accept-Encoding", "identity")
	request.Header.Del("If-Range")
	request.Header.Del("If-Modified-Since")
	request.Header.Del("If-None-Match")
	request.Host = p.upstream.Host

	response, err := p.client.Do(request)
	if err != nil {
		return false, err
	}
	defer response.Body.Close()
	if isPartRedirect(response.StatusCode) {
		location := strings.TrimSpace(response.Header.Get("Location"))
		return location != "" && sameControlTarget(expectedTarget, location), nil
	}
	return false, nil
}

func sameControlTarget(expected, actual string) bool {
	expectedURL, err := url.Parse(strings.TrimSpace(expected))
	if err != nil {
		return false
	}
	actualURL, err := url.Parse(strings.TrimSpace(actual))
	if err != nil {
		return false
	}
	expectedQuery, err := url.ParseQuery(expectedURL.RawQuery)
	if err != nil {
		return false
	}
	actualQuery, err := url.ParseQuery(actualURL.RawQuery)
	if err != nil {
		return false
	}
	if expectedURL.User != nil || actualURL.User != nil || expectedURL.Opaque != "" || actualURL.Opaque != "" {
		return false
	}
	return strings.EqualFold(expectedURL.Scheme, actualURL.Scheme) &&
		strings.EqualFold(expectedURL.Host, actualURL.Host) &&
		expectedURL.EscapedPath() == actualURL.EscapedPath() &&
		expectedQuery.Encode() == actualQuery.Encode()
}

func isPartRedirect(status int) bool {
	switch status {
	case http.StatusMovedPermanently,
		http.StatusFound,
		http.StatusSeeOther,
		http.StatusTemporaryRedirect,
		http.StatusPermanentRedirect:
		return true
	default:
		return false
	}
}

func joinURLPath(basePath, requestPath string) string {
	switch {
	case basePath == "":
		return requestPath
	case requestPath == "":
		return basePath
	case strings.HasSuffix(basePath, "/") && strings.HasPrefix(requestPath, "/"):
		return basePath + requestPath[1:]
	case !strings.HasSuffix(basePath, "/") && !strings.HasPrefix(requestPath, "/"):
		return basePath + "/" + requestPath
	default:
		return basePath + requestPath
	}
}

func removeHopByHopHeaders(header http.Header) {
	if connection := header.Values("Connection"); len(connection) > 0 {
		for _, value := range connection {
			for _, name := range strings.Split(value, ",") {
				header.Del(strings.TrimSpace(name))
			}
		}
	}
	for _, name := range []string{
		"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate",
		"Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade",
	} {
		header.Del(name)
	}
}

// removeForwardingHeaders prevents client-supplied network identity from
// influencing Plex authorization probes. Transparent proxy requests construct
// their own forwarding chain through ReverseProxy.SetXForwarded.
func removeForwardingHeaders(header http.Header) {
	for _, name := range []string{
		"Forwarded", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Port",
		"X-Forwarded-Proto", "X-Real-Ip", "Client-Ip", "True-Client-Ip",
		"Cf-Connecting-Ip",
	} {
		header.Del(name)
	}
}

// hasPlexToken requires request-scoped Plex authentication material before a
// cloud redirect can use Plex through the gateway's trusted network position.
func hasPlexToken(request *http.Request) bool {
	if request == nil || request.URL == nil {
		return false
	}
	queryValues := request.URL.Query()["X-Plex-Token"]
	if len(queryValues) > 1 {
		return false
	}
	headerValues := request.Header.Values("X-Plex-Token")
	if len(headerValues) > 1 {
		return false
	}
	return len(queryValues) == 1 && strings.TrimSpace(queryValues[0]) != "" ||
		len(headerValues) == 1 && strings.TrimSpace(headerValues[0]) != ""
}

// errorKind keeps operational logs useful without serializing transport error
// strings that may contain a request URL or other request-scoped data.
func errorKind(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	var netError net.Error
	if errors.As(err, &netError) {
		if netError.Timeout() {
			return "timeout"
		}
		return "network"
	}
	return "upstream"
}

func newTransport() *http.Transport {
	dialer := &net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
	}
	return &http.Transport{
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   32,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
}
