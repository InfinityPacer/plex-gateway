package mediainfo

import (
	"context"
	"crypto/sha256"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	defaultRecordTTL          = 30 * 24 * time.Hour
	defaultRecordRetention    = 180 * 24 * time.Hour
	defaultNegativeTTL        = 15 * time.Minute
	defaultPlaybackQueueSize  = 16
	defaultNeighborQueueSize  = 50
	defaultMetadataQueueSize  = 50
	defaultPendingTTL         = 5 * time.Minute
	defaultBackgroundInterval = 5 * time.Second
	defaultWorkerCount        = 1
	defaultNegativeLimit      = 4096
	defaultRetryLimit         = 256
	defaultTouchQueueSize     = 1024
	defaultGCInterval         = 24 * time.Hour
	defaultStoreTimeout       = 5 * time.Second
	defaultTouchInterval      = time.Hour
	maxClientUserAgentBytes   = 4 << 10
)

var (
	ErrServiceUnavailable    = errors.New("MediaInfo service is unavailable")
	ErrQueueFull             = errors.New("MediaInfo queue is full")
	ErrRetryRegistrationFull = errors.New("MediaInfo retry registration is full")
	ErrNegativeCache         = errors.New("MediaInfo probe is in backoff")
	ErrCacheResetInProgress  = errors.New("MediaInfo cache reset is in progress")
	ErrPendingExpired        = errors.New("MediaInfo pending request expired")
	ErrSuperseded            = errors.New("MediaInfo request was superseded")
)

// Priority orders MediaInfo work by user intent. P0 playback work can bypass
// the shared background start interval; P1 and P2 are bounded background work.
type Priority uint8

const (
	PriorityPlayback Priority = iota
	PriorityNeighbor
	PriorityMetadata
)

const (
	// Deprecated: use PriorityPlayback.
	PriorityInteractive = PriorityPlayback
	// Deprecated: use PriorityNeighbor.
	PriorityBackground = PriorityNeighbor
)

// Request describes one exact STRM control target. Target is used only for the
// active job and is never persisted in the MediaInfo record.
type Request struct {
	Key             Key
	RatingKey       string
	Target          string
	Priority        Priority
	ClientUserAgent string
}

// SubmitDisposition explains how a non-blocking Submit request was handled.
// A joined request may be retried later with its own User-Agent when the
// shared flight fails.
type SubmitDisposition string

const (
	SubmitFreshCache           SubmitDisposition = "fresh_cache"
	SubmitJoinedExistingFlight SubmitDisposition = "joined_existing_flight"
	SubmitNewlyQueued          SubmitDisposition = "newly_queued"
	SubmitRejected             SubmitDisposition = "rejected"
)

// SubmitResult is the detailed outcome of SubmitDetailed. Err is non-nil only
// for a rejected request; accepted cache hits, joins, and queue admissions
// have a nil Err.
type SubmitResult struct {
	Disposition SubmitDisposition
	Err         error
}

func (request Request) validate() error {
	if err := request.Key.Validate(); err != nil {
		return err
	}
	if request.Priority > PriorityMetadata {
		return errors.New("MediaInfo request priority is invalid")
	}
	if request.Target == "" {
		return errors.New("STRM target is required")
	}
	if strings.ContainsAny(request.ClientUserAgent, "\r\n") {
		return errors.New("client User-Agent is invalid")
	}
	if len(request.ClientUserAgent) > maxClientUserAgentBytes {
		return errors.New("client User-Agent is too large")
	}
	return nil
}

// RecordStore is the successful-result persistence boundary.
type RecordStore interface {
	Put(context.Context, Record) error
	Get(context.Context, Key) (Record, bool, error)
	Touch(context.Context, Key, time.Time, time.Time) error
}

// ResetResult reports the durable effect of one hot cache reset.
type ResetResult struct {
	DeletedRecords int64
	BackupPath     string
}

type recordResetStore interface {
	BackupAndDeleteAll(context.Context, string, time.Time) (ResetResult, error)
}

// RecordJanitor removes records after their retention window. It is separate
// from request-time storage so alternate stores need not expose SQLite details.
type RecordJanitor interface {
	DeleteUnretained(context.Context, time.Time) (int64, error)
}

// Prober produces normalized technical metadata for one resolved media URL.
type Prober interface {
	Probe(context.Context, string, string) (Media, error)
}

// ServiceMetrics receives bounded, label-free analysis events.
type ServiceMetrics interface {
	IncMediaInfoCacheHits()
	IncMediaInfoCacheMisses()
	IncMediaInfoProbeOffered(uint8)
	IncMediaInfoProbeQueued(uint8)
	IncMediaInfoProbeJoined(uint8)
	IncMediaInfoProbePromoted(uint8)
	IncMediaInfoProbeDroppedFull(uint8)
	IncMediaInfoProbeDroppedExpired(uint8)
	IncMediaInfoProbeSuperseded(uint8)
	IncMediaInfoProbeSuccess()
	IncMediaInfoProbeFailure()
	IncMediaInfoStoreFailure()
	IncMediaInfoProbeActive()
	DecMediaInfoProbeActive()
	ObserveMediaInfoProbeLatency(time.Duration)
}

// ServiceOptions owns the background analysis lifecycle. Resolver requests use
// only UserAgent and identity encoding; client credentials are never inherited.
type ServiceOptions struct {
	Cache        *Cache
	Store        RecordStore
	Janitor      RecordJanitor
	Provider     Provider
	PlexServerID string
	Logger       *slog.Logger
	Metrics      ServiceMetrics
	Concurrency  int
	// Deprecated: use PlaybackQueueSize.
	InteractiveQueueSize int
	// Deprecated: use NeighborQueueSize.
	BackgroundQueueSize int
	ProbeTimeout        time.Duration
	RecordTTL           time.Duration
	RecordRetention     time.Duration
	NegativeTTL         time.Duration
	PendingTTL          time.Duration
	BackgroundInterval  time.Duration
	PlaybackQueueSize   int
	NeighborQueueSize   int
	MetadataQueueSize   int
	BackgroundUserAgent string
	GCInterval          time.Duration
	TouchInterval       time.Duration
	Now                 func() time.Time
}

