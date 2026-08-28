package prewarm

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/InfinityPacer/plex-gateway/internal/mediainfo"
	"github.com/InfinityPacer/plex-gateway/internal/playback"
	"github.com/InfinityPacer/plex-gateway/internal/plexmeta"
)

type discoveryFunc func(context.Context, PlaybackContext, int, int) ([]Candidate, error)

func (function discoveryFunc) Neighbors(ctx context.Context, current PlaybackContext, before, after int) ([]Candidate, error) {
	return function(ctx, current, before, after)
}

type preparerFunc func(plexmeta.Part) playback.Preparation

func (function preparerFunc) Prepare(part plexmeta.Part) playback.Preparation { return function(part) }

type submitterFunc func(mediainfo.Request) error

func (function submitterFunc) SubmitDetailed(request mediainfo.Request) mediainfo.SubmitResult {
	if err := function(request); err != nil {
		return mediainfo.SubmitResult{Disposition: mediainfo.SubmitRejected, Err: err}
	}
	return mediainfo.SubmitResult{Disposition: mediainfo.SubmitNewlyQueued}
}

type detailedSubmitterFunc func(mediainfo.Request) mediainfo.SubmitResult

func (function detailedSubmitterFunc) SubmitDetailed(request mediainfo.Request) mediainfo.SubmitResult {
	return function(request)
}

func TestServiceTryEnqueueDoesNoIOInline(t *testing.T) {
	discoveryStarted := make(chan struct{})
	releaseDiscovery := make(chan struct{})
	submitted := make(chan mediainfo.Request, 1)
	service := newTestService(t,
		discoveryFunc(func(context.Context, PlaybackContext, int, int) ([]Candidate, error) {
			close(discoveryStarted)
			<-releaseDiscovery
			return nil, ErrNoCandidates
		}),
		preparerFunc(func(plexmeta.Part) playback.Preparation {
			t.Fatal("Prepare ran inline")
			return playback.Preparation{}
		}),
		submitterFunc(func(request mediainfo.Request) error {
			submitted <- request
			return nil
		}),
	)
	started := time.Now()
	if !service.TryEnqueue(testPlayback("42", "9", "current")) {
		t.Fatal("TryEnqueue() = false")
	}
	if elapsed := time.Since(started); elapsed > 50*time.Millisecond {
		t.Fatalf("TryEnqueue blocked for %s", elapsed)
	}
	select {
	case request := <-submitted:
		if request.Priority != mediainfo.PriorityInteractive || request.Key.PartID != "9" {
			t.Fatalf("current request = %#v", request)
		}
	case <-time.After(time.Second):
		t.Fatal("current item was not submitted")
	}
	select {
	case <-discoveryStarted:
	case <-time.After(time.Second):
		t.Fatal("background discovery did not start")
	}
	close(releaseDiscovery)
}

func TestServiceSubmitsCurrentImmediatelyAndSpacesNeighbors(t *testing.T) {
	type observed struct {
		request mediainfo.Request
		at      time.Time
	}
	submitted := make(chan observed, 3)
	service := newConfiguredTestService(t, ServiceOptions{
		Discovery: discoveryFunc(func(_ context.Context, _ PlaybackContext, before, after int) ([]Candidate, error) {
			if before != 2 || after != 3 {
				t.Fatalf("window = %d/%d", before, after)
			}
			return []Candidate{
				{RatingKey: "43", Part: plexmeta.Part{ID: "10", File: "/cloud/e03.strm"}},
				{RatingKey: "41", Part: plexmeta.Part{ID: "8", File: "/cloud/e01.strm"}},
			}, nil
		}),
		Playback: preparerFunc(func(part plexmeta.Part) playback.Preparation {
			return playback.Preparation{State: playback.PreparationReady, Part: playback.PreparedPart{
				Part: part, Target: "http://mediavault/redirect/" + part.ID,
			}}
		}),
		MediaInfo: submitterFunc(func(request mediainfo.Request) error {
			submitted <- observed{request: request, at: time.Now()}
			return nil
		}),
		BeforeCount: 2, AfterCount: 3, SubmitInterval: 25 * time.Millisecond,
	})
	started := time.Now()
	service.TryEnqueue(testPlayback("42", "9", "current"))
	first := <-submitted
	second := <-submitted
	third := <-submitted
	if first.request.Key.PartID != "9" || first.request.Priority != mediainfo.PriorityInteractive ||
		second.request.Key.PartID != "10" || second.request.Priority != mediainfo.PriorityBackground ||
		third.request.Key.PartID != "8" || third.request.Priority != mediainfo.PriorityBackground {
		t.Fatalf("submission order = %#v %#v %#v", first.request, second.request, third.request)
	}
	if first.at.Sub(started) > 20*time.Millisecond {
		t.Fatalf("current submission delay = %s", first.at.Sub(started))
	}
	if second.at.Sub(first.at) < 20*time.Millisecond || third.at.Sub(second.at) < 20*time.Millisecond {
		t.Fatalf("neighbor spacing = %s, %s", second.at.Sub(first.at), third.at.Sub(second.at))
	}
}

