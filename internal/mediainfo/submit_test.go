package mediainfo

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestServiceSubmitDetailedReportsAdmissionDisposition(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	prober := &blockingProber{started: make(chan string, 1), release: make(chan struct{})}
	service := newServiceForTest(t, ServiceOptions{
		Cache: NewCache(nil, now), Store: &fakeRecordStore{}, Provider: &fakeProvider{prober: prober},
		PlexServerID: "server", Now: func() time.Time { return now },
	})

	fresh := completeRecord(now)
	fresh.Key.PlexServerID = "server"
	fresh.Key.PartID = "fresh"
	service.cache.Put(fresh, now)
	freshResult := service.SubmitDetailed(testRequest("fresh", PriorityBackground))
	if freshResult.Disposition != SubmitFreshCache || freshResult.Err != nil {
		t.Fatalf("fresh cache result = %#v", freshResult)
	}

	queuedRequest := testRequest("queued", PriorityBackground)
	queuedResult := service.SubmitDetailed(queuedRequest)
	if queuedResult.Disposition != SubmitNewlyQueued || queuedResult.Err != nil {
		t.Fatalf("new queue result = %#v", queuedResult)
	}
	select {
	case <-prober.started:
	case <-time.After(time.Second):
		t.Fatal("queued probe did not start")
	}

	joined := testRequest("queued", PriorityBackground)
	joined.ClientUserAgent = "different-client"
	joinedResult := service.SubmitDetailed(joined)
	if joinedResult.Disposition != SubmitJoinedExistingFlight || joinedResult.Err != nil {
		t.Fatalf("joined result = %#v", joinedResult)
	}

	close(prober.release)
	waitFor(t, func() bool {
		_, ok := service.Get(queuedRequest.Key)
		return ok
	})
}

func TestServiceSubmitDetailedReportsRejection(t *testing.T) {
	prober := &blockingProber{started: make(chan string, 1), release: make(chan struct{})}
	service := newTestServiceWithQueues(t, prober, 1, 1, 1)
	if result := service.SubmitDetailed(testRequest("active", PriorityBackground)); result.Disposition != SubmitNewlyQueued || result.Err != nil {
		t.Fatalf("active result = %#v", result)
	}
	select {
	case <-prober.started:
	case <-time.After(time.Second):
		t.Fatal("active probe did not start")
	}
	if result := service.SubmitDetailed(testRequest("queued", PriorityBackground)); result.Disposition != SubmitNewlyQueued || result.Err != nil {
		t.Fatalf("queued result = %#v", result)
	}
	result := service.SubmitDetailed(testRequest("overflow", PriorityBackground))
	if result.Disposition != SubmitRejected || !errors.Is(result.Err, ErrQueueFull) {
		t.Fatalf("overflow result = %#v", result)
	}
}

func TestServiceRejectsOversizedClientUserAgent(t *testing.T) {
	service := newTestService(t, &blockingProber{}, 1)
	request := testRequest("oversized-user-agent", PriorityInteractive)
	request.ClientUserAgent = strings.Repeat("x", maxClientUserAgentBytes+1)
	result := service.SubmitDetailed(request)
	if result.Disposition != SubmitRejected || result.Err == nil {
		t.Fatalf("oversized User-Agent result = %#v", result)
	}
}

func TestServiceSubmitRequeuesJoinedUserAgentAfterFailure(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	started := make(chan string, 2)
	releaseFailure := make(chan struct{})
	var calls atomic.Int64
	prober := proberFunc(func(ctx context.Context, _ string, userAgent string) (Media, error) {
		call := calls.Add(1)
		started <- userAgent
		if call == 1 {
			select {
			case <-ctx.Done():
				return Media{}, ctx.Err()
			case <-releaseFailure:
				return Media{}, errors.New("first User-Agent rejected")
			}
		}
		return Media{
			Complete: true, Container: "mkv", DurationMS: 60_000,
			Streams: []Stream{{Index: 0, Type: "video", Codec: "hevc", Width: 1920, Height: 1080}},
		}, nil
	})
	service := newServiceForTest(t, ServiceOptions{
		Cache: NewCache(nil, now), Store: &fakeRecordStore{}, Provider: &fakeProvider{prober: prober},
		PlexServerID: "server", NegativeTTL: time.Hour, Now: func() time.Time { return now },
	})
	first := testRequest("submit-cross-ua", PriorityInteractive)
	first.ClientUserAgent = "client-agent-a"
	second := first
	second.ClientUserAgent = "client-agent-b"
	if result := service.SubmitDetailed(first); result.Disposition != SubmitNewlyQueued || result.Err != nil {
		t.Fatalf("first submit result = %#v", result)
	}
	if got := <-started; got != first.ClientUserAgent {
		t.Fatalf("first probe User-Agent = %q", got)
	}
	if result := service.SubmitDetailed(second); result.Disposition != SubmitJoinedExistingFlight || result.Err != nil {
		t.Fatalf("second submit result = %#v", result)
	}

	close(releaseFailure)
	if got := <-started; got != second.ClientUserAgent {
		t.Fatalf("retry probe User-Agent = %q", got)
	}
	waitFor(t, func() bool {
		_, ok := service.Get(first.Key)
		return ok
	})
	if got := calls.Load(); got != 2 {
		t.Fatalf("probe calls = %d, want 2", got)
	}
}

