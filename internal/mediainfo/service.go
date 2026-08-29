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
	defaultRecordTTL        = 30 * 24 * time.Hour
	defaultRecordRetention  = 180 * 24 * time.Hour
	defaultNegativeTTL      = 15 * time.Minute
	defaultWorkerQueueSize  = 256
	defaultWorkerCount      = 1
	defaultNegativeLimit    = 4096
	defaultRetryLimit       = 256
	defaultTouchQueueSize   = 1024
	defaultGCInterval       = 24 * time.Hour
	defaultStoreTimeout     = 5 * time.Second
	defaultTouchInterval    = time.Hour
	maxClientUserAgentBytes = 4 << 10
)

var (
	ErrServiceUnavailable    = errors.New("MediaInfo service is unavailable")
	ErrQueueFull             = errors.New("MediaInfo queue is full")
	ErrRetryRegistrationFull = errors.New("MediaInfo retry registration is full")
	ErrNegativeCache         = errors.New("MediaInfo probe is in backoff")
	ErrCacheResetInProgress  = errors.New("MediaInfo cache reset is in progress")
)

// Priority separates interactive misses from administrator prewarming. A
// playback-related metadata request must not wait behind season or root scans.
type Priority uint8

const (
	PriorityBackground Priority = iota
	PriorityInteractive
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
	IncMediaInfoProbeQueued()
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
	Cache                *Cache
	Store                RecordStore
	Janitor              RecordJanitor
	Provider             Provider
	PlexServerID         string
	Logger               *slog.Logger
	Metrics              ServiceMetrics
	Concurrency          int
	InteractiveQueueSize int
	BackgroundQueueSize  int
	ProbeTimeout         time.Duration
	RecordTTL            time.Duration
	RecordRetention      time.Duration
	NegativeTTL          time.Duration
	BackgroundUserAgent  string
	GCInterval           time.Duration
	TouchInterval        time.Duration
	Now                  func() time.Time
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

	mu                 sync.Mutex
	condition          *sync.Cond
	interactive        []*job
	background         []*job
	retryInteractive   []*job
	retryBackground    []*job
	interactiveLimit   int
	backgroundLimit    int
	flights            map[string]*flight
	negative           map[string]time.Time
	retryRegistrations int
	retryLimit         int
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
	userAgent    string
	retryWaiters []retryRegistration
	record       Record
	err          error
}

type retryRegistration struct {
	request      Request
	userAgentKey string
}

type job struct {
	request    Request
	flight     *flight
	generation uint64
	claimed    bool
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
	interactiveSize := options.InteractiveQueueSize
	if interactiveSize <= 0 {
		interactiveSize = defaultWorkerQueueSize
	}
	backgroundSize := options.BackgroundQueueSize
	if backgroundSize <= 0 {
		backgroundSize = defaultWorkerQueueSize
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
		negativeTTL: negativeTTL, negativeLimit: defaultNegativeLimit,
		retryLimit:          defaultRetryLimit,
		backgroundUserAgent: backgroundUserAgent, gcInterval: gcInterval,
		touchInterval: touchInterval, now: now,
		ctx: ctx, cancel: cancel, workerDone: make(chan struct{}),
		touches:          make(chan recordTouch, defaultTouchQueueSize),
		interactiveLimit: interactiveSize, backgroundLimit: backgroundSize,
		flights: make(map[string]*flight), negative: make(map[string]time.Time),
	}
	service.resetStore, _ = options.Store.(recordResetStore)
	service.condition = sync.NewCond(&service.mu)
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
			revalidation.Priority = PriorityBackground
			_, _ = service.begin(revalidation)
		}
		return record, nil
	}
	service.metric(func(metrics ServiceMetrics) { metrics.IncMediaInfoCacheMisses() })
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
			if flight.err != nil && flight.userAgent != request.ClientUserAgent {
				// Successful technical metadata is UA-independent and remains
				// shared. A failed provider attempt is UA-sensitive, so a waiter
				// with another client identity may retry within its own deadline.
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
	if found {
		request.Priority = PriorityBackground
	}
	_, disposition, err := service.beginDetailed(request, true)
	if err != nil {
		return SubmitResult{Disposition: SubmitRejected, Err: err}
	}
	return SubmitResult{Disposition: disposition}
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
		if request.Priority == PriorityInteractive {
			service.promoteLocked(existing.job)
		}
		if registerRetry && request.ClientUserAgent != existing.userAgent {
			if err := service.registerRetryLocked(existing, request); err != nil {
				service.mu.Unlock()
				return nil, SubmitRejected, err
			}
		}
		service.mu.Unlock()
		return existing, SubmitJoinedExistingFlight, nil
	}
	flight := &flight{done: make(chan struct{}), userAgent: request.ClientUserAgent}
	queued := &job{request: request, flight: flight, generation: service.generation.Load()}
	flight.job = queued
	if !service.enqueueLocked(queued) {
		service.mu.Unlock()
		return nil, SubmitRejected, ErrQueueFull
	}
	service.flights[key] = flight
	service.condition.Signal()
	service.mu.Unlock()
	service.metric(func(metrics ServiceMetrics) { metrics.IncMediaInfoProbeQueued() })
	return flight, SubmitNewlyQueued, nil
}

// registerRetryLocked records one distinct waiting User-Agent for an active
// flight. The caller must hold service.mu. Registration is intentionally
// bounded because User-Agent is untrusted request input.
func (service *Service) registerRetryLocked(existing *flight, request Request) error {
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
		request: request, userAgentKey: userAgentKey,
	})
	service.retryRegistrations++
	return nil
}