func TestServiceLatestPlaybackCancelsOlderNeighborDiscovery(t *testing.T) {
	started := make(chan string, 3)
	submitted := make(chan mediainfo.Request, 8)
	service := newTestService(t,
		discoveryFunc(func(ctx context.Context, current PlaybackContext, _, _ int) ([]Candidate, error) {
			started <- current.RatingKey
			if current.RatingKey != "44" {
				<-ctx.Done()
				return nil, ctx.Err()
			}
			return []Candidate{{RatingKey: "45", Part: plexmeta.Part{ID: "12", File: "/cloud/e05.strm"}}}, nil
		}),
		preparerFunc(func(part plexmeta.Part) playback.Preparation {
			return playback.Preparation{State: playback.PreparationReady, Part: playback.PreparedPart{
				Part: part, Target: "http://mediavault/redirect/next",
			}}
		}),
		submitterFunc(func(request mediainfo.Request) error {
			submitted <- request
			return nil
		}),
	)
	service.TryEnqueue(testPlayback("42", "9", "a"))
	if got := <-started; got != "42" {
		t.Fatalf("first discovery = %q", got)
	}
	service.TryEnqueue(testPlayback("43", "10", "b"))
	service.TryEnqueue(testPlayback("44", "11", "c"))

	deadline := time.After(time.Second)
	seenCurrent := map[string]bool{}
	seenCurrentC := false
	seenNeighbor := false
	for !seenCurrentC || !seenNeighbor {
		select {
		case request := <-submitted:
			if request.Priority == mediainfo.PriorityInteractive {
				seenCurrent[request.Key.PartID] = true
				seenCurrentC = request.Key.PartID == "11" || seenCurrentC
			}
			if request.Key.PartID == "12" && request.RatingKey == "45" && request.ClientUserAgent == "Client/c" {
				seenNeighbor = true
			}
		case <-deadline:
			t.Fatalf("currentC=%v neighbor=%v", seenCurrentC, seenNeighbor)
		}
	}
	for _, partID := range []string{"9", "10", "11"} {
		if !seenCurrent[partID] {
			t.Fatalf("current item %s was not submitted", partID)
		}
	}
}

func TestServiceKeepsNeighborWindowsIndependentAcrossClients(t *testing.T) {
	started := make(chan string, 2)
	release := make(chan struct{})
	service := newTestService(t,
		discoveryFunc(func(ctx context.Context, current PlaybackContext, _, _ int) ([]Candidate, error) {
			started <- current.WindowKey
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-release:
				return nil, ErrNoCandidates
			}
		}),
		preparerFunc(func(plexmeta.Part) playback.Preparation { return playback.Preparation{} }),
		submitterFunc(func(mediainfo.Request) error { return nil }),
	)
	first := testPlayback("42", "9", "first")
	first.WindowKey = "client-a"
	second := testPlayback("43", "10", "second")
	second.WindowKey = "client-b"
	service.TryEnqueue(first)
	service.TryEnqueue(second)
	seen := map[string]bool{}
	for len(seen) < 2 {
		select {
		case key := <-started:
			seen[key] = true
		case <-time.After(time.Second):
			t.Fatalf("started windows = %#v", seen)
		}
	}
	close(release)
}

func TestServiceRetiresCompletedNeighborWindow(t *testing.T) {
	done := make(chan struct{}, 1)
	service := newTestService(t,
		discoveryFunc(func(context.Context, PlaybackContext, int, int) ([]Candidate, error) {
			done <- struct{}{}
			return nil, ErrNoCandidates
		}),
		preparerFunc(func(plexmeta.Part) playback.Preparation { return playback.Preparation{} }),
		submitterFunc(func(mediainfo.Request) error { return nil }),
	)
	service.TryEnqueue(testPlayback("42", "9", "retire"))
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("discovery did not run")
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		service.mu.Lock()
		windowCount := len(service.windows)
		service.mu.Unlock()
		if windowCount == 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("completed neighbor window was not retired")
}