// Service schedules bounded MediaInfo work independently from client request
// cancellation. Request waiters may leave while the shared job finishes.
type Service struct {
	cache               *Cache
	store               RecordStore
	resetStore          recordResetStore
	janitor             RecordJanitor
	provider            Provider
	providerDescriptor  ProviderDescriptor
	plexServerID        string
	logger              *slog.Logger
	metrics             ServiceMetrics
	probeTimeout        time.Duration
	recordTTL           time.Duration
	recordRetention     time.Duration
	negativeTTL         time.Duration
	pendingTTL          time.Duration
	backgroundInterval  time.Duration
	negativeLimit       int
	backgroundUserAgent string
	gcInterval          time.Duration
	touchInterval       time.Duration
	now                 func() time.Time

	ctx        context.Context
	cancel     context.CancelFunc
	workers    sync.WaitGroup
	workerDone chan struct{}
	touches    chan recordTouch
	wake       chan struct{}

	mu                 sync.Mutex
	playback           []*job
	neighbor           []*job
	metadata           []*job
	retryPlayback      []*job
	retryNeighbor      []*job
	retryMetadata      []*job
	playbackLimit      int
	neighborLimit      int
	metadataLimit      int
	nextBackgroundAt   time.Time
	flights            map[string]*flight
	negative           map[string]time.Time
	retryRegistrations int
	retryLimit         int
	retrySequence      uint64
	closed             bool
	active             atomic.Int64
	generation         atomic.Uint64
	resetting          atomic.Bool
	resetMu            sync.RWMutex
}

type recordTouch struct {
	key         Key
	accessedAt  time.Time
	retainUntil time.Time
	reservation touchReservation
}

type flight struct {
	done         chan struct{}
	job          *job
	request      Request
	priority     Priority
	userAgent    string
	retryWaiters []retryRegistration
	retryDepth   uint8
	record       Record
	err          error
}

type retryRegistration struct {
	request      Request
	userAgentKey string
	sequence     uint64
}

type job struct {
	request      Request
	flight       *flight
	generation   uint64
	expiresAt    time.Time
	cacheChecked bool
	claimed      bool
}

// NewService starts the configured worker count after validating every
// required boundary. The caller owns the store lifecycle separately.
func NewService(options ServiceOptions) (*Service, error) {
	if options.Cache == nil || options.Store == nil || options.Provider == nil {
		return nil, ErrServiceUnavailable
	}
	concurrency := options.Concurrency
	if concurrency <= 0 {
		concurrency = defaultWorkerCount
	}
	playbackSize := options.PlaybackQueueSize
	if playbackSize <= 0 {
		playbackSize = options.InteractiveQueueSize
	}
	if playbackSize <= 0 {
		playbackSize = defaultPlaybackQueueSize
	}
	neighborSize := options.NeighborQueueSize
	if neighborSize <= 0 {
		neighborSize = options.BackgroundQueueSize
	}
	if neighborSize <= 0 {
		neighborSize = defaultNeighborQueueSize
	}
	metadataSize := options.MetadataQueueSize
	if metadataSize <= 0 {
		metadataSize = defaultMetadataQueueSize
	}
	pendingTTL := options.PendingTTL
	if pendingTTL <= 0 {
		pendingTTL = defaultPendingTTL
	}
	backgroundInterval := options.BackgroundInterval
	if backgroundInterval <= 0 {
		backgroundInterval = defaultBackgroundInterval
	}
	probeTimeout := options.ProbeTimeout
	if probeTimeout <= 0 {
		probeTimeout = defaultProbeTimeout
	}
	recordTTL := options.RecordTTL
	if recordTTL <= 0 {
		recordTTL = defaultRecordTTL
	}
	negativeTTL := options.NegativeTTL
	if negativeTTL <= 0 {
		negativeTTL = defaultNegativeTTL
	}
	recordRetention := options.RecordRetention
	if recordRetention <= 0 {
		recordRetention = defaultRecordRetention
	}
	if recordRetention < recordTTL {
		return nil, errors.New("MediaInfo record retention must not be shorter than freshness TTL")
	}
	backgroundUserAgent := strings.TrimSpace(options.BackgroundUserAgent)
	if backgroundUserAgent == "" || strings.ContainsAny(backgroundUserAgent, "\r\n") {
		return nil, errors.New("MediaInfo background User-Agent is invalid")
	}
	if len(backgroundUserAgent) > maxClientUserAgentBytes {
		return nil, errors.New("MediaInfo background User-Agent is too large")
	}
	gcInterval := options.GCInterval
	if gcInterval <= 0 {
		gcInterval = defaultGCInterval
	}
	touchInterval := options.TouchInterval
	if touchInterval <= 0 {
		touchInterval = defaultTouchInterval
	}
	now := options.Now
	if now == nil {
		now = time.Now
	}
	logger := options.Logger
	if logger == nil {
		logger = slog.Default()
	}
	providerDescriptor := options.Provider.Descriptor()
	if err := providerDescriptor.validate(); err != nil {
		return nil, err
	}
	plexServerID := strings.TrimSpace(options.PlexServerID)
	if plexServerID == "" || strings.ContainsAny(plexServerID, "\x00\r\n") {
		return nil, errors.New("MediaInfo Plex server identity is invalid")
	}
	ctx, cancel := context.WithCancel(context.Background())
	service := &Service{
		cache: options.Cache, store: options.Store, janitor: options.Janitor,
		provider: options.Provider, providerDescriptor: providerDescriptor, plexServerID: plexServerID,
		logger: logger, metrics: options.Metrics, probeTimeout: probeTimeout,
		recordTTL: recordTTL, recordRetention: recordRetention,
		negativeTTL: negativeTTL, pendingTTL: pendingTTL,
		backgroundInterval: backgroundInterval, negativeLimit: defaultNegativeLimit,
		retryLimit:          defaultRetryLimit,
		backgroundUserAgent: backgroundUserAgent, gcInterval: gcInterval,
		touchInterval: touchInterval, now: now,
		ctx: ctx, cancel: cancel, workerDone: make(chan struct{}),
		touches: make(chan recordTouch, defaultTouchQueueSize), wake: make(chan struct{}, 1),
		playbackLimit: playbackSize, neighborLimit: neighborSize, metadataLimit: metadataSize,
		flights: make(map[string]*flight), negative: make(map[string]time.Time),
	}
	service.resetStore, _ = options.Store.(recordResetStore)
	for index := 0; index < concurrency; index++ {
		service.workers.Add(1)
		go service.worker()
	}
	service.workers.Add(1)
	go service.touchWorker()
	if service.janitor != nil {
		service.workers.Add(1)
		go service.janitorWorker()
	}
	go func() {
		service.workers.Wait()
		close(service.workerDone)
	}()
	return service, nil
}

// Get returns one exact retained record from L1 or the durable store without
// scheduling analysis or revalidation. Cache-only consumers can therefore use
// known-good stale data without turning enumeration into remote probe work.
func (service *Service) Get(key Key) (Record, bool) {
	if service == nil {
		return Record{}, false
	}
	return service.GetContext(service.ctx, key)
}

