package mediainfo

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeRecordStore struct {
	mu      sync.Mutex
	records map[string]Record
	touches atomic.Int64
}

func (store *fakeRecordStore) Put(_ context.Context, record Record) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.records == nil {
		store.records = make(map[string]Record)
	}
	store.records[record.Key.cacheKey()] = cloneRecord(record)
	return nil
}

func (store *fakeRecordStore) Get(_ context.Context, key Key) (Record, bool, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	record, ok := store.records[key.cacheKey()]
	return cloneRecord(record), ok, nil
}

func (store *fakeRecordStore) Touch(_ context.Context, key Key, accessedAt, retainUntil time.Time) error {
	store.touches.Add(1)
	store.mu.Lock()
	defer store.mu.Unlock()
	record, ok := store.records[key.cacheKey()]
	if !ok {
		return nil
	}
	if accessedAt.After(record.LastAccessedAt) {
		record.LastAccessedAt = accessedAt
		record.RetainUntil = retainUntil
		store.records[key.cacheKey()] = record
	}
	return nil
}

type fakeProvider struct {
	calls      atomic.Int64
	prober     Prober
	userAgents chan string
}

func (*fakeProvider) Descriptor() ProviderDescriptor {
	return ProviderDescriptor{Name: ProviderMediaVaultFFProbe, Revision: ProviderRevisionFFProbeJSONV3}
}

func (fake *fakeProvider) Probe(ctx context.Context, request ProviderRequest) (ProviderResult, error) {
	fake.calls.Add(1)
	if fake.userAgents != nil {
		fake.userAgents <- request.UserAgent
	}
	media, err := fake.prober.Probe(ctx, "https://cdn.example.test/"+request.Target, request.UserAgent)
	if err != nil {
		return ProviderResult{}, err
	}
	return ProviderResult{Media: media}, nil
}

type blockingProber struct {
	calls   atomic.Int64
	started chan string
	release chan struct{}
}

type deadlineResultProber struct {
	deadlineObserved chan struct{}
	release          chan struct{}
}

func (prober deadlineResultProber) Probe(ctx context.Context, _, _ string) (Media, error) {
	<-ctx.Done()
	if prober.deadlineObserved != nil {
		close(prober.deadlineObserved)
	}
	if prober.release != nil {
		<-prober.release
	}
	return Media{
		Complete: true, Container: "mkv", DurationMS: 60_000,
		Streams: []Stream{{Index: 0, Type: "video", Codec: "hevc", Width: 1920, Height: 1080}},
	}, nil
}

func (prober *blockingProber) Probe(ctx context.Context, target, _ string) (Media, error) {
	prober.calls.Add(1)
	if prober.started != nil {
		prober.started <- target
	}
	if prober.release != nil {
		select {
		case <-ctx.Done():
			return Media{}, ctx.Err()
		case <-prober.release:
		}
	}
	return Media{
		Complete: true, Container: "mkv", DurationMS: 60_000,
		Streams: []Stream{{Index: 0, Type: "video", Codec: "hevc", Width: 1920, Height: 1080}},
	}, nil
}

func TestServiceSingleflightsMultipleClientsForSamePart(t *testing.T) {
	prober := &blockingProber{release: make(chan struct{})}
	service := newTestService(t, prober, 1)
	request := testRequest("same", PriorityInteractive)

	results := make(chan error, 2)
	for range 2 {
		go func() {
			_, err := service.Ensure(t.Context(), request)
			results <- err
		}()
	}
	waitFor(t, func() bool { return prober.calls.Load() == 1 })
	close(prober.release)
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if prober.calls.Load() != 1 {
		t.Fatalf("probe calls = %d", prober.calls.Load())
	}
}

