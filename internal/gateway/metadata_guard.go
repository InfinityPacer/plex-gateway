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
	defaultMetadataGlobalConcurrency    = 16
	defaultMetadataPerClientConcurrency = 16
	defaultMetadataBatchConcurrency     = 4
	defaultMetadataQueueTimeout         = 10 * time.Second
)

// MetadataGuardOptions controls independent admission pools for single-item
// and comma-separated batch metadata reads. The guard does not cover playback,
// timeline, scrobble, mutations, or library listing paths.
type MetadataGuardOptions struct {
	Enabled              bool
	GlobalConcurrency    int
	PerClientConcurrency int
	BatchEnabled         bool
	BatchConcurrency     int
	QueueTimeout         time.Duration
}

type metadataGuard struct {
	next         http.Handler
	logger       *slog.Logger
	metrics      *metrics.Metrics
	global       chan struct{}
	batch        chan struct{}
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
	if !options.Enabled && !options.BatchEnabled {
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
	if options.BatchConcurrency <= 0 {
		options.BatchConcurrency = defaultMetadataBatchConcurrency
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
	guard := &metadataGuard{
		next:         next,
		logger:       logger,
		metrics:      registry,
		perClientMax: options.PerClientConcurrency,
		queueTimeout: options.QueueTimeout,
		clients:      make(map[string]*metadataClientBucket),
	}
	if options.Enabled {
		guard.global = make(chan struct{}, options.GlobalConcurrency)
	}
	if options.BatchEnabled {
		guard.batch = make(chan struct{}, options.BatchConcurrency)
	}
	return guard
}

func (g *metadataGuard) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	kind := classifyMetadataRequest(r)
	if kind == metadataRequestNone || (kind == metadataRequestSingle && g.global == nil) || (kind == metadataRequestBatch && g.batch == nil) {
		g.next.ServeHTTP(w, r)
		return
	}

	queuedAt := time.Now()
	ctx, cancel := context.WithTimeout(r.Context(), g.queueTimeout)
	defer cancel()
	var release func()
	var err error
	switch kind {
	case metadataRequestSingle:
		release, err = g.acquireSingle(ctx, metadataClientKey(r))
	case metadataRequestBatch:
		release, err = g.acquireBatch(ctx)
	}
	if err != nil {
		if r.Context().Err() != nil {
			return
		}
		if kind == metadataRequestBatch {
			g.metrics.IncMetadataBatchGuardTimeouts()
		} else {
			g.metrics.IncMetadataGuardTimeouts()
		}
		g.logger.Warn("metadata_guard_timeout", "method", r.Method, "request_kind", kind.String())
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Retry-After", "1")
		http.Error(w, http.StatusText(http.StatusTooManyRequests), http.StatusTooManyRequests)
		return
	}
	defer release()
	timing, _ := r.Context().Value(metadataCoalesceTimingContextKey{}).(*metadataCoalesceTiming)
	if kind == metadataRequestBatch && timing != nil {
		timing.guardWait = time.Since(queuedAt)
		plexStarted := time.Now()
		g.next.ServeHTTP(w, r)
		timing.plex = time.Since(plexStarted)
		timing.guardSeen = true
		return
	}
	g.next.ServeHTTP(w, r)
}

func (g *metadataGuard) acquireSingle(ctx context.Context, clientKey string) (func(), error) {
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

func (g *metadataGuard) acquireBatch(ctx context.Context) (func(), error) {
	g.metrics.IncMetadataBatchGuardQueued()
	defer g.metrics.DecMetadataBatchGuardQueued()

	select {
	case g.batch <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	g.metrics.IncMetadataBatchGuardAdmitted()
	g.metrics.IncMetadataBatchGuardActive()
	var once sync.Once
	return func() {
		once.Do(func() {
			<-g.batch
			g.metrics.DecMetadataBatchGuardActive()
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

type metadataRequestKind uint8

const (
	metadataRequestNone metadataRequestKind = iota
	metadataRequestSingle
	metadataRequestBatch
)

func (kind metadataRequestKind) String() string {
	if kind == metadataRequestBatch {
		return "batch"
	}
	return "single"
}

func classifyMetadataRequest(r *http.Request) metadataRequestKind {
	if r == nil || r.URL == nil || (r.Method != http.MethodGet && r.Method != http.MethodHead) {
		return metadataRequestNone
	}
	const prefix = "/library/metadata/"
	identifier := strings.TrimPrefix(r.URL.Path, prefix)
	if identifier == r.URL.Path || identifier == "" {
		return metadataRequestNone
	}
	identifiers := strings.Split(identifier, ",")
	for _, value := range identifiers {
		if value == "" {
			return metadataRequestNone
		}
		for _, character := range value {
			if character < '0' || character > '9' {
				return metadataRequestNone
			}
		}
	}
	if len(identifiers) == 1 {
		return metadataRequestSingle
	}
	return metadataRequestBatch
}