// GetContext performs a cache-only lookup bounded by the caller's response
// deadline and never schedules remote analysis.
func (service *Service) GetContext(ctx context.Context, key Key) (Record, bool) {
	if service == nil {
		return Record{}, false
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if strings.TrimSpace(key.PlexServerID) == "" {
		key.PlexServerID = service.plexServerID
	}
	if key.PlexServerID != service.plexServerID {
		return Record{}, false
	}
	record, ok, _ := service.lookup(ctx, key)
	if ok {
		service.metric(func(metrics ServiceMetrics) { metrics.IncMediaInfoCacheHits() })
	} else {
		service.metric(func(metrics ServiceMetrics) { metrics.IncMediaInfoCacheMisses() })
	}
	return record, ok
}

// GetMemory returns one exact retained L1 record without durable storage I/O or
// remote work. Playback-policy callers use it to keep decision and Part paths
// independent from SQLite latency.
func (service *Service) GetMemory(key Key) (Record, bool) {
	if service == nil || service.resetting.Load() {
		return Record{}, false
	}
	if strings.TrimSpace(key.PlexServerID) == "" {
		key.PlexServerID = service.plexServerID
	}
	if key.PlexServerID != service.plexServerID {
		return Record{}, false
	}
	service.resetMu.RLock()
	defer service.resetMu.RUnlock()
	if service.resetting.Load() {
		return Record{}, false
	}
	now := service.now().UTC()
	record, found := service.cache.GetKnown(key, now)
	if !found || !service.compatible(record) {
		return Record{}, false
	}
	return service.touchCached(record, now), true
}

// Ensure returns a cached result or waits for a shared job until ctx ends. The
// worker uses the service context, so a client disconnect or five-second wait
// expiry does not cancel work needed by another client or a later request.
func (service *Service) Ensure(ctx context.Context, request Request) (Record, error) {
	if service == nil {
		return Record{}, ErrServiceUnavailable
	}
	if ctx == nil {
		ctx = context.Background()
	}
	var err error
	request, err = service.normalizeRequest(request)
	if err != nil {
		return Record{}, err
	}
	record, found, fresh := service.lookup(ctx, request.Key)
	if found {
		service.metric(func(metrics ServiceMetrics) { metrics.IncMediaInfoCacheHits() })
		if !fresh {
			revalidation := request
			revalidation.Priority = PriorityNeighbor
			_, _ = service.begin(revalidation)
		}
		return record, nil
	}
	service.metric(func(metrics ServiceMetrics) { metrics.IncMediaInfoCacheMisses() })
	retries := 0
	for {
		if err := ctx.Err(); err != nil {
			return Record{}, err
		}
		flight, err := service.begin(request)
		if err != nil {
			return Record{}, err
		}
		select {
		case <-ctx.Done():
			return Record{}, ctx.Err()
		case <-flight.done:
			if flight.err != nil && flight.userAgent != request.ClientUserAgent && retries == 0 {
				// Successful technical metadata is UA-independent and remains
				// shared. A failed provider attempt is UA-sensitive, so a waiter
				// with another client identity may make one bounded retry.
				retries++
				continue
			}
			return cloneRecord(flight.record), flight.err
		}
	}
}

// Submit schedules work without creating a request-bound waiter. It retains
// the original error-only contract; use SubmitDetailed when the admission
// disposition is needed.
func (service *Service) Submit(request Request) error {
	return service.SubmitDetailed(request).Err
}

// Offer attempts a best-effort admission using only in-memory state. It is
// safe for response paths because it never reads the durable store or waits
// for a worker; a worker performs the authoritative L2 check before probing.
func (service *Service) Offer(request Request) SubmitResult {
	if service == nil {
		return SubmitResult{Disposition: SubmitRejected, Err: ErrServiceUnavailable}
	}
	request, err := service.normalizeRequest(request)
	if err != nil {
		return SubmitResult{Disposition: SubmitRejected, Err: err}
	}
	if _, ok := service.memoryFresh(request.Key); ok {
		service.metric(func(metrics ServiceMetrics) { metrics.IncMediaInfoCacheHits() })
		return SubmitResult{Disposition: SubmitFreshCache}
	}
	_, disposition, err := service.beginDetailed(request, true)
	if err != nil {
		return SubmitResult{Disposition: SubmitRejected, Err: err}
	}
	return SubmitResult{Disposition: disposition}
}

// TrySubmit is the concise alias used by non-blocking callers.
func (service *Service) TrySubmit(request Request) SubmitResult {
	return service.Offer(request)
}

// SubmitDetailed schedules work and reports whether the request used a fresh
// cache record, joined an existing flight, was newly queued, or was rejected.
// A request that joins a different-User-Agent flight is registered once, with
// a bounded per-service budget, so a provider failure can hand the flight to
// that User-Agent without probing blindly with the same identity.
func (service *Service) SubmitDetailed(request Request) SubmitResult {
	if service == nil {
		return SubmitResult{Disposition: SubmitRejected, Err: ErrServiceUnavailable}
	}
	request, err := service.normalizeRequest(request)
	if err != nil {
		return SubmitResult{Disposition: SubmitRejected, Err: err}
	}
	_, found, fresh := service.lookup(service.ctx, request.Key)
	if found && fresh {
		return SubmitResult{Disposition: SubmitFreshCache}
	}
	_, disposition, err := service.beginDetailed(request, true)
	if err != nil {
		return SubmitResult{Disposition: SubmitRejected, Err: err}
	}
	return SubmitResult{Disposition: disposition}
}

func (service *Service) memoryFresh(key Key) (Record, bool) {
	if service == nil || service.resetting.Load() {
		return Record{}, false
	}
	service.resetMu.RLock()
	if service.resetting.Load() {
		service.resetMu.RUnlock()
		return Record{}, false
	}
	now := service.now().UTC()
	record, ok := service.cache.Get(key, now)
	service.resetMu.RUnlock()
	if !ok || !service.compatible(record) {
		return Record{}, false
	}
	return service.touchCached(record, now), true
}

func (service *Service) normalizeRequest(request Request) (Request, error) {
	if strings.TrimSpace(request.Key.PlexServerID) == "" {
		request.Key.PlexServerID = service.plexServerID
	}
	if request.Key.PlexServerID != service.plexServerID {
		return Request{}, errors.New("MediaInfo request belongs to another Plex server")
	}
	request.ClientUserAgent = strings.TrimSpace(request.ClientUserAgent)
	if request.ClientUserAgent == "" {
		request.ClientUserAgent = service.backgroundUserAgent
	}
	if err := request.validate(); err != nil {
		return Request{}, err
	}
	return request, nil
}

func (service *Service) lookup(ctx context.Context, key Key) (Record, bool, bool) {
	if service.resetting.Load() {
		return Record{}, false, false
	}
	service.resetMu.RLock()
	defer service.resetMu.RUnlock()
	if service.resetting.Load() {
		return Record{}, false, false
	}
	now := service.now().UTC()
	if record, ok := service.cache.GetKnown(key, now); ok && service.compatible(record) {
		record = service.touchCached(record, now)
		return record, true, record.Fresh(now)
	}
	record, found, err := service.store.Get(ctx, key)
	if err != nil {
		service.metric(func(metrics ServiceMetrics) { metrics.IncMediaInfoStoreFailure() })
		service.logger.Warn("mediainfo_store_read_failed", "part", key.PartID, "error_kind", "storage")
		return Record{}, false, false
	}
	if !found || !record.Retained(now) || !service.compatible(record) {
		return Record{}, false, false
	}
	service.cache.Put(record, now)
	record = service.touchCached(record, now)
	return record, true, record.Fresh(now)
}

func (service *Service) compatible(record Record) bool {
	return record.Provider == service.providerDescriptor.Name &&
		record.ProviderRevision == service.providerDescriptor.Revision
}

func (service *Service) touchCached(record Record, now time.Time) Record {
	retainUntil := now.Add(service.recordRetention)
	touched, ok := service.cache.Touch(record.Key, now, retainUntil)
	if !ok {
		return record
	}
	reservation, persist := service.cache.reserveTouch(record.Key, now, service.touchInterval)
	if !persist {
		return touched
	}
	service.mu.Lock()
	if service.closed {
		service.mu.Unlock()
		reservation.release()
		return touched
	}
	select {
	case service.touches <- recordTouch{
		key: record.Key, accessedAt: now, retainUntil: retainUntil, reservation: reservation,
	}:
	default:
		reservation.release()
	}
	service.mu.Unlock()
	return touched
}

func (service *Service) begin(request Request) (*flight, error) {
	flight, _, err := service.beginDetailed(request, false)
	return flight, err
}

func (service *Service) beginDetailed(request Request, registerRetry bool) (*flight, SubmitDisposition, error) {
	if service == nil {
		return nil, SubmitRejected, ErrServiceUnavailable
	}
	if service.resetting.Load() {
		return nil, SubmitRejected, ErrCacheResetInProgress
	}
	var err error
	request, err = service.normalizeRequest(request)
	if err != nil {
		return nil, SubmitRejected, err
	}
	service.metric(func(metrics ServiceMetrics) { metrics.IncMediaInfoProbeOffered(uint8(request.Priority)) })
	key := request.Key.cacheKey()
	negativeKey := service.negativeKey(request)
	now := service.now()
	service.mu.Lock()
	if service.closed {
		service.mu.Unlock()
		return nil, SubmitRejected, ErrServiceUnavailable
	}
	if until, exists := service.negative[negativeKey]; exists {
		if now.Before(until) {
			service.mu.Unlock()
			return nil, SubmitRejected, ErrNegativeCache
		}
		delete(service.negative, negativeKey)
	}
	if existing, exists := service.flights[key]; exists {
		accepted, promoted := service.updateExistingLocked(existing, request)
		if !accepted {
			service.mu.Unlock()
			service.metric(func(metrics ServiceMetrics) { metrics.IncMediaInfoProbeDroppedFull(uint8(request.Priority)) })
			return nil, SubmitRejected, ErrQueueFull
		}
		if registerRetry && existing.job.claimed && request.ClientUserAgent != existing.userAgent {
			if err := service.registerRetryLocked(existing, request); err != nil {
				service.mu.Unlock()
				return nil, SubmitRejected, err
			}
		}
		service.mu.Unlock()
		service.metric(func(metrics ServiceMetrics) { metrics.IncMediaInfoProbeJoined(uint8(request.Priority)) })
		if promoted {
			service.metric(func(metrics ServiceMetrics) { metrics.IncMediaInfoProbePromoted(uint8(request.Priority)) })
		}
		return existing, SubmitJoinedExistingFlight, nil
	}
	service.supersedeQueuedBackgroundLocked(request)
	flight := &flight{
		done: make(chan struct{}), request: request, priority: request.Priority,
		userAgent: request.ClientUserAgent,
	}
	queued := &job{
		request: request, flight: flight, generation: service.generation.Load(),
		expiresAt: service.pendingExpiry(request.Priority, now),
	}
	flight.job = queued
	if !service.enqueueLocked(queued) {
		service.mu.Unlock()
		service.metric(func(metrics ServiceMetrics) { metrics.IncMediaInfoProbeDroppedFull(uint8(request.Priority)) })
		return nil, SubmitRejected, ErrQueueFull
	}
	service.flights[key] = flight
	service.signalWakeLocked()
	service.mu.Unlock()
	service.metric(func(metrics ServiceMetrics) { metrics.IncMediaInfoProbeQueued(uint8(request.Priority)) })
	return flight, SubmitNewlyQueued, nil
}

func (service *Service) pendingExpiry(priority Priority, now time.Time) time.Time {
	if priority == PriorityPlayback {
		return time.Time{}
	}
	return now.Add(service.pendingTTL)
}

func (service *Service) updateExistingLocked(existing *flight, request Request) (bool, bool) {
	if existing == nil || existing.job == nil {
		return false, false
	}
	if request.Priority < existing.priority {
		if existing.job.claimed {
			existing.priority = request.Priority
			existing.request = request
			service.signalWakeLocked()
		} else if service.promoteLocked(existing.job, request) {
			existing.priority = request.Priority
			existing.request = request
		} else {
			return false, false
		}
		return true, true
	}
	if request.Priority != existing.priority {
		return true, false
	}
	// The latest same-priority playback/neighbor request carries the current
	// target and identity metadata. A queued job may also replace its transport
	// User-Agent because no provider call has started yet.
	existing.request = request
	if !existing.job.claimed {
		existing.job.request = request
		existing.userAgent = request.ClientUserAgent
	}
	return true, false
}

// registerRetryLocked records one distinct waiting User-Agent for an active
// flight. The caller must hold service.mu. Registration is intentionally
// bounded because User-Agent is untrusted request input.
func (service *Service) registerRetryLocked(existing *flight, request Request) error {
	if existing == nil || existing.job == nil || existing.retryDepth >= 1 ||
		request.Priority > PriorityNeighbor || request.Priority > existing.priority {
		return nil
	}
	userAgentKey := retryUserAgentKey(request.ClientUserAgent)
	for _, registration := range existing.retryWaiters {
		if registration.userAgentKey == userAgentKey {
			return nil
		}
	}
	if service.retryRegistrations >= service.retryLimit {
		return ErrRetryRegistrationFull
	}
	existing.retryWaiters = append(existing.retryWaiters, retryRegistration{
		request: request, userAgentKey: userAgentKey, sequence: service.retrySequence,
	})
	service.retrySequence++
	service.retryRegistrations++
	return nil
}

func (service *Service) enqueueLocked(queued *job) bool {
	if queued == nil {
		return false
	}
	switch queued.request.Priority {
	case PriorityPlayback:
		if service.playbackCountLocked() >= service.playbackLimit {
			return false
		}
		service.playback = append(service.playback, queued)
	case PriorityNeighbor:
		if service.neighborCountLocked() >= service.neighborLimit {
			return false
		}
		service.neighbor = append(service.neighbor, queued)
	case PriorityMetadata:
		if service.metadataCountLocked() >= service.metadataLimit {
			return false
		}
		service.metadata = append(service.metadata, queued)
	default:
		return false
	}
	return true
}

func (service *Service) promoteLocked(queued *job, request Request) bool {
	if queued == nil || queued.claimed || request.Priority >= queued.request.Priority {
		return false
	}
	if !service.priorityCapacityAvailableLocked(request.Priority) {
		return false
	}
	if !service.removeFromQueueLocked(queued) {
		return false
	}
	queued.request = request
	queued.expiresAt = service.pendingExpiry(request.Priority, service.now())
	queued.flight.userAgent = request.ClientUserAgent
	service.prependToQueueLocked(queued)
	service.signalWakeLocked()
	return true
}

func removeQueuedJob(queue *[]*job, target *job) bool {
	for index, candidate := range *queue {
		if candidate != target {
			continue
		}
		copy((*queue)[index:], (*queue)[index+1:])
		last := len(*queue) - 1
		(*queue)[last] = nil
		*queue = (*queue)[:last]
		return true
	}
	return false
}

func (service *Service) removeFromQueueLocked(target *job) bool {
	for _, queue := range service.queuedQueuesLocked() {
		if removeQueuedJob(queue, target) {
			return true
		}
	}
	return false
}

func (service *Service) prependToQueueLocked(queued *job) {
	switch queued.request.Priority {
	case PriorityPlayback:
		service.playback = append([]*job{queued}, service.playback...)
	case PriorityNeighbor:
		service.neighbor = append([]*job{queued}, service.neighbor...)
	case PriorityMetadata:
		service.metadata = append([]*job{queued}, service.metadata...)
	}
}

func (service *Service) queuedQueuesLocked() []*[]*job {
	return []*[]*job{
		&service.playback, &service.neighbor, &service.metadata,
		&service.retryPlayback, &service.retryNeighbor, &service.retryMetadata,
	}
}

func (service *Service) priorityCapacityAvailableLocked(priority Priority) bool {
	switch priority {
	case PriorityPlayback:
		return service.playbackCountLocked() < service.playbackLimit
	case PriorityNeighbor:
		return service.neighborCountLocked() < service.neighborLimit
	case PriorityMetadata:
		return service.metadataCountLocked() < service.metadataLimit
	default:
		return false
	}
}

func (service *Service) playbackCountLocked() int {
	return len(service.playback) + len(service.retryPlayback)
}

func (service *Service) neighborCountLocked() int {
	return len(service.neighbor) + len(service.retryNeighbor)
}

func (service *Service) metadataCountLocked() int {
	return len(service.metadata) + len(service.retryMetadata)
}

func (service *Service) supersedeQueuedBackgroundLocked(request Request) {
	for _, queue := range []*[]*job{
		&service.neighbor, &service.metadata, &service.retryNeighbor, &service.retryMetadata,
	} {
		kept := (*queue)[:0]
		for _, queued := range *queue {
			if queued.request.Key.PlexServerID == request.Key.PlexServerID &&
				queued.request.Key.PartID == request.Key.PartID &&
				queued.request.Key.STRMFingerprint != request.Key.STRMFingerprint {
				service.cancelQueuedLocked(queued, ErrSuperseded)
				continue
			}
			kept = append(kept, queued)
		}
		*queue = kept
	}
}

func (service *Service) cancelQueuedLocked(queued *job, err error) {
	if queued == nil || queued.claimed || queued.flight == nil {
		return
	}
	key := queued.request.Key.cacheKey()
	if current, ok := service.flights[key]; !ok || current != queued.flight {
		return
	}
	delete(service.flights, key)
	service.releaseRetryRegistrationsLocked(queued.flight)
	queued.flight.err = err
	close(queued.flight.done)
	switch {
	case errors.Is(err, ErrQueueFull):
		service.metric(func(metrics ServiceMetrics) { metrics.IncMediaInfoProbeDroppedFull(uint8(queued.request.Priority)) })
	case errors.Is(err, ErrPendingExpired):
		service.metric(func(metrics ServiceMetrics) { metrics.IncMediaInfoProbeDroppedExpired(uint8(queued.request.Priority)) })
	case errors.Is(err, ErrSuperseded):
		service.metric(func(metrics ServiceMetrics) { metrics.IncMediaInfoProbeSuperseded(uint8(queued.request.Priority)) })
	}
}

func (service *Service) signalWakeLocked() {
	select {
	case service.wake <- struct{}{}:
	default:
	}
}

func (service *Service) worker() {
	defer service.workers.Done()
	for {
		queued, ok := service.nextJob()
		if !ok {
			return
		}
		service.run(queued)
	}
}

func (service *Service) touchWorker() {
	defer service.workers.Done()
	for {
		select {
		case <-service.ctx.Done():
			return
		case touch := <-service.touches:
			ctx, cancel := context.WithTimeout(service.ctx, defaultStoreTimeout)
			err := service.store.Touch(ctx, touch.key, touch.accessedAt, touch.retainUntil)
			cancel()
			if err != nil {
				touch.reservation.release()
			}
			if err != nil && !errors.Is(err, context.Canceled) {
				service.metric(func(metrics ServiceMetrics) { metrics.IncMediaInfoStoreFailure() })
				service.logger.Warn("mediainfo_store_touch_failed", "part", touch.key.PartID, "error_kind", "storage")
			}
		}
	}
}

func (service *Service) janitorWorker() {
	defer service.workers.Done()
	ticker := time.NewTicker(service.gcInterval)
	defer ticker.Stop()
	for {
		select {
		case <-service.ctx.Done():
			return
		case now := <-ticker.C:
			ctx, cancel := context.WithTimeout(service.ctx, defaultStoreTimeout)
			_, err := service.janitor.DeleteUnretained(ctx, now.UTC())
			cancel()
			if err != nil && !errors.Is(err, context.Canceled) {
				service.metric(func(metrics ServiceMetrics) { metrics.IncMediaInfoStoreFailure() })
				service.logger.Warn("mediainfo_store_gc_failed", "error_kind", "storage")
			}
		}
	}
}

func (service *Service) nextJob() (*job, bool) {
	for {
		service.mu.Lock()
		if service.closed {
			service.mu.Unlock()
			return nil, false
		}
		service.expireQueuedLocked(service.now())
		if queued := popQueue(&service.playback, &service.retryPlayback); queued != nil {
			queued.claimed = true
			if service.playbackCountLocked() > 0 {
				service.signalWakeLocked()
			}
			service.mu.Unlock()
			return queued, true
		}
		backgroundQueued := service.backgroundJobsLocked()
		now := service.now()
		if backgroundQueued > 0 && (service.nextBackgroundAt.IsZero() || !now.Before(service.nextBackgroundAt)) {
			queued := service.popPreparedBackgroundLocked()
			if queued != nil {
				queued.claimed = true
				service.nextBackgroundAt = now.Add(service.backgroundInterval)
				service.mu.Unlock()
				return queued, true
			}
		}
		if queued := service.popUnpreparedBackgroundLocked(); queued != nil {
			queued.claimed = true
			if service.backgroundJobsLocked() > 0 {
				service.signalWakeLocked()
			}
			service.mu.Unlock()
			service.preflight(queued)
			continue
		}

		wait := time.Duration(-1)
		if backgroundQueued > 0 && !service.nextBackgroundAt.IsZero() {
			wait = service.nextBackgroundAt.Sub(now)
			if wait < 0 {
				wait = 0
			}
		}
		service.mu.Unlock()

		if wait < 0 {
			select {
			case <-service.wake:
				continue
			case <-service.ctx.Done():
				return nil, false
			}
		}
		timer := time.NewTimer(wait)
		select {
		case <-service.wake:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		case <-timer.C:
		case <-service.ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return nil, false
		}
	}
}

func (service *Service) queuedJobsLocked() int {
	return len(service.retryPlayback) + len(service.playback) +
		len(service.retryNeighbor) + len(service.neighbor) +
		len(service.retryMetadata) + len(service.metadata)
}

func (service *Service) backgroundJobsLocked() int {
	return len(service.retryNeighbor) + len(service.neighbor) +
		len(service.retryMetadata) + len(service.metadata)
}

func popQueue(primary, retry *[]*job) *job {
	if len(*primary) > 0 {
		queued := (*primary)[0]
		(*primary)[0] = nil
		*primary = (*primary)[1:]
		return queued
	}
	if len(*retry) > 0 {
		queued := (*retry)[0]
		(*retry)[0] = nil
		*retry = (*retry)[1:]
		return queued
	}
	return nil
}

func popUnprepared(queue *[]*job) *job {
	return popBackgroundByPreparation(queue, false)
}

func popPrepared(queue *[]*job) *job {
	return popBackgroundByPreparation(queue, true)
}

func popBackgroundByPreparation(queue *[]*job, prepared bool) *job {
	for index, queued := range *queue {
		if queued == nil || queued.cacheChecked != prepared {
			continue
		}
		copy((*queue)[index:], (*queue)[index+1:])
		last := len(*queue) - 1
		(*queue)[last] = nil
		*queue = (*queue)[:last]
		return queued
	}
	return nil
}

func (service *Service) popPreparedBackgroundLocked() *job {
	for _, queue := range []*[]*job{
		&service.neighbor, &service.retryNeighbor, &service.metadata, &service.retryMetadata,
	} {
		if queued := popPrepared(queue); queued != nil {
			return queued
		}
	}
	return nil
}

func (service *Service) popUnpreparedBackgroundLocked() *job {
	for _, queue := range []*[]*job{
		&service.neighbor, &service.retryNeighbor, &service.metadata, &service.retryMetadata,
	} {
		if queued := popUnprepared(queue); queued != nil {
			return queued
		}
	}
	return nil
}

func (service *Service) enqueueRetryLocked(queued *job) bool {
	if queued == nil {
		return false
	}
	switch queued.request.Priority {
	case PriorityPlayback:
		if service.playbackCountLocked() >= service.playbackLimit {
			return false
		}
		service.retryPlayback = append(service.retryPlayback, queued)
	case PriorityNeighbor:
		if service.neighborCountLocked() >= service.neighborLimit {
			return false
		}
		service.retryNeighbor = append(service.retryNeighbor, queued)
	case PriorityMetadata:
		if service.metadataCountLocked() >= service.metadataLimit {
			return false
		}
		service.retryMetadata = append(service.retryMetadata, queued)
	default:
		return false
	}
	return true
}

// preflight completes fresh L2 hits before the remote-start limiter. A durable
// miss is returned to its priority queue without extending its pending TTL.
func (service *Service) preflight(queued *job) {
	if !queued.expiresAt.IsZero() && !service.now().Before(queued.expiresAt) {
		service.finish(queued, Record{}, ErrPendingExpired)
		return
	}
	if record, found, fresh := service.lookup(service.ctx, queued.request.Key); found && fresh {
		service.finishCached(queued, service.recordForFlight(queued, record))
		return
	}

	service.mu.Lock()
	key := queued.request.Key.cacheKey()
	flight := service.flights[key]
	if service.closed || flight != queued.flight {
		service.mu.Unlock()
		return
	}
	queued.claimed = false
	queued.cacheChecked = true
	queued.request = flight.request
	queued.request.Priority = flight.priority
	flight.userAgent = queued.request.ClientUserAgent
	if flight.priority == PriorityPlayback {
		queued.expiresAt = time.Time{}
	}
	if !service.priorityCapacityAvailableLocked(queued.request.Priority) {
		service.cancelQueuedLocked(queued, ErrQueueFull)
		service.mu.Unlock()
		return
	}
	service.prependToQueueLocked(queued)
	service.signalWakeLocked()
	service.mu.Unlock()
}

func (service *Service) expireQueuedLocked(now time.Time) {
	for _, queue := range []*[]*job{
		&service.playback, &service.neighbor, &service.metadata,
		&service.retryPlayback, &service.retryNeighbor, &service.retryMetadata,
	} {
		kept := (*queue)[:0]
		for _, queued := range *queue {
			if queued != nil && !queued.expiresAt.IsZero() && !now.Before(queued.expiresAt) {
				service.cancelQueuedLocked(queued, ErrPendingExpired)
				continue
			}
			kept = append(kept, queued)
		}
		*queue = kept
	}
}

func (service *Service) run(queued *job) {
	started := service.now()
	service.active.Add(1)
	service.metric(func(metrics ServiceMetrics) { metrics.IncMediaInfoProbeActive() })
	defer func() {
		service.active.Add(-1)
		service.metric(func(metrics ServiceMetrics) { metrics.DecMediaInfoProbeActive() })
		service.metric(func(metrics ServiceMetrics) { metrics.ObserveMediaInfoProbeLatency(service.now().Sub(started)) })
	}()
	if !queued.expiresAt.IsZero() && !service.now().Before(queued.expiresAt) {
		service.finish(queued, Record{}, ErrPendingExpired)
		return
	}
	// P0 reaches the worker without preflight and checks both cache tiers. A
	// preflighted background miss only rechecks L1 so remote work does not repeat
	// the same SQLite read.
	var record Record
	var found, fresh bool
	if queued.cacheChecked {
		record, found = service.memoryFresh(queued.request.Key)
		fresh = found
	} else {
		record, found, fresh = service.lookup(service.ctx, queued.request.Key)
	}
	if found && fresh {
		service.finishCached(queued, service.recordForFlight(queued, record))
		return
	}

	probeCtx, cancel := context.WithTimeout(service.ctx, service.probeTimeout)
	defer cancel()
	userAgent := strings.TrimSpace(queued.request.ClientUserAgent)
	if userAgent == "" {
		userAgent = service.backgroundUserAgent
	}
	result, err := service.provider.Probe(probeCtx, ProviderRequest{
		Target: queued.request.Target, UserAgent: userAgent,
	})
	if err != nil {
		service.finish(queued, Record{}, err)
		return
	}
	// A provider can return a complete result while an optional best-effort
	// enrichment consumes the final instant of the probe deadline. Preserve that
	// result unless the service lifecycle itself has already been canceled.
	if err := service.ctx.Err(); err != nil {
		service.finish(queued, Record{}, err)
		return
	}
	if !result.Media.Complete {
		service.finish(queued, Record{}, ErrProbeIncomplete)
		return
	}
	now := service.now().UTC()
	request := service.requestForFlight(queued)
	record = Record{
		Key: queued.request.Key, RatingKey: request.RatingKey,
		Provider: service.providerDescriptor.Name, ProviderRevision: service.providerDescriptor.Revision,
		ContentFingerprint: result.ContentFingerprint, SchemaVersion: SchemaVersion,
		Media: result.Media, ProbedAt: now, ExpiresAt: now.Add(service.recordTTL),
		LastAccessedAt: now, RetainUntil: now.Add(service.recordRetention),
	}
	if !record.Retained(now) {
		service.finish(queued, Record{}, ErrProbeIncomplete)
		return
	}
	service.resetMu.RLock()
	if queued.generation != service.generation.Load() || service.resetting.Load() {
		service.resetMu.RUnlock()
		service.finish(queued, Record{}, ErrCacheResetInProgress)
		return
	}
	storeCtx, cancelStore := context.WithTimeout(service.ctx, defaultStoreTimeout)
	if err := service.store.Put(storeCtx, record); err != nil {
		service.metric(func(metrics ServiceMetrics) { metrics.IncMediaInfoStoreFailure() })
		service.logger.Warn("mediainfo_store_failed", "part", queued.request.Key.PartID, "error_kind", "storage")
	}
	cancelStore()
	service.cache.Put(record, now)
	service.resetMu.RUnlock()
	service.finish(queued, record, nil)
}

func (service *Service) requestForFlight(queued *job) Request {
	service.mu.Lock()
	defer service.mu.Unlock()
	if queued != nil && queued.flight != nil && queued.flight.request.Key.cacheKey() == queued.request.Key.cacheKey() {
		return queued.flight.request
	}
	if queued == nil {
		return Request{}
	}
	return queued.request
}

func (service *Service) recordForFlight(queued *job, record Record) Record {
	request := service.requestForFlight(queued)
	if request.RatingKey != "" {
		record.RatingKey = request.RatingKey
	}
	return record
}

func (service *Service) finishCached(queued *job, record Record) {
	key := queued.request.Key.cacheKey()
	service.mu.Lock()
	flight := service.flights[key]
	if flight != queued.flight {
		service.mu.Unlock()
		return
	}
	delete(service.flights, key)
	service.releaseRetryRegistrationsLocked(flight)
	flight.record = record
	close(flight.done)
	service.mu.Unlock()
}

func (service *Service) finish(queued *job, record Record, err error) {
	key := queued.request.Key.cacheKey()
	var retry *flight
	service.mu.Lock()
	flight := service.flights[key]
	if flight != queued.flight {
		service.mu.Unlock()
		return
	}
	delete(service.flights, key)
	if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, ErrPendingExpired) && !errors.Is(err, ErrSuperseded) {
		now := service.now()
		service.rememberNegativeLocked(service.negativeKey(queued.request), now)
		retry = service.scheduleRetryLocked(flight, flight.userAgent, now)
	} else {
		service.releaseRetryRegistrationsLocked(flight)
	}
	flight.record = record
	flight.err = err
	close(flight.done)
	service.mu.Unlock()
	if retry != nil {
		service.metric(func(metrics ServiceMetrics) { metrics.IncMediaInfoProbeQueued(uint8(retry.priority)) })
	}
	if errors.Is(err, ErrPendingExpired) {
		service.metric(func(metrics ServiceMetrics) { metrics.IncMediaInfoProbeDroppedExpired(uint8(queued.request.Priority)) })
		return
	}
	if err != nil {
		service.metric(func(metrics ServiceMetrics) { metrics.IncMediaInfoProbeFailure() })
		service.logger.Info("mediainfo_probe_failed", "part", queued.request.Key.PartID, "error_kind", probeErrorKind(err))
		return
	}
	service.metric(func(metrics ServiceMetrics) { metrics.IncMediaInfoProbeSuccess() })
}