func TestServiceSubmitsCurrentWithoutRatingKey(t *testing.T) {
	submitted := make(chan mediainfo.Request, 1)
	service := newTestService(t,
		discoveryFunc(func(context.Context, PlaybackContext, int, int) ([]Candidate, error) {
			t.Fatal("neighbor discovery must not run without a rating key")
			return nil, nil
		}),
		preparerFunc(func(plexmeta.Part) playback.Preparation { return playback.Preparation{} }),
		submitterFunc(func(request mediainfo.Request) error {
			submitted <- request
			return nil
		}),
	)
	current := testPlayback("", "9", "no-rating-key")
	if !service.TryEnqueue(current) {
		t.Fatal("TryEnqueue() = false")
	}
	select {
	case request := <-submitted:
		if request.Key.PartID != "9" || request.RatingKey != "" {
			t.Fatalf("request = %#v", request)
		}
	case <-time.After(time.Second):
		t.Fatal("current item was not submitted")
	}
}

func TestServiceRecordsDetailedSubmitDisposition(t *testing.T) {
	metrics := &recordingMetrics{}
	results := []mediainfo.SubmitDisposition{
		mediainfo.SubmitFreshCache,
		mediainfo.SubmitJoinedExistingFlight,
		mediainfo.SubmitNewlyQueued,
		mediainfo.SubmitRejected,
	}
	index := 0
	service := newConfiguredTestService(t, ServiceOptions{
		Discovery: discoveryFunc(func(context.Context, PlaybackContext, int, int) ([]Candidate, error) {
			return nil, ErrNoCandidates
		}),
		Playback: preparerFunc(func(plexmeta.Part) playback.Preparation { return playback.Preparation{} }),
		MediaInfo: detailedSubmitterFunc(func(mediainfo.Request) mediainfo.SubmitResult {
			result := mediainfo.SubmitResult{Disposition: results[index]}
			index++
			return result
		}),
		Metrics: metrics, BeforeCount: 0, AfterCount: 0,
	})
	for _, partID := range []string{"1", "2", "3", "4"} {
		service.TryEnqueue(testPlayback("42", partID, "result-"+partID))
	}
	deadline := time.Now().Add(time.Second)
	for metrics.totalSubmits() < len(results) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if metrics.fresh != 1 || metrics.joined != 1 || metrics.queued != 1 || metrics.rejected != 1 {
		t.Fatalf("metrics = %#v", metrics)
	}
}

func TestServiceRetriesCurrentAfterAdmissionRejection(t *testing.T) {
	requests := make(chan mediainfo.Request, 2)
	attempt := 0
	service := newConfiguredTestService(t, ServiceOptions{
		Playback: preparerFunc(func(plexmeta.Part) playback.Preparation { return playback.Preparation{} }),
		MediaInfo: detailedSubmitterFunc(func(request mediainfo.Request) mediainfo.SubmitResult {
			requests <- request
			attempt++
			if attempt == 1 {
				return mediainfo.SubmitResult{Disposition: mediainfo.SubmitRejected, Err: mediainfo.ErrQueueFull}
			}
			return mediainfo.SubmitResult{Disposition: mediainfo.SubmitNewlyQueued}
		}),
		BeforeCount: 0, AfterCount: 0,
	})
	current := testPlayback("42", "9", "retry")
	service.TryEnqueue(current)
	select {
	case <-requests:
	case <-time.After(time.Second):
		t.Fatal("first submission did not run")
	}
	deadline := time.Now().Add(time.Second)
	for service.Status().Active && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !service.TryEnqueue(current) {
		t.Fatal("second TryEnqueue() = false")
	}
	select {
	case <-requests:
	case <-time.After(time.Second):
		t.Fatal("second submission did not run")
	}
}

type recordingMetrics struct {
	mu                                       sync.Mutex
	fresh, joined, queued, rejected, skipped int
}

func (*recordingMetrics) IncMediaInfoPrewarmTriggered()        {}
func (*recordingMetrics) IncMediaInfoPrewarmReplaced()         {}
func (*recordingMetrics) IncMediaInfoPrewarmDiscoverySuccess() {}
func (*recordingMetrics) IncMediaInfoPrewarmDiscoveryFailure() {}
func (metrics *recordingMetrics) IncMediaInfoPrewarmFreshCache() {
	metrics.mu.Lock()
	defer metrics.mu.Unlock()
	metrics.fresh++
}
func (metrics *recordingMetrics) IncMediaInfoPrewarmJoinedFlight() {
	metrics.mu.Lock()
	defer metrics.mu.Unlock()
	metrics.joined++
}
func (metrics *recordingMetrics) IncMediaInfoPrewarmQueued() {
	metrics.mu.Lock()
	defer metrics.mu.Unlock()
	metrics.queued++
}
func (metrics *recordingMetrics) IncMediaInfoPrewarmRejected() {
	metrics.mu.Lock()
	defer metrics.mu.Unlock()
	metrics.rejected++
}
func (metrics *recordingMetrics) IncMediaInfoPrewarmSkipped() {
	metrics.mu.Lock()
	defer metrics.mu.Unlock()
	metrics.skipped++
}
func (metrics *recordingMetrics) totalSubmits() int {
	metrics.mu.Lock()
	defer metrics.mu.Unlock()
	return metrics.fresh + metrics.joined + metrics.queued + metrics.rejected
}