func TestServiceOwnsPlexServerIdentity(t *testing.T) {
	provider := &fakeProvider{prober: &blockingProber{}}
	service := newServiceForTest(t, ServiceOptions{
		Cache: NewCache(nil, time.Now()), Store: &fakeRecordStore{}, Provider: provider,
		PlexServerID: "server",
	})
	request := testRequest("identity", PriorityInteractive)
	request.Key.PlexServerID = ""
	record, err := service.Ensure(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if record.Key.PlexServerID != "server" {
		t.Fatalf("record Plex server identity = %q", record.Key.PlexServerID)
	}

	request.Key.PlexServerID = "other-server"
	if _, err := service.Ensure(t.Context(), request); err == nil {
		t.Fatal("request for another Plex server was accepted")
	}
	if provider.calls.Load() != 1 {
		t.Fatalf("provider calls = %d", provider.calls.Load())
	}
}

func TestServiceClientWaitCancellationDoesNotCancelBackgroundJob(t *testing.T) {
	prober := &blockingProber{release: make(chan struct{})}
	service := newTestService(t, prober, 1)
	request := testRequest("detached", PriorityInteractive)

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if _, err := service.Ensure(ctx, request); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Ensure() error = %v", err)
	}
	close(prober.release)
	waitFor(t, func() bool {
		_, ok := service.Get(request.Key)
		return ok
	})
}

func TestServicePreservesSuccessfulResultReturnedAtProbeDeadline(t *testing.T) {
	service := newServiceForTest(t, ServiceOptions{
		Cache: NewCache(nil, time.Now()), Store: &fakeRecordStore{},
		Provider:     &fakeProvider{prober: deadlineResultProber{}},
		PlexServerID: "server", ProbeTimeout: 20 * time.Millisecond,
	})

	record, err := service.Ensure(t.Context(), testRequest("deadline-success", PriorityInteractive))
	if err != nil {
		t.Fatal(err)
	}
	if !record.Media.Complete || len(record.Media.Streams) != 1 || record.Media.Streams[0].Codec != "hevc" {
		t.Fatalf("record = %#v", record)
	}
}

func TestServiceRejectsSuccessfulResultReturnedAfterShutdownCancellation(t *testing.T) {
	prober := deadlineResultProber{
		deadlineObserved: make(chan struct{}),
		release:          make(chan struct{}),
	}
	store := &fakeRecordStore{}
	service := newServiceForTest(t, ServiceOptions{
		Cache: NewCache(nil, time.Now()), Store: store,
		Provider:     &fakeProvider{prober: prober},
		PlexServerID: "server", ProbeTimeout: 20 * time.Millisecond,
	})
	request := testRequest("shutdown-canceled", PriorityBackground)
	if err := service.Submit(request); err != nil {
		t.Fatal(err)
	}
	<-prober.deadlineObserved

	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	closed := make(chan error, 1)
	go func() { closed <- service.Close(ctx) }()
	<-service.ctx.Done()
	close(prober.release)
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	if _, ok := service.cache.GetKnown(request.Key, time.Now()); ok {
		t.Fatal("result returned after shutdown cancellation was written to L1")
	}
	store.mu.Lock()
	_, persisted := store.records[request.Key.cacheKey()]
	store.mu.Unlock()
	if persisted {
		t.Fatal("result returned after shutdown cancellation was persisted")
	}
}

func TestServicePrioritizesRapidInteractiveSwitchesOverPrewarm(t *testing.T) {
	prober := &blockingProber{started: make(chan string, 4), release: make(chan struct{}, 4)}
	service := newTestService(t, prober, 1)
	if err := service.Submit(testRequest("A", PriorityBackground)); err != nil {
		t.Fatal(err)
	}
	if got := <-prober.started; got != "https://cdn.example.test/A" {
		t.Fatalf("first target = %q", got)
	}
	if err := service.Submit(testRequest("prewarm", PriorityBackground)); err != nil {
		t.Fatal(err)
	}
	if err := service.Submit(testRequest("B", PriorityInteractive)); err != nil {
		t.Fatal(err)
	}
	prober.release <- struct{}{}
	if got := <-prober.started; got != "https://cdn.example.test/B" {
		t.Fatalf("next target = %q, want interactive B", got)
	}
	prober.release <- struct{}{}
	if got := <-prober.started; got != "https://cdn.example.test/prewarm" {
		t.Fatalf("third target = %q", got)
	}
	prober.release <- struct{}{}
}