// scheduleRetryLocked selects at most one highest-priority, most-recent
// User-Agent registration. The caller must hold service.mu. A failed
// User-Agent is already in negative backoff and cannot be retried immediately.
func (service *Service) scheduleRetryLocked(failed *flight, failedUserAgent string, now time.Time) *flight {
	if failed.retryDepth >= 1 {
		service.releaseRetryRegistrationsLocked(failed)
		return nil
	}
	var selected *retryRegistration
	for index := range failed.retryWaiters {
		registration := failed.retryWaiters[index]
		service.retryRegistrations--
		if registration.request.ClientUserAgent == failedUserAgent ||
			registration.request.Priority > PriorityNeighbor || service.negativeActiveLocked(registration.request, now) {
			continue
		}
		if selected == nil || registration.request.Priority < selected.request.Priority ||
			(registration.request.Priority == selected.request.Priority && registration.sequence > selected.sequence) {
			candidate := registration
			selected = &candidate
		}
	}
	failed.retryWaiters = nil
	if selected == nil {
		return nil
	}

	next := &flight{
		done: make(chan struct{}), request: selected.request,
		priority: selected.request.Priority, userAgent: selected.request.ClientUserAgent,
		retryDepth: failed.retryDepth + 1,
	}
	next.job = &job{
		request: selected.request, flight: next, generation: failed.job.generation,
		expiresAt: service.pendingExpiry(selected.request.Priority, now), cacheChecked: true,
	}
	next.job.flight = next
	if !service.enqueueRetryLocked(next.job) {
		service.metric(func(metrics ServiceMetrics) { metrics.IncMediaInfoProbeDroppedFull(uint8(next.priority)) })
		return nil
	}
	service.flights[next.job.request.Key.cacheKey()] = next
	service.signalWakeLocked()
	return next
}