func TestServiceDeduplicatesRecentPlayback(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	done := make(chan struct{}, 1)
	service := newTestService(t,
		discoveryFunc(func(context.Context, PlaybackContext, int, int) ([]Candidate, error) {
			mu.Lock()
			calls++
			mu.Unlock()
			done <- struct{}{}
			return nil, ErrNoCandidates
		}),
		preparerFunc(func(plexmeta.Part) playback.Preparation { return playback.Preparation{} }),
		submitterFunc(func(mediainfo.Request) error { return nil }),
	)
	current := testPlayback("42", "9", "current")
	service.TryEnqueue(current)
	<-done
	for service.Status().Active {
		time.Sleep(time.Millisecond)
	}
	service.TryEnqueue(current)
	time.Sleep(20 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Fatalf("discovery calls = %d", calls)
	}
}

func TestServiceDoesNotDeduplicateChangedTargetOrUserAgent(t *testing.T) {
	called := make(chan PlaybackContext, 3)
	service := newTestService(t,
		discoveryFunc(func(_ context.Context, current PlaybackContext, _, _ int) ([]Candidate, error) {
			called <- current
			return nil, ErrNoCandidates
		}),
		preparerFunc(func(plexmeta.Part) playback.Preparation { return playback.Preparation{} }),
		submitterFunc(func(mediainfo.Request) error { return nil }),
	)
	base := testPlayback("42", "9", "base")
	service.TryEnqueue(base)
	<-called
	for service.Status().Active {
		time.Sleep(time.Millisecond)
	}
	changedTarget := base
	changedTarget.Target += "?revision=2"
	service.TryEnqueue(changedTarget)
	<-called
	for service.Status().Active {
		time.Sleep(time.Millisecond)
	}
	changedAgent := changedTarget
	changedAgent.UserAgent = "Client/other"
	service.TryEnqueue(changedAgent)
	<-called
}

func TestServiceAllowsCurrentOnlyWindow(t *testing.T) {
	submitted := make(chan mediainfo.Request, 1)
	service := newConfiguredTestService(t, ServiceOptions{
		Discovery: discoveryFunc(func(context.Context, PlaybackContext, int, int) ([]Candidate, error) {
			t.Fatal("discovery must not run for a zero window")
			return nil, nil
		}),
		Playback: preparerFunc(func(plexmeta.Part) playback.Preparation {
			t.Fatal("neighbor preparation must not run for a zero window")
			return playback.Preparation{}
		}),
		MediaInfo: submitterFunc(func(request mediainfo.Request) error {
			submitted <- request
			return nil
		}),
		BeforeCount: 0, AfterCount: 0, SubmitInterval: time.Millisecond,
	})
	service.TryEnqueue(testPlayback("42", "9", "current-only"))
	select {
	case request := <-submitted:
		if request.Key.PartID != "9" || request.Priority != mediainfo.PriorityInteractive {
			t.Fatalf("request = %#v", request)
		}
	case <-time.After(time.Second):
		t.Fatal("current item was not submitted")
	}
}

func newTestService(t *testing.T, discovery neighborDiscovery, preparer partPreparer, submitter mediaInfoSubmitter) *Service {
	t.Helper()
	return newConfiguredTestService(t, ServiceOptions{
		Discovery: discovery, Playback: preparer, MediaInfo: submitter,
		BeforeCount: 2, AfterCount: 3, SubmitInterval: time.Millisecond,
	})
}

func newConfiguredTestService(t *testing.T, options ServiceOptions) *Service {
	t.Helper()
	if options.Logger == nil {
		options.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if options.DiscoveryTimeout == 0 {
		options.DiscoveryTimeout = time.Second
	}
	if options.TriggerCooldown == 0 {
		options.TriggerCooldown = time.Minute
	}
	service, err := NewService(options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := service.Close(ctx); err != nil && !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	})
	return service
}

func testPlayback(ratingKey, partID, suffix string) PlaybackContext {
	return PlaybackContext{
		RatingKey: ratingKey, PartID: partID,
		Target:    "http://mediavault/redirect/" + suffix,
		WindowKey: "test-session",
		UserAgent: "Client/" + suffix,
	}
}