func TestServicePromotesBackgroundJobWithoutLeavingDuplicate(t *testing.T) {
	prober := &blockingProber{started: make(chan string, 2), release: make(chan struct{}, 2)}
	service := newTestService(t, prober, 1)
	if err := service.Submit(testRequest("active", PriorityBackground)); err != nil {
		t.Fatal(err)
	}
	<-prober.started
	background := testRequest("promoted", PriorityBackground)
	if err := service.Submit(background); err != nil {
		t.Fatal(err)
	}
	interactive := background
	interactive.Priority = PriorityInteractive
	flight, err := service.begin(interactive)
	if err != nil {
		t.Fatal(err)
	}
	status := service.Status()
	if status.InteractiveQueued != 1 || status.BackgroundQueued != 0 {
		t.Fatalf("queue status after promotion = %#v", status)
	}

	prober.release <- struct{}{}
	if got := <-prober.started; got != "https://cdn.example.test/promoted" {
		t.Fatalf("promoted target = %q", got)
	}
	prober.release <- struct{}{}
	select {
	case <-flight.done:
	case <-time.After(time.Second):
		t.Fatal("promoted flight did not finish")
	}
	waitFor(t, func() bool {
		status := service.Status()
		return status.InteractiveQueued == 0 && status.BackgroundQueued == 0
	})
}

func TestServiceCloseCompletesQueuedFlights(t *testing.T) {
	prober := &blockingProber{started: make(chan string, 1), release: make(chan struct{})}
	service := newTestService(t, prober, 1)
	if err := service.Submit(testRequest("active", PriorityInteractive)); err != nil {
		t.Fatal(err)
	}
	<-prober.started
	flight, err := service.begin(testRequest("queued", PriorityBackground))
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := service.Close(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-flight.done:
		if !errors.Is(flight.err, ErrServiceUnavailable) {
			t.Fatalf("queued flight error = %v", flight.err)
		}
	default:
		t.Fatal("queued flight was not completed during close")
	}
}

func TestServiceRejectsWorkWhenPriorityQueueIsFull(t *testing.T) {
	prober := &blockingProber{started: make(chan string, 1), release: make(chan struct{})}
	service := newTestServiceWithQueues(t, prober, 1, 1, 1)
	if err := service.Submit(testRequest("active", PriorityBackground)); err != nil {
		t.Fatal(err)
	}
	<-prober.started
	if err := service.Submit(testRequest("queued", PriorityBackground)); err != nil {
		t.Fatal(err)
	}
	if err := service.Submit(testRequest("overflow", PriorityBackground)); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("Submit() error = %v", err)
	}
}

func TestServiceBoundsAndSweepsNegativeCache(t *testing.T) {
	service := newTestService(t, &blockingProber{}, 1)
	now := time.Unix(1_800_000_000, 0)
	service.mu.Lock()
	for index := 0; index < defaultNegativeLimit+100; index++ {
		service.rememberNegativeLocked(fmt.Sprintf("key-%d", index), now)
	}
	if len(service.negative) != defaultNegativeLimit {
		service.mu.Unlock()
		t.Fatalf("negative cache size = %d", len(service.negative))
	}
	service.rememberNegativeLocked("after-expiry", now.Add(service.negativeTTL+time.Second))
	remaining := len(service.negative)
	service.mu.Unlock()
	if remaining != 1 {
		t.Fatalf("negative cache size after sweep = %d", remaining)
	}
}

func TestServiceRestoresFreshL2RecordWithoutProbe(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	request := testRequest("persisted", PriorityInteractive)
	record := completeRecord(now)
	record.Key = request.Key
	record.RatingKey = request.RatingKey
	store := &fakeRecordStore{records: map[string]Record{record.Key.cacheKey(): record}}
	provider := &fakeProvider{prober: &blockingProber{}}
	service := newServiceForTest(t, ServiceOptions{
		Cache: NewCache(nil, now), Store: store, Provider: provider, Now: func() time.Time { return now },
	})

	got, err := service.Ensure(t.Context(), request)
	if err != nil || got.Media.VideoCodec != record.Media.VideoCodec {
		t.Fatalf("Ensure() = %#v, %v", got, err)
	}
	if provider.calls.Load() != 0 {
		t.Fatalf("provider calls = %d", provider.calls.Load())
	}
}