func (service *Service) releaseRetryRegistrationsLocked(flight *flight) {
	service.retryRegistrations -= len(flight.retryWaiters)
	if service.retryRegistrations < 0 {
		service.retryRegistrations = 0
	}
	flight.retryWaiters = nil
}

func (service *Service) negativeActiveLocked(request Request, now time.Time) bool {
	key := service.negativeKey(request)
	until, exists := service.negative[key]
	if !exists {
		return false
	}
	if now.Before(until) {
		return true
	}
	delete(service.negative, key)
	return false
}

func (service *Service) negativeKey(request Request) string {
	digest := userAgentDigest(request.ClientUserAgent)
	return request.Key.cacheKey() + "\x00" + string(digest[:])
}

func retryUserAgentKey(userAgent string) string {
	digest := userAgentDigest(userAgent)
	return string(digest[:])
}

func userAgentDigest(userAgent string) [sha256.Size]byte {
	return sha256.Sum256([]byte(userAgent))
}

func (service *Service) rememberNegativeLocked(key string, now time.Time) {
	for candidate, until := range service.negative {
		if !now.Before(until) {
			delete(service.negative, candidate)
		}
	}
	if len(service.negative) >= service.negativeLimit {
		var earliestKey string
		var earliest time.Time
		for candidate, until := range service.negative {
			if earliestKey == "" || until.Before(earliest) {
				earliestKey = candidate
				earliest = until
			}
		}
		delete(service.negative, earliestKey)
	}
	service.negative[key] = now.Add(service.negativeTTL)
}

