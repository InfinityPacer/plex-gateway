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
	defaultRecordTTL       = 30 * 24 * time.Hour
	defaultRecordRetention = 180 * 24 * time.Hour
	defaultNegativeTTL     = 15 * time.Minute
	defaultWorkerQueueSize = 256
	defaultWorkerCount     = 1
	defaultNegativeLimit   = 4096
	defaultTouchQueueSize  = 1024
	defaultGCInterval      = 24 * time.Hour
	defaultStoreTimeout    = 5 * time.Second
	defaultTouchInterval   = time.Hour
)

var (
	ErrServiceUnavailable = errors.New("MediaInfo service is unavailable")
	ErrQueueFull          = errors.New("MediaInfo queue is full")
	ErrNegativeCache      = errors.New("MediaInfo probe is in backoff")
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
	return nil
}

// RecordStore is the successful-result persistence boundary.
type RecordStore interface {
	Put(context.Context, Record) error
	Get(context.Context, Key) (Record, bool, error)
	Touch(context.Context, Key, time.Time, time.Time) error
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

	mu               sync.Mutex
	condition        *sync.Cond
	interactive      []*job
	background       []*job
	interactiveLimit int
	backgroundLimit  int
	flights          map[string]*flight
	negative         map[string]time.Time
	closed           bool
	active           atomic.Int64
}

type recordTouch struct {
	key         Key
	accessedAt  time.Time
	retainUntil time.Time
	reservation touchReservation
}

type flight struct {
	done      chan struct{}
	job       *job
	userAgent string
	record    Record
	err       error
}

type job struct {
	request Request
	flight  *flight
	claimed bool
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
		backgroundUserAgent: backgroundUserAgent, gcInterval: gcInterval,
		touchInterval: touchInterval, now: now,
		ctx: ctx, cancel: cancel, workerDone: make(chan struct{}),
		touches:          make(chan recordTouch, defaultTouchQueueSize),
		interactiveLimit: interactiveSize, backgroundLimit: backgroundSize,
		flights: make(map[string]*flight), negative: make(map[string]time.Time),
	}
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

// Get returns one exact fresh L1 record and renews its retention asynchronously.
func (service *Service) Get(key Key) (Record, bool) {
	if service == nil {
		return Record{}, false
	}
	now := service.now().UTC()
	record, ok := service.cache.Get(key, now)
	if ok {
		record = service.touchCached(record, now)
		service.metric(func(metrics ServiceMetrics) { metrics.IncMediaInfoCacheHits() })
	} else {
		service.metric(func(metrics ServiceMetrics) { metrics.IncMediaInfoCacheMisses() })
	}
	return record, ok
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

// Submit schedules work without creating a request-bound waiter.
func (service *Service) Submit(request Request) error {
	if service == nil {
		return ErrServiceUnavailable
	}
	request, err := service.normalizeRequest(request)
	if err != nil {
		return err
	}
	_, found, fresh := service.lookup(service.ctx, request.Key)
	if found && fresh {
		return nil
	}
	if found {
		request.Priority = PriorityBackground
	}
	_, err = service.begin(request)
	return err
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
	if service == nil {
		return nil, ErrServiceUnavailable
	}
	var err error
	request, err = service.normalizeRequest(request)
	if err != nil {
		return nil, err
	}
	key := request.Key.cacheKey()
	negativeKey := service.negativeKey(request)
	now := service.now()
	service.mu.Lock()
	if service.closed {
		service.mu.Unlock()
		return nil, ErrServiceUnavailable
	}
	if until, exists := service.negative[negativeKey]; exists {
		if now.Before(until) {
			service.mu.Unlock()
			return nil, ErrNegativeCache
		}
		delete(service.negative, negativeKey)
	}
	if existing, exists := service.flights[key]; exists {
		if request.Priority == PriorityInteractive {
			service.promoteLocked(existing.job)
		}
		service.mu.Unlock()
		return existing, nil
	}
	flight := &flight{done: make(chan struct{}), userAgent: request.ClientUserAgent}
	queued := &job{request: request, flight: flight}
	flight.job = queued
	if !service.enqueueLocked(queued) {
		service.mu.Unlock()
		return nil, ErrQueueFull
	}
	service.flights[key] = flight
	service.condition.Signal()
	service.mu.Unlock()
	service.metric(func(metrics ServiceMetrics) { metrics.IncMediaInfoProbeQueued() })
	return flight, nil
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
	if queued == nil || queued.claimed || queued.request.Priority == PriorityInteractive ||
		len(service.interactive) >= service.interactiveLimit {
		return
	}
	for index, candidate := range service.background {
		if candidate != queued {
			continue
		}
		copy(service.background[index:], service.background[index+1:])
		service.background[len(service.background)-1] = nil
		service.background = service.background[:len(service.background)-1]
		queued.request.Priority = PriorityInteractive
		service.interactive = append(service.interactive, queued)
		service.condition.Signal()
		return
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
	service.mu.Lock()
	defer service.mu.Unlock()
	for !service.closed && len(service.interactive) == 0 && len(service.background) == 0 {
		service.condition.Wait()
	}
	if service.closed {
		return nil, false
	}
	var queued *job
	if len(service.interactive) > 0 {
		queued = service.interactive[0]
		service.interactive[0] = nil
		service.interactive = service.interactive[1:]
	} else {
		queued = service.background[0]
		service.background[0] = nil
		service.background = service.background[1:]
	}
	queued.claimed = true
	return queued, true
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
	if err := probeCtx.Err(); err != nil {
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
	storeCtx, cancelStore := context.WithTimeout(service.ctx, defaultStoreTimeout)
	if err := service.store.Put(storeCtx, record); err != nil {
		service.metric(func(metrics ServiceMetrics) { metrics.IncMediaInfoStoreFailure() })
		service.logger.Warn("mediainfo_store_failed", "part", queued.request.Key.PartID, "error_kind", "storage")
	}
	cancelStore()
	service.cache.Put(record, now)
	service.finish(queued, record, nil)
}

func (service *Service) finish(queued *job, record Record, err error) {
	key := queued.request.Key.cacheKey()
	service.mu.Lock()
	flight := service.flights[key]
	if flight != queued.flight {
		service.mu.Unlock()
		return
	}
	delete(service.flights, key)
	if err != nil && !errors.Is(err, context.Canceled) {
		service.rememberNegativeLocked(service.negativeKey(queued.request), service.now())
	}
	flight.record = record
	flight.err = err
	close(flight.done)
	service.mu.Unlock()
	if err != nil {
		service.metric(func(metrics ServiceMetrics) { metrics.IncMediaInfoProbeFailure() })
		service.logger.Info("mediainfo_probe_failed", "part", queued.request.Key.PartID, "error_kind", probeErrorKind(err))
		return
	}
	service.metric(func(metrics ServiceMetrics) { metrics.IncMediaInfoProbeSuccess() })
}

func (service *Service) negativeKey(request Request) string {
	digest := sha256.Sum256([]byte(request.ClientUserAgent))
	return request.Key.cacheKey() + "\x00" + string(digest[:])
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
		InteractiveQueued: len(service.interactive), BackgroundQueued: len(service.background),
		NegativeEntries: len(service.negative),
	}
	service.mu.Unlock()
	return status
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
			close(flight.done)
		}
		service.interactive = nil
		service.background = nil
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
