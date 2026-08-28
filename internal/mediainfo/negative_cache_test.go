package mediainfo

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type scriptedMediaProber struct {
	calls      atomic.Int64
	errors     []error
	userAgents chan string
}

type proberFunc func(context.Context, string, string) (Media, error)

func (probe proberFunc) Probe(ctx context.Context, target, userAgent string) (Media, error) {
	return probe(ctx, target, userAgent)
}

type synchronizedBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

type observedDoneContext struct {
	context.Context
	once     sync.Once
	observed chan struct{}
}

func (ctx *observedDoneContext) Done() <-chan struct{} {
	ctx.once.Do(func() { close(ctx.observed) })
	return ctx.Context.Done()
}

func (buffer *synchronizedBuffer) Write(value []byte) (int, error) {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.buffer.Write(value)
}

func (buffer *synchronizedBuffer) String() string {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.buffer.String()
}

func (prober *scriptedMediaProber) Probe(_ context.Context, _ string, userAgent string) (Media, error) {
	index := int(prober.calls.Add(1)) - 1
	if prober.userAgents != nil {
		prober.userAgents <- userAgent
	}
	if index < len(prober.errors) && prober.errors[index] != nil {
		return Media{}, fmt.Errorf("probe failed for %s: %w", userAgent, prober.errors[index])
	}
	return Media{
		Complete: true, Container: "mkv", DurationMS: 60_000,
		Streams: []Stream{{Index: 0, Type: "video", Codec: "hevc", Width: 1920, Height: 1080}},
	}, nil
}

func TestServiceNegativeCacheSeparatesUserAgents(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	probeErr := errors.New("probe unavailable")
	prober := &scriptedMediaProber{errors: []error{probeErr, probeErr}}
	provider := &fakeProvider{prober: prober}
	logBuffer := new(synchronizedBuffer)
	service := newServiceForTest(t, ServiceOptions{
		Cache: NewCache(nil, now), Store: &fakeRecordStore{}, Provider: provider,
		PlexServerID: "server", NegativeTTL: time.Hour,
		Logger: slog.New(slog.NewTextHandler(logBuffer, nil)), Now: func() time.Time { return now },
	})
	requestA := testRequest("negative-ua", PriorityInteractive)
	requestA.ClientUserAgent = "client-agent-a"
	requestB := requestA
	requestB.ClientUserAgent = "client-agent-b"

	if _, err := service.Ensure(t.Context(), requestA); !errors.Is(err, probeErr) {
		t.Fatalf("first User-Agent error = %v", err)
	}
	if _, err := service.Ensure(t.Context(), requestA); !errors.Is(err, ErrNegativeCache) {
		t.Fatalf("same User-Agent retry error = %v", err)
	}
	if _, err := service.Ensure(t.Context(), requestB); !errors.Is(err, probeErr) {
		t.Fatalf("different User-Agent error = %v", err)
	}
	if _, err := service.Ensure(t.Context(), requestB); !errors.Is(err, ErrNegativeCache) {
		t.Fatalf("different User-Agent retry error = %v", err)
	}
	if got := provider.calls.Load(); got != 2 {
		t.Fatalf("provider calls = %d, want 2", got)
	}

	service.mu.Lock()
	if len(service.negative) != 2 {
		service.mu.Unlock()
		t.Fatalf("negative cache entries = %d, want 2", len(service.negative))
	}
	for key := range service.negative {
		if strings.Contains(key, requestA.ClientUserAgent) || strings.Contains(key, requestB.ClientUserAgent) {
			service.mu.Unlock()
			t.Fatalf("negative cache key contains raw User-Agent: %q", key)
		}
	}
	service.mu.Unlock()
	logs := logBuffer.String()
	if strings.Contains(logs, requestA.ClientUserAgent) || strings.Contains(logs, requestB.ClientUserAgent) {
		t.Fatalf("failure logs contain raw User-Agent: %s", logs)
	}
}

func TestServiceNegativeCacheUsesBackgroundUserAgentForEmptyRequest(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	backgroundUserAgent := "background-agent"
	probeErr := errors.New("probe unavailable")
	prober := &scriptedMediaProber{
		errors:     []error{probeErr},
		userAgents: make(chan string, 1),
	}
	provider := &fakeProvider{prober: prober}
	service := newServiceForTest(t, ServiceOptions{
		Cache: NewCache(nil, now), Store: &fakeRecordStore{}, Provider: provider,
		PlexServerID: "server", BackgroundUserAgent: backgroundUserAgent,
		NegativeTTL: time.Hour, Now: func() time.Time { return now },
	})
	request := testRequest("negative-background-ua", PriorityInteractive)
	request.ClientUserAgent = ""
	if _, err := service.Ensure(t.Context(), request); !errors.Is(err, probeErr) {
		t.Fatalf("empty User-Agent error = %v", err)
	}
	select {
	case got := <-prober.userAgents:
		if got != backgroundUserAgent {
			t.Fatalf("effective User-Agent = %q, want %q", got, backgroundUserAgent)
		}
	case <-time.After(time.Second):
		t.Fatal("probe User-Agent was not captured")
	}

	explicit := request
	explicit.ClientUserAgent = backgroundUserAgent
	if _, err := service.Ensure(t.Context(), explicit); !errors.Is(err, ErrNegativeCache) {
		t.Fatalf("explicit background User-Agent retry error = %v", err)
	}
	if got := provider.calls.Load(); got != 1 {
		t.Fatalf("provider calls = %d, want 1", got)
	}
	service.mu.Lock()
	for key := range service.negative {
		if strings.Contains(key, backgroundUserAgent) {
			service.mu.Unlock()
			t.Fatalf("negative cache key contains raw background User-Agent: %q", key)
		}
	}
	service.mu.Unlock()
}