// Status is a credential-free operational snapshot.
type Status struct {
	Available         bool  `json:"available"`
	CacheEntries      int   `json:"cache_entries"`
	ActiveProbes      int64 `json:"active_probes"`
	PlaybackQueued    int   `json:"playback_queued"`
	NeighborQueued    int   `json:"neighbor_queued"`
	MetadataQueued    int   `json:"metadata_queued"`
	InteractiveQueued int   `json:"interactive_queued"`
	BackgroundQueued  int   `json:"background_queued"`
	NegativeEntries   int   `json:"negative_entries"`
}

// Status returns bounded queue and cache counts without media identities.
func (service *Service) Status() Status {
	if service == nil {
		return Status{}
	}
	service.mu.Lock()
	available := !service.closed
	status := Status{
		Available: available, CacheEntries: service.cache.Len(), ActiveProbes: service.active.Load(),
		PlaybackQueued:    service.playbackCountLocked(),
		NeighborQueued:    service.neighborCountLocked(),
		MetadataQueued:    service.metadataCountLocked(),
		InteractiveQueued: service.playbackCountLocked(),
		BackgroundQueued:  service.neighborCountLocked() + service.metadataCountLocked(),
		NegativeEntries:   len(service.negative),
	}
	service.mu.Unlock()
	return status
}

