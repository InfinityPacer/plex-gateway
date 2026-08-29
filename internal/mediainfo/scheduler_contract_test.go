package mediainfo

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gatewaymetrics "github.com/InfinityPacer/plex-gateway/internal/metrics"
)

type atomicClock struct {
	nanos atomic.Int64
}

type blockingGetStore struct {
	*fakeRecordStore
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (store *blockingGetStore) Get(ctx context.Context, key Key) (Record, bool, error) {
	store.once.Do(func() { close(store.started) })
	select {
	case <-ctx.Done():
		return Record{}, false, ctx.Err()
	case <-store.release:
		return store.fakeRecordStore.Get(ctx, key)
	}
}

func newAtomicClock(now time.Time) *atomicClock {
	clock := &atomicClock{}
	clock.nanos.Store(now.UnixNano())
	return clock
}

func (clock *atomicClock) Now() time.Time {
	return time.Unix(0, clock.nanos.Load()).UTC()
}

func (clock *atomicClock) Advance(duration time.Duration) {
	clock.nanos.Add(duration.Nanoseconds())
}

func TestServiceOfferIsMemoryOnly(t *testing.T) {
	store := &fakeRecordStore{}
	prober := &blockingProber{started: make(chan string, 2), release: make(chan struct{}, 2)}
	service := newServiceForTest(t, ServiceOptions{
		Cache: NewCache(nil, time.Now()), Store: store,
		Provider: &fakeProvider{prober: prober}, PlexServerID: "server",
	})

	if result := service.Offer(testRequest("offer-blocker", PriorityPlayback)); result.Err != nil {
		t.Fatal(result.Err)
	}
	<-prober.started
	store.gets.Store(0)
	result := service.Offer(testRequest("offer-memory-only", PriorityMetadata))
	if result.Disposition != SubmitNewlyQueued || result.Err != nil {
		t.Fatalf("Offer() result = %#v", result)
	}
	if got := store.gets.Load(); got != 0 {
		t.Fatalf("Offer() performed %d durable reads", got)
	}
	prober.release <- struct{}{}
}

func TestServicePromotesP2ToP0InPlaceAndUsesPlaybackRequest(t *testing.T) {
	prober := &blockingProber{started: make(chan string, 3), release: make(chan struct{}, 3)}
	service := newServiceForTest(t, ServiceOptions{
		Cache: NewCache(nil, time.Now()), Store: &fakeRecordStore{}, Provider: &fakeProvider{prober: prober},
		PlexServerID: "server", Concurrency: 1, PlaybackQueueSize: 2,
		NeighborQueueSize: 2, MetadataQueueSize: 2, BackgroundInterval: time.Millisecond,
	})
	active := testRequest("promotion-blocker", PriorityPlayback)
	if result := service.Offer(active); result.Err != nil {
		t.Fatal(result.Err)
	}
	if got := <-prober.started; got != "https://cdn.example.test/promotion-blocker" {
		t.Fatalf("active target = %q", got)
	}

	metadata := testRequest("promotion-p2", PriorityMetadata)
	metadata.ClientUserAgent = "metadata-agent"
	if result := service.Offer(metadata); result.Disposition != SubmitNewlyQueued || result.Err != nil {
		t.Fatalf("P2 offer = %#v", result)
	}
	playback := metadata
	playback.Priority = PriorityPlayback
	playback.RatingKey = "playback-rating"
	playback.Target = "playback-target"
	playback.ClientUserAgent = "playback-agent"
	if result := service.Offer(playback); result.Disposition != SubmitJoinedExistingFlight || result.Err != nil {
		t.Fatalf("P0 promotion = %#v", result)
	}
	status := service.Status()
	if status.PlaybackQueued != 1 || status.MetadataQueued != 0 {
		t.Fatalf("status after promotion = %#v", status)
	}

	prober.release <- struct{}{}
	if got := <-prober.started; got != "https://cdn.example.test/playback-target" {
		t.Fatalf("promoted target = %q", got)
	}
	prober.release <- struct{}{}
}

func TestServicePromotesP1ToP0WithoutAddingAFlight(t *testing.T) {
	prober := &blockingProber{started: make(chan string, 3), release: make(chan struct{}, 3)}
	service := newServiceForTest(t, ServiceOptions{
		Cache: NewCache(nil, time.Now()), Store: &fakeRecordStore{}, Provider: &fakeProvider{prober: prober},
		PlexServerID: "server", Concurrency: 1, PlaybackQueueSize: 1,
		NeighborQueueSize: 2, MetadataQueueSize: 2, BackgroundInterval: time.Millisecond,
	})
	if result := service.Offer(testRequest("p1-blocker", PriorityPlayback)); result.Err != nil {
		t.Fatal(result.Err)
	}
	<-prober.started
	neighbor := testRequest("promotion-p1", PriorityNeighbor)
	if result := service.Offer(neighbor); result.Err != nil {
		t.Fatal(result.Err)
	}
	if result := service.Offer(Request{
		Key: neighbor.Key, RatingKey: "new-rating", Target: "new-target",
		Priority: PriorityPlayback, ClientUserAgent: "playback-agent",
	}); result.Disposition != SubmitJoinedExistingFlight || result.Err != nil {
		t.Fatalf("P1 promotion = %#v", result)
	}
	if got := service.Status().PlaybackQueued; got != 1 {
		t.Fatalf("playback queue = %d, want 1", got)
	}
	prober.release <- struct{}{}
	if got := <-prober.started; got != "https://cdn.example.test/new-target" {
		t.Fatalf("promoted target = %q", got)
	}
	prober.release <- struct{}{}
}

func TestServiceP0PromotionDuringPreflightDoesNotRetrySameUserAgent(t *testing.T) {
	store := &blockingGetStore{
		fakeRecordStore: &fakeRecordStore{}, started: make(chan struct{}), release: make(chan struct{}),
	}
	started := make(chan string, 2)
	var calls atomic.Int64
	probe := proberFunc(func(_ context.Context, _ string, userAgent string) (Media, error) {
		calls.Add(1)
		started <- userAgent
		return Media{}, errors.New("probe rejected")
	})
	service := newServiceForTest(t, ServiceOptions{
		Cache: NewCache(nil, time.Now()), Store: store, Provider: &fakeProvider{prober: probe},
		PlexServerID: "server", Concurrency: 1, BackgroundInterval: time.Millisecond,
	})
	request := testRequest("preflight-promotion", PriorityMetadata)
	request.ClientUserAgent = "metadata-agent"
	if result := service.Offer(request); result.Err != nil {
		t.Fatal(result.Err)
	}
	select {
	case <-store.started:
	case <-time.After(time.Second):
		t.Fatal("background preflight did not start")
	}
	request.Priority = PriorityPlayback
	request.ClientUserAgent = "playback-agent"
	if result := service.Offer(request); result.Disposition != SubmitJoinedExistingFlight || result.Err != nil {
		t.Fatalf("P0 promotion = %#v", result)
	}
	close(store.release)
	select {
	case got := <-started:
		if got != "playback-agent" {
			t.Fatalf("probe User-Agent = %q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("promoted probe did not start")
	}
	waitFor(t, func() bool { return service.Status().ActiveProbes == 0 })
	select {
	case got := <-started:
		t.Fatalf("unexpected same-User-Agent retry with %q", got)
	case <-time.After(50 * time.Millisecond):
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("probe calls = %d, want 1", got)
	}
}

func TestServicePendingTTLExpiresBeforeProviderAndDoesNotNegativeCache(t *testing.T) {
	clock := newAtomicClock(time.Unix(1_800_000_000, 0))
	prober := &blockingProber{started: make(chan string, 2), release: make(chan struct{}, 2)}
	service := newServiceForTest(t, ServiceOptions{
		Cache: NewCache(nil, clock.Now()), Store: &fakeRecordStore{}, Provider: &fakeProvider{prober: prober},
		PlexServerID: "server", Concurrency: 1, PendingTTL: 5 * time.Minute,
		BackgroundInterval: time.Millisecond, Now: clock.Now,
	})
	if result := service.Offer(testRequest("ttl-blocker", PriorityPlayback)); result.Err != nil {
		t.Fatal(result.Err)
	}
	<-prober.started
	pending := testRequest("ttl-expired", PriorityMetadata)
	flight, err := service.begin(pending)
	if err != nil {
		t.Fatal(err)
	}
	clock.Advance(6 * time.Minute)
	prober.release <- struct{}{}
	select {
	case <-flight.done:
	case <-time.After(time.Second):
		t.Fatal("expired flight did not finish")
	}
	if !errors.Is(flight.err, ErrPendingExpired) {
		t.Fatalf("expired flight error = %v", flight.err)
	}
	if got := service.provider.(*fakeProvider).calls.Load(); got != 1 {
		t.Fatalf("provider calls = %d, want blocker only", got)
	}
	service.mu.Lock()
	negative := len(service.negative)
	service.mu.Unlock()
	if negative != 0 {
		t.Fatalf("negative entries after expiration = %d", negative)
	}
}

func TestServiceP0WakesWorkerWaitingForBackgroundInterval(t *testing.T) {
	prober := &blockingProber{started: make(chan string, 4), release: make(chan struct{}, 4)}
	service := newServiceForTest(t, ServiceOptions{
		Cache: NewCache(nil, time.Now()), Store: &fakeRecordStore{}, Provider: &fakeProvider{prober: prober},
		PlexServerID: "server", Concurrency: 1, BackgroundInterval: 200 * time.Millisecond,
	})
	if result := service.Offer(testRequest("interval-first", PriorityNeighbor)); result.Err != nil {
		t.Fatal(result.Err)
	}
	if got := <-prober.started; got != "https://cdn.example.test/interval-first" {
		t.Fatalf("first target = %q", got)
	}
	if result := service.Offer(testRequest("interval-waiting", PriorityNeighbor)); result.Err != nil {
		t.Fatal(result.Err)
	}
	prober.release <- struct{}{}

	select {
	case got := <-prober.started:
		t.Fatalf("background started early with %q", got)
	case <-time.After(30 * time.Millisecond):
	}
	urgent := testRequest("interval-playback", PriorityPlayback)
	if result := service.Offer(urgent); result.Err != nil {
		t.Fatal(result.Err)
	}
	select {
	case got := <-prober.started:
		if got != "https://cdn.example.test/interval-playback" {
			t.Fatalf("urgent target = %q", got)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("P0 did not wake the worker")
	}
	prober.release <- struct{}{}
	select {
	case got := <-prober.started:
		if got != "https://cdn.example.test/interval-waiting" {
			t.Fatalf("waiting target = %q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("background job did not resume after interval")
	}
	prober.release <- struct{}{}
}

func TestServiceP0AdmissionIsIndependentFromBackgroundCapacity(t *testing.T) {
	prober := &blockingProber{started: make(chan string, 4), release: make(chan struct{}, 4)}
	service := newServiceForTest(t, ServiceOptions{
		Cache: NewCache(nil, time.Now()), Store: &fakeRecordStore{}, Provider: &fakeProvider{prober: prober},
		PlexServerID: "server", Concurrency: 1, PlaybackQueueSize: 1,
		NeighborQueueSize: 1, MetadataQueueSize: 1, BackgroundInterval: time.Hour,
	})
	if result := service.Offer(testRequest("capacity-blocker", PriorityPlayback)); result.Err != nil {
		t.Fatal(result.Err)
	}
	<-prober.started
	if result := service.Offer(testRequest("capacity-neighbor", PriorityNeighbor)); result.Err != nil {
		t.Fatal(result.Err)
	}
	if result := service.Offer(testRequest("capacity-metadata", PriorityMetadata)); result.Err != nil {
		t.Fatal(result.Err)
	}
	if result := service.Offer(testRequest("capacity-playback", PriorityPlayback)); result.Err != nil {
		t.Fatalf("P0 was rejected while background queues were full: %#v", result)
	}
	if got := service.Status().PlaybackQueued; got != 1 {
		t.Fatalf("playback queue = %d, want 1", got)
	}
	prober.release <- struct{}{}
}

func TestServiceActivePlaybackJoinKeepsLatestUserAgentForOneFallback(t *testing.T) {
	started := make(chan string, 2)
	releaseFailure := make(chan struct{})
	var calls atomic.Int64
	probe := proberFunc(func(ctx context.Context, _, userAgent string) (Media, error) {
		call := calls.Add(1)
		started <- userAgent
		if call == 1 {
			select {
			case <-ctx.Done():
				return Media{}, ctx.Err()
			case <-releaseFailure:
			}
			return Media{}, errors.New("playback User-Agent rejected")
		}
		return Media{
			Complete: true, Container: "mkv", DurationMS: 60_000,
			Streams: []Stream{{Index: 0, Type: "video", Codec: "hevc", Width: 1920, Height: 1080}},
		}, nil
	})
	service := newServiceForTest(t, ServiceOptions{
		Cache: NewCache(nil, time.Now()), Store: &fakeRecordStore{}, Provider: &fakeProvider{prober: probe},
		PlexServerID: "server", BackgroundInterval: time.Millisecond,
	})
	first := testRequest("active-playback-fallback", PriorityPlayback)
	first.ClientUserAgent = "playback-a"
	if result := service.Offer(first); result.Err != nil {
		t.Fatal(result.Err)
	}
	if got := <-started; got != "playback-a" {
		t.Fatalf("first User-Agent = %q", got)
	}
	latest := first
	latest.ClientUserAgent = "playback-b"
	latest.Target = "latest-target"
	if result := service.Offer(latest); result.Disposition != SubmitJoinedExistingFlight || result.Err != nil {
		t.Fatalf("active join = %#v", result)
	}
	close(releaseFailure)
	if got := <-started; got != "playback-b" {
		t.Fatalf("fallback User-Agent = %q", got)
	}
	waitFor(t, func() bool { return calls.Load() == 2 && service.Status().ActiveProbes == 0 })
}

func TestServiceMetadataJoinDoesNotRegisterUserAgentFallback(t *testing.T) {
	started := make(chan string, 1)
	releaseFailure := make(chan struct{})
	var calls atomic.Int64
	probe := proberFunc(func(ctx context.Context, _, userAgent string) (Media, error) {
		calls.Add(1)
		started <- userAgent
		select {
		case <-ctx.Done():
			return Media{}, ctx.Err()
		case <-releaseFailure:
			return Media{}, errors.New("metadata probe rejected")
		}
	})
	service := newServiceForTest(t, ServiceOptions{
		Cache: NewCache(nil, time.Now()), Store: &fakeRecordStore{}, Provider: &fakeProvider{prober: probe},
		PlexServerID: "server", BackgroundInterval: time.Millisecond,
	})
	first := testRequest("metadata-no-fallback", PriorityMetadata)
	first.ClientUserAgent = "metadata-a"
	if result := service.Offer(first); result.Err != nil {
		t.Fatal(result.Err)
	}
	if got := <-started; got != "metadata-a" {
		t.Fatalf("first User-Agent = %q", got)
	}
	joined := first
	joined.ClientUserAgent = "metadata-b"
	if result := service.Offer(joined); result.Disposition != SubmitJoinedExistingFlight || result.Err != nil {
		t.Fatalf("metadata join = %#v", result)
	}
	close(releaseFailure)
	waitFor(t, func() bool { return service.Status().ActiveProbes == 0 })
	if got := calls.Load(); got != 1 {
		t.Fatalf("metadata probe calls = %d, want 1", got)
	}
}

func TestServiceWorkerSkipsFreshDurableRecord(t *testing.T) {
	prober := &blockingProber{started: make(chan string, 2), release: make(chan struct{}, 2)}
	store := &fakeRecordStore{}
	service := newServiceForTest(t, ServiceOptions{
		Cache: NewCache(nil, time.Now()), Store: store, Provider: &fakeProvider{prober: prober},
		PlexServerID: "server", BackgroundInterval: time.Millisecond,
	})
	blocker := testRequest("l2-blocker", PriorityPlayback)
	if result := service.Offer(blocker); result.Err != nil {
		t.Fatal(result.Err)
	}
	<-prober.started
	pending := testRequest("l2-hit", PriorityMetadata)
	now := time.Now().UTC()
	record := completeRecord(now)
	record.Key = pending.Key
	record.RatingKey = pending.RatingKey
	record.ExpiresAt = now.Add(time.Hour)
	record.RetainUntil = now.Add(24 * time.Hour)
	record.LastAccessedAt = now
	store.mu.Lock()
	store.records = map[string]Record{pending.Key.cacheKey(): record}
	store.mu.Unlock()
	flight, err := service.begin(pending)
	if err != nil {
		t.Fatal(err)
	}
	prober.release <- struct{}{}
	select {
	case <-flight.done:
	case <-time.After(time.Second):
		t.Fatal("L2 hit flight did not finish")
	}
	if flight.err != nil || prober.calls.Load() != 1 {
		t.Fatalf("L2 hit err=%v provider_calls=%d", flight.err, prober.calls.Load())
	}
}

func TestServiceFreshOfferRenewsAccessWithoutDurableRead(t *testing.T) {
	createdAt := time.Unix(1_800_000_000, 0).UTC()
	clock := newAtomicClock(createdAt.Add(2 * time.Hour))
	request := testRequest("offer-touch", PriorityPlayback)
	record := completeRecord(createdAt)
	record.Key = request.Key
	record.RatingKey = request.RatingKey
	store := &fakeRecordStore{records: map[string]Record{record.Key.cacheKey(): record}}
	service := newServiceForTest(t, ServiceOptions{
		Cache: NewCache([]Record{record}, createdAt), Store: store,
		Provider: &fakeProvider{prober: &blockingProber{}}, PlexServerID: "server",
		Now: clock.Now, TouchInterval: time.Hour,
	})

	result := service.Offer(request)
	if result.Disposition != SubmitFreshCache || result.Err != nil {
		t.Fatalf("Offer() = %#v", result)
	}
	if got := store.gets.Load(); got != 0 {
		t.Fatalf("durable reads = %d", got)
	}
	waitFor(t, func() bool { return store.touches.Load() == 1 })
	got, found := service.cache.GetKnown(request.Key, clock.Now())
	if !found || !got.LastAccessedAt.Equal(clock.Now()) {
		t.Fatalf("renewed record found=%v last_access=%s", found, got.LastAccessedAt)
	}
}

func TestServiceBackgroundColdMissReadsDurableStoreOnce(t *testing.T) {
	prober := &blockingProber{started: make(chan string, 1), release: make(chan struct{}, 1)}
	store := &fakeRecordStore{}
	service := newServiceForTest(t, ServiceOptions{
		Cache: NewCache(nil, time.Now()), Store: store, Provider: &fakeProvider{prober: prober},
		PlexServerID: "server", BackgroundInterval: time.Millisecond,
	})
	if result := service.Offer(testRequest("one-l2-read", PriorityNeighbor)); result.Err != nil {
		t.Fatal(result.Err)
	}
	select {
	case <-prober.started:
	case <-time.After(time.Second):
		t.Fatal("background provider did not start")
	}
	if got := store.gets.Load(); got != 1 {
		t.Fatalf("durable reads = %d, want 1", got)
	}
	prober.release <- struct{}{}
}

func TestServiceFreshDurableHitBypassesBackgroundInterval(t *testing.T) {
	now := time.Now().UTC()
	request := testRequest("l2-no-rate-limit", PriorityMetadata)
	record := completeRecord(now)
	record.Key = request.Key
	record.RatingKey = request.RatingKey
	store := &fakeRecordStore{records: map[string]Record{record.Key.cacheKey(): record}}
	provider := &fakeProvider{prober: &blockingProber{}}
	service := newServiceForTest(t, ServiceOptions{
		Cache: NewCache(nil, now), Store: store, Provider: provider,
		PlexServerID: "server", BackgroundInterval: time.Hour,
	})
	flight, _, err := service.beginDetailed(request, false)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-flight.done:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("fresh L2 hit waited for the remote background interval")
	}
	if flight.err != nil || provider.calls.Load() != 0 || store.gets.Load() != 1 {
		t.Fatalf("flight_err=%v provider_calls=%d store_gets=%d", flight.err, provider.calls.Load(), store.gets.Load())
	}
}

func TestServiceP0BurstWakesConfiguredWorkers(t *testing.T) {
	prober := &blockingProber{started: make(chan string, 3), release: make(chan struct{}, 3)}
	service := newServiceForTest(t, ServiceOptions{
		Cache: NewCache(nil, time.Now()), Store: &fakeRecordStore{}, Provider: &fakeProvider{prober: prober},
		PlexServerID: "server", Concurrency: 3, PlaybackQueueSize: 3,
	})
	for _, partID := range []string{"burst-a", "burst-b", "burst-c"} {
		if result := service.Offer(testRequest(partID, PriorityPlayback)); result.Err != nil {
			t.Fatal(result.Err)
		}
	}
	for range 3 {
		select {
		case <-prober.started:
		case <-time.After(200 * time.Millisecond):
			t.Fatal("P0 burst did not use all configured workers")
		}
	}
	for range 3 {
		prober.release <- struct{}{}
	}
}

func TestServiceP0PromotionFailsOpenWhenP0QueueIsFull(t *testing.T) {
	prober := &blockingProber{started: make(chan string, 1), release: make(chan struct{}, 1)}
	service := newServiceForTest(t, ServiceOptions{
		Cache: NewCache(nil, time.Now()), Store: &fakeRecordStore{}, Provider: &fakeProvider{prober: prober},
		PlexServerID: "server", Concurrency: 1, PlaybackQueueSize: 1,
		NeighborQueueSize: 1, MetadataQueueSize: 1, BackgroundInterval: time.Hour,
	})
	if result := service.Offer(testRequest("promotion-full-active", PriorityPlayback)); result.Err != nil {
		t.Fatal(result.Err)
	}
	<-prober.started
	if result := service.Offer(testRequest("promotion-full-p0", PriorityPlayback)); result.Err != nil {
		t.Fatal(result.Err)
	}
	metadata := testRequest("promotion-full-target", PriorityMetadata)
	if result := service.Offer(metadata); result.Err != nil {
		t.Fatal(result.Err)
	}
	metadata.Priority = PriorityPlayback
	result := service.Offer(metadata)
	if result.Disposition != SubmitRejected || !errors.Is(result.Err, ErrQueueFull) {
		t.Fatalf("promotion result = %#v", result)
	}
	if status := service.Status(); status.PlaybackQueued != 1 || status.MetadataQueued != 1 {
		t.Fatalf("status after rejected promotion = %#v", status)
	}
	prober.release <- struct{}{}
}

func TestServicePlaybackRetryChainAllowsOnlyOneFallback(t *testing.T) {
	started := make(chan string, 3)
	releases := make(chan struct{}, 2)
	var calls atomic.Int64
	probe := proberFunc(func(ctx context.Context, _, userAgent string) (Media, error) {
		calls.Add(1)
		started <- userAgent
		select {
		case <-ctx.Done():
			return Media{}, ctx.Err()
		case <-releases:
			return Media{}, errors.New("User-Agent rejected")
		}
	})
	service := newServiceForTest(t, ServiceOptions{
		Cache: NewCache(nil, time.Now()), Store: &fakeRecordStore{},
		Provider: &fakeProvider{prober: probe}, PlexServerID: "server",
	})
	request := testRequest("retry-chain", PriorityPlayback)
	request.ClientUserAgent = "playback-a"
	if result := service.Offer(request); result.Err != nil {
		t.Fatal(result.Err)
	}
	<-started
	request.ClientUserAgent = "playback-b"
	if result := service.Offer(request); result.Err != nil {
		t.Fatal(result.Err)
	}
	releases <- struct{}{}
	if got := <-started; got != "playback-b" {
		t.Fatalf("fallback User-Agent = %q", got)
	}
	request.ClientUserAgent = "playback-c"
	if result := service.Offer(request); result.Err != nil {
		t.Fatal(result.Err)
	}
	releases <- struct{}{}
	waitFor(t, func() bool { return service.Status().ActiveProbes == 0 })
	select {
	case got := <-started:
		t.Fatalf("unexpected third retry with User-Agent %q", got)
	case <-time.After(50 * time.Millisecond):
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("probe calls = %d, want 2", got)
	}
}

func TestServiceCountsRetryDroppedByFullPriorityQueue(t *testing.T) {
	started := make(chan string, 2)
	releaseFirst := make(chan struct{})
	var calls atomic.Int64
	probe := proberFunc(func(ctx context.Context, target, _ string) (Media, error) {
		if calls.Add(1) == 1 {
			started <- target
			select {
			case <-ctx.Done():
				return Media{}, ctx.Err()
			case <-releaseFirst:
				return Media{}, errors.New("first User-Agent rejected")
			}
		}
		started <- target
		return Media{
			Complete: true, Container: "mkv", DurationMS: 60_000,
			Streams: []Stream{{Index: 0, Type: "video", Codec: "hevc", Width: 1920, Height: 1080}},
		}, nil
	})
	registry := gatewaymetrics.New()
	service := newServiceForTest(t, ServiceOptions{
		Cache: NewCache(nil, time.Now()), Store: &fakeRecordStore{},
		Provider: &fakeProvider{prober: probe}, PlexServerID: "server",
		Concurrency: 1, PlaybackQueueSize: 1, Metrics: registry,
	})
	request := testRequest("retry-full", PriorityPlayback)
	request.ClientUserAgent = "playback-a"
	if result := service.Offer(request); result.Err != nil {
		t.Fatal(result.Err)
	}
	<-started
	request.ClientUserAgent = "playback-b"
	if result := service.Offer(request); result.Err != nil {
		t.Fatal(result.Err)
	}
	if result := service.Offer(testRequest("retry-full-blocker", PriorityPlayback)); result.Err != nil {
		t.Fatal(result.Err)
	}
	close(releaseFirst)
	waitFor(t, func() bool {
		return registry.Snapshot().MediaInfoProbeDroppedFullTotal == 1
	})
	if got := registry.Snapshot().MediaInfoProbeDroppedFullTotal; got != 1 {
		t.Fatalf("dropped full = %d, want 1", got)
	}
}

func TestServiceSupersedesQueuedBackgroundWithNewFingerprint(t *testing.T) {
	for _, priority := range []Priority{PriorityNeighbor, PriorityMetadata} {
		t.Run(priorityName(priority), func(t *testing.T) {
			prober := &blockingProber{started: make(chan string, 3), release: make(chan struct{}, 3)}
			service := newServiceForTest(t, ServiceOptions{
				Cache: NewCache(nil, time.Now()), Store: &fakeRecordStore{}, Provider: &fakeProvider{prober: prober},
				PlexServerID: "server", Concurrency: 1, NeighborQueueSize: 4,
				MetadataQueueSize: 4, BackgroundInterval: time.Millisecond,
			})
			if result := service.Offer(testRequest("fingerprint-blocker", PriorityPlayback)); result.Err != nil {
				t.Fatal(result.Err)
			}
			<-prober.started
			old := testRequest("fingerprint-same-part", priority)
			old.Key.STRMFingerprint = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
			old.Target = "old-target"
			oldFlight, err := service.begin(old)
			if err != nil {
				t.Fatal(err)
			}
			newer := old
			newer.Key.STRMFingerprint = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
			newer.Target = "new-target"
			newFlight, err := service.begin(newer)
			if err != nil {
				t.Fatal(err)
			}
			select {
			case <-oldFlight.done:
				if !errors.Is(oldFlight.err, ErrSuperseded) {
					t.Fatalf("old fingerprint error = %v", oldFlight.err)
				}
			case <-time.After(time.Second):
				t.Fatal("old fingerprint was not superseded")
			}
			prober.release <- struct{}{}
			if got := <-prober.started; got != "https://cdn.example.test/new-target" {
				t.Fatalf("new fingerprint target = %q", got)
			}
			prober.release <- struct{}{}
			select {
			case <-newFlight.done:
			case <-time.After(time.Second):
				t.Fatal("new fingerprint flight did not finish")
			}
		})
	}
}

func priorityName(priority Priority) string {
	if priority == PriorityNeighbor {
		return "neighbor"
	}
	return "metadata"
}

func TestServiceTrySubmitAliasesOffer(t *testing.T) {
	service := newTestService(t, &blockingProber{}, 1)
	request := testRequest("try-submit", PriorityMetadata)
	offered := service.Offer(request)
	tried := service.TrySubmit(request)
	if offered.Disposition != SubmitNewlyQueued || offered.Err != nil {
		t.Fatalf("Offer() = %#v", offered)
	}
	if tried.Disposition != SubmitJoinedExistingFlight || tried.Err != nil {
		t.Fatalf("TrySubmit() = %#v", tried)
	}
}

func TestServiceCloseCompletesAllPriorityQueues(t *testing.T) {
	prober := &blockingProber{started: make(chan string, 1), release: make(chan struct{})}
	service := newServiceForTest(t, ServiceOptions{
		Cache: NewCache(nil, time.Now()), Store: &fakeRecordStore{}, Provider: &fakeProvider{prober: prober},
		PlexServerID: "server", Concurrency: 1, PlaybackQueueSize: 2,
		NeighborQueueSize: 2, MetadataQueueSize: 2,
	})
	if result := service.Offer(testRequest("close-active", PriorityPlayback)); result.Err != nil {
		t.Fatal(result.Err)
	}
	<-prober.started
	flights := make([]*flight, 0, 3)
	for _, priority := range []Priority{PriorityPlayback, PriorityNeighbor, PriorityMetadata} {
		flight, err := service.begin(testRequest("close-"+string(rune('a'+len(flights))), priority))
		if err != nil {
			t.Fatal(err)
		}
		flights = append(flights, flight)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := service.Close(ctx); err != nil {
		t.Fatal(err)
	}
	for _, flight := range flights {
		select {
		case <-flight.done:
			if !errors.Is(flight.err, ErrServiceUnavailable) {
				t.Fatalf("closed flight error = %v", flight.err)
			}
		default:
			t.Fatal("queued flight was not completed during close")
		}
	}
}