func TestServiceReprobesIncompatibleProviderRevision(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	request := testRequest("old-provider", PriorityInteractive)
	record := completeRecord(now)
	record.Key = request.Key
	record.ProviderRevision = "ffprobe-json-v0"
	store := &fakeRecordStore{records: map[string]Record{record.Key.cacheKey(): record}}
	provider := &fakeProvider{prober: &blockingProber{}}
	service := newServiceForTest(t, ServiceOptions{
		Cache: NewCache(nil, now), Store: store, Provider: provider, Now: func() time.Time { return now },
	})

	got, err := service.Ensure(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if got.ProviderRevision != ProviderRevisionFFProbeJSONV3 || provider.calls.Load() != 1 {
		t.Fatalf("Ensure() revision=%q provider_calls=%d", got.ProviderRevision, provider.calls.Load())
	}
}

func TestServiceGetNormalizesPlexServerIdentity(t *testing.T) {
	now := time.Date(2026, 8, 29, 8, 0, 0, 0, time.UTC)
	record := completeRecord(now.Add(-31 * 24 * time.Hour))
	record.Key.PlexServerID = "server"
	store := &fakeRecordStore{records: map[string]Record{record.Key.cacheKey(): record}}
	provider := &fakeProvider{prober: &blockingProber{}}
	service := newServiceForTest(t, ServiceOptions{
		Cache: NewCache(nil, now), Store: store,
		Provider:     provider,
		PlexServerID: "server", Now: func() time.Time { return now },
	})

	key := record.Key
	key.PlexServerID = ""
	got, found := service.Get(key)
	if !found || got.Key != record.Key || got.Fresh(now) {
		t.Fatalf("Get() found=%v key=%#v, want %#v", found, got.Key, record.Key)
	}
	if provider.calls.Load() != 0 {
		t.Fatalf("cache-only Get scheduled %d provider calls", provider.calls.Load())
	}

	key.PlexServerID = "other-server"
	if _, found := service.Get(key); found {
		t.Fatal("Get() returned a record for another Plex server")
	}
}

func TestServiceReturnsStaleKnownGoodAndRevalidatesInBackground(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	request := testRequest("stale", PriorityInteractive)
	record := completeRecord(now.Add(-31 * 24 * time.Hour))
	record.Key = request.Key
	record.RatingKey = request.RatingKey
	store := &fakeRecordStore{records: map[string]Record{record.Key.cacheKey(): record}}
	prober := &blockingProber{started: make(chan string, 1), release: make(chan struct{})}
	service := newServiceForTest(t, ServiceOptions{
		Cache: NewCache(nil, now), Store: store, Provider: &fakeProvider{prober: prober},
		Now: func() time.Time { return now },
	})

	got, err := service.Ensure(t.Context(), request)
	if err != nil || got.Fresh(now) || !got.Retained(now) {
		t.Fatalf("Ensure() = %#v, %v", got, err)
	}
	select {
	case <-prober.started:
	case <-time.After(time.Second):
		t.Fatal("stale record did not schedule background revalidation")
	}
	close(prober.release)
	waitFor(t, func() bool {
		fresh, ok := service.Get(request.Key)
		return ok && fresh.Fresh(now)
	})
}

func TestServicePersistsHotRecordTouchAcrossIntervals(t *testing.T) {
	createdAt := time.Unix(1_800_000_000, 0).UTC()
	record := completeRecord(createdAt)
	service := &Service{
		cache: NewCache([]Record{record}, createdAt), recordRetention: 180 * 24 * time.Hour,
		touchInterval: time.Hour, touches: make(chan recordTouch, 2),
	}

	for step := 1; step <= 12; step++ {
		service.touchCached(record, createdAt.Add(time.Duration(step)*10*time.Minute))
	}
	if got := len(service.touches); got != 2 {
		t.Fatalf("scheduled persistent touches = %d, want 2", got)
	}
	first := <-service.touches
	second := <-service.touches
	if !first.accessedAt.Equal(createdAt.Add(time.Hour)) || !second.accessedAt.Equal(createdAt.Add(2*time.Hour)) {
		t.Fatalf("touch schedule = %v, %v", first.accessedAt, second.accessedAt)
	}
}

func TestServiceRetriesTouchImmediatelyAfterQueueFull(t *testing.T) {
	createdAt := time.Unix(1_800_000_000, 0).UTC()
	record := completeRecord(createdAt)
	service := &Service{
		cache: NewCache([]Record{record}, createdAt), recordRetention: 180 * 24 * time.Hour,
		touchInterval: time.Hour, touches: make(chan recordTouch, 1),
	}
	service.touches <- recordTouch{}
	due := createdAt.Add(time.Hour)
	service.touchCached(record, due)
	<-service.touches
	service.touchCached(record, due)
	if got := len(service.touches); got != 1 {
		t.Fatalf("retry queue length = %d, want 1", got)
	}
}

func TestServiceUsesClientUserAgentAndBackgroundFallback(t *testing.T) {
	provider := &fakeProvider{prober: &blockingProber{}, userAgents: make(chan string, 2)}
	service := newServiceForTest(t, ServiceOptions{
		Cache: NewCache(nil, time.Now()), Store: &fakeRecordStore{}, Provider: provider,
	})
	interactive := testRequest("interactive-ua", PriorityInteractive)
	interactive.ClientUserAgent = "Infuse-Library/9.0"
	if _, err := service.Ensure(t.Context(), interactive); err != nil {
		t.Fatal(err)
	}
	background := testRequest("background-ua", PriorityBackground)
	background.ClientUserAgent = ""
	if err := service.Submit(background); err != nil {
		t.Fatal(err)
	}
	if got := <-provider.userAgents; got != "Infuse-Library/9.0" {
		t.Fatalf("interactive User-Agent = %q", got)
	}
	if got := <-provider.userAgents; got != "Infuse-Library/8.4.4" {
		t.Fatalf("background User-Agent = %q", got)
	}
}

func newTestService(t *testing.T, prober Prober, concurrency int) *Service {
	return newTestServiceWithQueues(t, prober, concurrency, 8, 8)
}

func newTestServiceWithQueues(t *testing.T, prober Prober, concurrency, interactiveQueueSize, backgroundQueueSize int) *Service {
	t.Helper()
	return newServiceForTest(t, ServiceOptions{
		Cache: NewCache(nil, time.Now()), Store: &fakeRecordStore{}, Provider: &fakeProvider{prober: prober},
		Concurrency: concurrency, InteractiveQueueSize: interactiveQueueSize, BackgroundQueueSize: backgroundQueueSize,
		ProbeTimeout: time.Second, RecordTTL: time.Hour, RecordRetention: 24 * time.Hour,
		NegativeTTL: time.Minute, BackgroundUserAgent: "Infuse-Library/8.4.4",
	})
}

func newServiceForTest(t *testing.T, options ServiceOptions) *Service {
	t.Helper()
	if options.Concurrency == 0 {
		options.Concurrency = 1
	}
	if options.ProbeTimeout == 0 {
		options.ProbeTimeout = time.Second
	}
	if options.RecordTTL == 0 {
		options.RecordTTL = time.Hour
	}
	if options.RecordRetention == 0 {
		options.RecordRetention = 24 * time.Hour
	}
	if options.NegativeTTL == 0 {
		options.NegativeTTL = time.Minute
	}
	if options.BackgroundUserAgent == "" {
		options.BackgroundUserAgent = "Infuse-Library/8.4.4"
	}
	if options.PlexServerID == "" {
		options.PlexServerID = "server"
	}
	service, err := NewService(options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := service.Close(ctx); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})
	return service
}

func testRequest(partID string, priority Priority) Request {
	return Request{
		Key: Key{
			PlexServerID: "server", PartID: partID,
			STRMFingerprint: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		},
		RatingKey: partID + "-rating", Target: partID, Priority: priority,
		ClientUserAgent: "Infuse-Library/8.4.4",
	}
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met")
		}
		time.Sleep(time.Millisecond)
	}
}