func TestServicePromotesPendingUserAgentRetryForCurrentPlayback(t *testing.T) {
	started := make(chan string, 8)
	releases := map[string]chan struct{}{
		"first":   make(chan struct{}),
		"blocker": make(chan struct{}),
		"retry":   make(chan struct{}),
		"other":   make(chan struct{}),
	}
	prober := proberFunc(func(ctx context.Context, target, _ string) (Media, error) {
		target = strings.TrimPrefix(target, "https://cdn.example.test/")
		started <- target
		select {
		case <-ctx.Done():
			return Media{}, ctx.Err()
		case <-releases[target]:
		}
		if target == "first" {
			return Media{}, errors.New("first User-Agent rejected")
		}
		return Media{
			Complete: true, Container: "mkv", DurationMS: 60_000,
			Streams: []Stream{{Index: 0, Type: "video", Codec: "hevc", Width: 1920, Height: 1080}},
		}, nil
	})
	service := newServiceForTest(t, ServiceOptions{
		Cache: NewCache(nil, time.Now()), Store: &fakeRecordStore{}, Provider: &fakeProvider{prober: prober},
		PlexServerID: "server", Concurrency: 1, InteractiveQueueSize: 8, BackgroundQueueSize: 8,
	})
	first := testRequest("first", PriorityBackground)
	first.ClientUserAgent = "client-a"
	retry := first
	retry.Target = "retry"
	retry.ClientUserAgent = "client-b"
	if result := service.SubmitDetailed(first); result.Disposition != SubmitNewlyQueued {
		t.Fatalf("first result = %#v", result)
	}
	if got := <-started; got != "first" {
		t.Fatalf("first target = %q", got)
	}
	if result := service.SubmitDetailed(retry); result.Disposition != SubmitJoinedExistingFlight {
		t.Fatalf("retry registration = %#v", result)
	}
	if result := service.SubmitDetailed(testRequest("blocker", PriorityInteractive)); result.Disposition != SubmitNewlyQueued {
		t.Fatalf("blocker result = %#v", result)
	}
	close(releases["first"])
	if got := <-started; got != "blocker" {
		t.Fatalf("second target = %q", got)
	}
	if result := service.SubmitDetailed(testRequest("other", PriorityInteractive)); result.Disposition != SubmitNewlyQueued {
		t.Fatalf("other result = %#v", result)
	}
	current := retry
	current.Priority = PriorityInteractive
	if result := service.SubmitDetailed(current); result.Disposition != SubmitJoinedExistingFlight {
		t.Fatalf("current join result = %#v", result)
	}
	close(releases["blocker"])
	if got := <-started; got != "retry" {
		t.Fatalf("promoted target = %q, want retry", got)
	}
	close(releases["retry"])
	if got := <-started; got != "other" {
		t.Fatalf("following target = %q, want other", got)
	}
	close(releases["other"])
}

func TestServiceSubmitDoesNotRequeueSameUserAgent(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	started := make(chan string, 1)
	releaseFailure := make(chan struct{})
	var calls atomic.Int64
	prober := proberFunc(func(ctx context.Context, _ string, userAgent string) (Media, error) {
		calls.Add(1)
		started <- userAgent
		select {
		case <-ctx.Done():
			return Media{}, ctx.Err()
		case <-releaseFailure:
			return Media{}, errors.New("probe rejected")
		}
	})
	service := newServiceForTest(t, ServiceOptions{
		Cache: NewCache(nil, now), Store: &fakeRecordStore{}, Provider: &fakeProvider{prober: prober},
		PlexServerID: "server", NegativeTTL: time.Hour, Now: func() time.Time { return now },
	})
	request := testRequest("submit-same-ua", PriorityBackground)
	request.ClientUserAgent = "same-client"
	if result := service.SubmitDetailed(request); result.Disposition != SubmitNewlyQueued || result.Err != nil {
		t.Fatalf("first submit result = %#v", result)
	}
	<-started
	if result := service.SubmitDetailed(request); result.Disposition != SubmitJoinedExistingFlight || result.Err != nil {
		t.Fatalf("same User-Agent join result = %#v", result)
	}
	close(releaseFailure)
	waitFor(t, func() bool { return calls.Load() == 1 && service.Status().ActiveProbes == 0 })
	select {
	case got := <-started:
		t.Fatalf("same User-Agent was requeued as %q", got)
	default:
	}
}

func TestServiceSubmitBoundsRetryRegistrations(t *testing.T) {
	prober := &blockingProber{started: make(chan string, 1), release: make(chan struct{})}
	service := newTestService(t, prober, 1)
	first := testRequest("submit-registration-limit", PriorityBackground)
	first.ClientUserAgent = "owner"
	if result := service.SubmitDetailed(first); result.Err != nil {
		t.Fatalf("first submit result = %#v", result)
	}
	<-prober.started
	for index := 0; index < defaultRetryLimit+32; index++ {
		request := first
		request.ClientUserAgent = fmt.Sprintf("client-%d", index)
		result := service.SubmitDetailed(request)
		if index < defaultRetryLimit {
			if result.Disposition != SubmitJoinedExistingFlight || result.Err != nil {
				t.Fatalf("registration %d result = %#v", index, result)
			}
			continue
		}
		if result.Disposition != SubmitRejected || !errors.Is(result.Err, ErrRetryRegistrationFull) {
			t.Fatalf("overflow registration %d result = %#v", index, result)
		}
	}
	service.mu.Lock()
	registrations := service.retryRegistrations
	service.mu.Unlock()
	if registrations != defaultRetryLimit {
		t.Fatalf("retry registrations = %d, want %d", registrations, defaultRetryLimit)
	}
}