// ResetCache hot-clears rebuildable MediaInfo state while the proxy remains
// available. Requests arriving after the reset may populate new records.
func (service *Service) ResetCache(ctx context.Context, backupDir string) (ResetResult, error) {
	if service == nil || service.resetStore == nil {
		return ResetResult{}, ErrServiceUnavailable
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if !service.resetting.CompareAndSwap(false, true) {
		return ResetResult{}, ErrCacheResetInProgress
	}
	defer service.resetting.Store(false)
	service.generation.Add(1)

	service.mu.Lock()
	if service.closed {
		service.mu.Unlock()
		return ResetResult{}, ErrServiceUnavailable
	}
	for key, flight := range service.flights {
		delete(service.flights, key)
		flight.err = ErrCacheResetInProgress
		service.releaseRetryRegistrationsLocked(flight)
		close(flight.done)
	}
	service.playback = nil
	service.neighbor = nil
	service.metadata = nil
	service.retryPlayback = nil
	service.retryNeighbor = nil
	service.retryMetadata = nil
	service.negative = make(map[string]time.Time)
	service.nextBackgroundAt = time.Time{}
	for {
		select {
		case touch := <-service.touches:
			touch.reservation.release()
		default:
			service.signalWakeLocked()
			service.mu.Unlock()
			goto queuesCleared
		}
	}

queuesCleared:
	service.resetMu.Lock()
	defer service.resetMu.Unlock()
	result, err := service.resetStore.BackupAndDeleteAll(ctx, backupDir, service.now().UTC())
	if err != nil {
		return ResetResult{}, err
	}
	service.cache.Purge()
	return result, nil
}

// Close cancels active processes and waits for workers without closing the
// caller-owned persistent store.
func (service *Service) Close(ctx context.Context) error {
	if service == nil {
		return nil
	}
	service.mu.Lock()
	if !service.closed {
		service.closed = true
		service.cancel()
		for {
			select {
			case touch := <-service.touches:
				touch.reservation.release()
			default:
				goto touchesReleased
			}
		}
	touchesReleased:
		for key, flight := range service.flights {
			delete(service.flights, key)
			flight.err = ErrServiceUnavailable
			service.releaseRetryRegistrationsLocked(flight)
			close(flight.done)
		}
		service.playback = nil
		service.neighbor = nil
		service.metadata = nil
		service.retryPlayback = nil
		service.retryNeighbor = nil
		service.retryMetadata = nil
		service.signalWakeLocked()
	}
	service.mu.Unlock()
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-service.workerDone:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (service *Service) metric(call func(ServiceMetrics)) {
	if service != nil && service.metrics != nil {
		call(service.metrics)
	}
}