func TestServiceDoesNotNegativeCacheCanceledProbe(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	prober := &scriptedMediaProber{errors: []error{fmt.Errorf("probe canceled: %w", context.Canceled), nil}}
	provider := &fakeProvider{prober: prober}
	service := newServiceForTest(t, ServiceOptions{
		Cache: NewCache(nil, now), Store: &fakeRecordStore{}, Provider: provider,
		PlexServerID: "server", NegativeTTL: time.Hour, Now: func() time.Time { return now },
	})
	request := testRequest("canceled-probe", PriorityInteractive)
	if _, err := service.Ensure(t.Context(), request); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled probe error = %v", err)
	}
	if _, err := service.Ensure(t.Context(), request); err != nil {
		t.Fatalf("retry after cancellation error = %v", err)
	}
	if got := provider.calls.Load(); got != 2 {
		t.Fatalf("provider calls = %d, want 2", got)
	}
	service.mu.Lock()
	negativeEntries := len(service.negative)
	service.mu.Unlock()
	if negativeEntries != 0 {
		t.Fatalf("negative cache entries after cancellation = %d", negativeEntries)
	}
}

func TestServiceSingleflightsSuccessfulProbesAcrossUserAgents(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	prober := &blockingProber{started: make(chan string, 1), release: make(chan struct{})}
	provider := &fakeProvider{prober: prober}
	service := newServiceForTest(t, ServiceOptions{
		Cache: NewCache(nil, now), Store: &fakeRecordStore{}, Provider: provider,
		PlexServerID: "server", Now: func() time.Time { return now },
	})
	requestA := testRequest("singleflight-ua", PriorityInteractive)
	requestA.ClientUserAgent = "client-agent-a"
	requestB := requestA
	requestB.ClientUserAgent = "client-agent-b"

	flight, err := service.begin(requestA)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-prober.started:
	case <-time.After(time.Second):
		t.Fatal("probe did not start")
	}
	sameFlight, err := service.begin(requestB)
	if err != nil {
		t.Fatal(err)
	}
	if sameFlight != flight {
		t.Fatal("different User-Agent did not join the existing Key flight")
	}
	close(prober.release)
	select {
	case <-flight.done:
		if flight.err != nil {
			t.Fatalf("singleflight error = %v", flight.err)
		}
	case <-time.After(time.Second):
		t.Fatal("singleflight did not finish")
	}
	if got := provider.calls.Load(); got != 1 {
		t.Fatalf("provider calls = %d, want 1", got)
	}
	if _, err := service.Ensure(t.Context(), requestB); err != nil {
		t.Fatalf("successful record lookup with different User-Agent error = %v", err)
	}
	if got := provider.calls.Load(); got != 1 {
		t.Fatalf("provider calls after cached lookup = %d, want 1", got)
	}
}

func TestServiceRetriesFailedSharedFlightWithWaitingUserAgent(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	releaseFirst := make(chan struct{})
	userAgents := make(chan string, 2)
	var calls atomic.Int64
	prober := proberFunc(func(ctx context.Context, _ string, userAgent string) (Media, error) {
		call := calls.Add(1)
		userAgents <- userAgent
		if call == 1 {
			select {
			case <-ctx.Done():
				return Media{}, ctx.Err()
			case <-releaseFirst:
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
	requestA := testRequest("concurrent-failed-ua", PriorityInteractive)
	requestA.ClientUserAgent = "client-agent-a"
	requestB := requestA
	requestB.ClientUserAgent = "client-agent-b"

	resultA := make(chan error, 1)
	go func() {
		_, err := service.Ensure(t.Context(), requestA)
		resultA <- err
	}()
	select {
	case got := <-userAgents:
		if got != requestA.ClientUserAgent {
			t.Fatalf("first probe User-Agent = %q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("first probe did not start")
	}

	waitObserved := make(chan struct{})
	waitContext, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	resultB := make(chan error, 1)
	go func() {
		_, err := service.Ensure(&observedDoneContext{Context: waitContext, observed: waitObserved}, requestB)
		resultB <- err
	}()
	select {
	case <-waitObserved:
	case <-time.After(time.Second):
		t.Fatal("second User-Agent did not join the shared flight")
	}
	close(releaseFirst)

	if err := <-resultA; err == nil || !strings.Contains(err.Error(), "first User-Agent rejected") {
		t.Fatalf("first User-Agent error = %v", err)
	}
	select {
	case got := <-userAgents:
		if got != requestB.ClientUserAgent {
			t.Fatalf("retry probe User-Agent = %q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("waiting User-Agent retry did not start")
	}
	if err := <-resultB; err != nil {
		t.Fatalf("waiting User-Agent retry error = %v", err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("probe calls = %d, want 2", got)
	}
}