func (service *Service) enqueueLocked(queued *job) bool {
	if queued.request.Priority == PriorityInteractive {
		if len(service.interactive) >= service.interactiveLimit {
			return false
		}
		service.interactive = append(service.interactive, queued)
		return true
	}
	if len(service.background) >= service.backgroundLimit {
		return false
	}
	service.background = append(service.background, queued)
	return true
}

func (service *Service) promoteLocked(queued *job) {
	if queued == nil || queued.claimed || queued.request.Priority == PriorityInteractive {
		return
	}
	if removeQueuedJob(&service.retryBackground, queued) {
		queued.request.Priority = PriorityInteractive
		service.retryInteractive = append(service.retryInteractive, queued)
		service.condition.Signal()
		return
	}
	if len(service.interactive) >= service.interactiveLimit {
		return
	}
	if !removeQueuedJob(&service.background, queued) {
		return
	}
	queued.request.Priority = PriorityInteractive
	service.interactive = append(service.interactive, queued)
	service.condition.Signal()
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
	service.mu.Lock()
	defer service.mu.Unlock()
	for !service.closed && service.queuedJobsLocked() == 0 {
		service.condition.Wait()
	}
	if service.closed {
		return nil, false
	}
	var queued *job
	if len(service.retryInteractive) > 0 {
		queued = service.retryInteractive[0]
		service.retryInteractive[0] = nil
		service.retryInteractive = service.retryInteractive[1:]
	} else if len(service.interactive) > 0 {
		queued = service.interactive[0]
		service.interactive[0] = nil
		service.interactive = service.interactive[1:]
	} else if len(service.retryBackground) > 0 {
		queued = service.retryBackground[0]
		service.retryBackground[0] = nil
		service.retryBackground = service.retryBackground[1:]
	} else {
		queued = service.background[0]
		service.background[0] = nil
		service.background = service.background[1:]
	}
	queued.claimed = true
	return queued, true
}

func (service *Service) queuedJobsLocked() int {
	return len(service.retryInteractive) + len(service.interactive) +
		len(service.retryBackground) + len(service.background)
}

func (service *Service) enqueueRetryLocked(queued *job) {
	if queued.request.Priority == PriorityInteractive {
		service.retryInteractive = append(service.retryInteractive, queued)
		return
	}
	service.retryBackground = append(service.retryBackground, queued)
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
	record := Record{
		Key: queued.request.Key, RatingKey: queued.request.RatingKey,
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
	if err != nil && !errors.Is(err, context.Canceled) {
		now := service.now()
		service.rememberNegativeLocked(service.negativeKey(queued.request), now)
		retry = service.scheduleRetryLocked(flight, queued.request.ClientUserAgent, now)
	} else {
		service.releaseRetryRegistrationsLocked(flight)
	}
	flight.record = record
	flight.err = err
	close(flight.done)
	service.mu.Unlock()
	if retry != nil {
		service.metric(func(metrics ServiceMetrics) { metrics.IncMediaInfoProbeQueued() })
	}
	if err != nil {
		service.metric(func(metrics ServiceMetrics) { metrics.IncMediaInfoProbeFailure() })
		service.logger.Info("mediainfo_probe_failed", "part", queued.request.Key.PartID, "error_kind", probeErrorKind(err))
		return
	}
	service.metric(func(metrics ServiceMetrics) { metrics.IncMediaInfoProbeSuccess() })
}

// scheduleRetryLocked promotes the first eligible Submit registration to a
// new flight and carries the remaining registrations forward. The caller must
// hold service.mu. A failed User-Agent is already in negative backoff, so it
// cannot be selected for an immediate blind retry.
func (service *Service) scheduleRetryLocked(failed *flight, failedUserAgent string, now time.Time) *flight {
	var selected *retryRegistration
	remaining := make([]retryRegistration, 0, len(failed.retryWaiters))
	for index := range failed.retryWaiters {
		registration := failed.retryWaiters[index]
		service.retryRegistrations--
		if registration.request.ClientUserAgent == failedUserAgent ||
			service.negativeActiveLocked(registration.request, now) {
			continue
		}
		if selected == nil {
			candidate := registration
			selected = &candidate
			continue
		}
		remaining = append(remaining, registration)
	}
	failed.retryWaiters = nil
	if selected == nil {
		return nil
	}

	next := &flight{done: make(chan struct{}), userAgent: selected.request.ClientUserAgent}
	next.retryWaiters = remaining
	service.retryRegistrations += len(remaining)
	next.job = &job{request: selected.request, flight: next, generation: failed.job.generation}
	service.enqueueRetryLocked(next.job)
	service.flights[next.job.request.Key.cacheKey()] = next
	service.condition.Signal()
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
		InteractiveQueued: len(service.retryInteractive) + len(service.interactive),
		BackgroundQueued:  len(service.retryBackground) + len(service.background),
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
	service.interactive = nil
	service.background = nil
	service.retryInteractive = nil
	service.retryBackground = nil
	service.negative = make(map[string]time.Time)
	for {
		select {
		case touch := <-service.touches:
			touch.reservation.release()
		default:
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
		service.interactive = nil
		service.background = nil
		service.retryInteractive = nil
		service.retryBackground = nil
		service.condition.Broadcast()
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
