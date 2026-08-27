package gateway

import (
	"context"
	"crypto/sha256"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/InfinityPacer/plex-gateway/internal/metrics"
)

const (
	defaultMetadataGlobalConcurrency    = 8
	defaultMetadataPerClientConcurrency = 4
	defaultMetadataQueueTimeout         = 10 * time.Second
)

// MetadataGuardOptions controls admission for detailed metadata requests. The
// guard does not cover playback, timeline, scrobble, or library listing paths.
type MetadataGuardOptions struct {
	Enabled              bool
	GlobalConcurrency    int
	PerClientConcurrency int
	QueueTimeout         time.Duration
}

type metadataGuard struct {
	next         http.Handler
	logger       *slog.Logger
	metrics      *metrics.Metrics
	global       chan struct{}
	perClientMax int
	queueTimeout time.Duration

	mu      sync.Mutex
	clients map[string]*metadataClientBucket
}

type metadataClientBucket struct {
	tokens chan struct{}
	refs   int
}

func newMetadataGuard(options MetadataGuardOptions, next http.Handler, registry *metrics.Metrics, logger *slog.Logger) http.Handler {
	if !options.Enabled {
		return next
	}
	if options.GlobalConcurrency <= 0 {
		options.GlobalConcurrency = defaultMetadataGlobalConcurrency
	}
	if options.PerClientConcurrency <= 0 {
		options.PerClientConcurrency = defaultMetadataPerClientConcurrency
	}
	if options.PerClientConcurrency > options.GlobalConcurrency {
		options.PerClientConcurrency = options.GlobalConcurrency
	}
	if options.QueueTimeout <= 0 {
		options.QueueTimeout = defaultMetadataQueueTimeout
	}
	if registry == nil {
		registry = metrics.New()
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &metadataGuard{
		next:         next,
		logger:       logger,
		metrics:      registry,
		global:       make(chan struct{}, options.GlobalConcurrency),
		perClientMax: options.PerClientConcurrency,
		queueTimeout: options.QueueTimeout,
		clients:      make(map[string]*metadataClientBucket),
	}
}

func (g *metadataGuard) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !isDetailedMetadataRequest(r) {
		g.next.ServeHTTP(w, r)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), g.queueTimeout)
	defer cancel()
	release, err := g.acquire(ctx, metadataClientKey(r))
	if err != nil {
		if r.Context().Err() != nil {
			return
		}
		g.metrics.IncMetadataGuardTimeouts()
		g.logger.Warn("metadata_guard_timeout", "method", r.Method)
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Retry-After", "1")
		http.Error(w, http.StatusText(http.StatusTooManyRequests), http.StatusTooManyRequests)
		return
	}
	defer release()
	g.next.ServeHTTP(w, r)
}

func (g *metadataGuard) acquire(ctx context.Context, clientKey string) (func(), error) {
	bucket := g.retainClient(clientKey)
	g.metrics.IncMetadataGuardQueued()
	defer g.metrics.DecMetadataGuardQueued()

	select {
	case bucket.tokens <- struct{}{}:
	case <-ctx.Done():
		g.releaseClient(clientKey, bucket)
		return nil, ctx.Err()
	}

	select {
	case g.global <- struct{}{}:
	case <-ctx.Done():
		<-bucket.tokens
		g.releaseClient(clientKey, bucket)
		return nil, ctx.Err()
	}

	g.metrics.IncMetadataGuardAdmitted()
	g.metrics.IncMetadataGuardActive()
	var once sync.Once
	return func() {
		once.Do(func() {
			<-g.global
			<-bucket.tokens
			g.releaseClient(clientKey, bucket)
			g.metrics.DecMetadataGuardActive()
		})
	}, nil
}

func (g *metadataGuard) retainClient(key string) *metadataClientBucket {
	g.mu.Lock()
	defer g.mu.Unlock()
	bucket := g.clients[key]
	if bucket == nil {
		bucket = &metadataClientBucket{tokens: make(chan struct{}, g.perClientMax)}
		g.clients[key] = bucket
	}
	bucket.refs++
	return bucket
}

func (g *metadataGuard) releaseClient(key string, bucket *metadataClientBucket) {
	g.mu.Lock()
	defer g.mu.Unlock()
	current := g.clients[key]
	if current != bucket {
		return
	}
	bucket.refs--
	if bucket.refs == 0 {
		delete(g.clients, key)
	}
}

func metadataClientKey(r *http.Request) string {
	identifier := singleValue(r.Header.Values("X-Plex-Client-Identifier"))
	if identifier == "" && r.URL != nil {
		identifier = singleValue(r.URL.Query()["X-Plex-Client-Identifier"])
	}
	if identifier == "" {
		return "anonymous"
	}
	digest := sha256.Sum256([]byte(identifier))
	return string(digest[:])
}

func singleValue(values []string) string {
	if len(values) != 1 {
		return ""
	}
	return strings.TrimSpace(values[0])
}

func isDetailedMetadataRequest(r *http.Request) bool {
	if r == nil || r.URL == nil || (r.Method != http.MethodGet && r.Method != http.MethodHead) {
		return false
	}
	const prefix = "/library/metadata/"
	identifier := strings.TrimPrefix(r.URL.Path, prefix)
	if identifier == r.URL.Path || identifier == "" {
		return false
	}
	for _, character := range identifier {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}
