package prewarm

import (
	"context"
	"crypto/sha256"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/InfinityPacer/plex-gateway/internal/mediainfo"
	"github.com/InfinityPacer/plex-gateway/internal/playback"
	"github.com/InfinityPacer/plex-gateway/internal/plexmeta"
)

const (
	defaultDiscoveryTimeout = 15 * time.Second
	defaultTriggerCooldown  = 5 * time.Minute
	defaultRecentLimit      = 4096
	defaultCurrentQueueSize = 64
	defaultWindowGroupLimit = 8
)

type neighborDiscovery interface {
	Neighbors(context.Context, PlaybackContext, int, int) ([]Candidate, error)
}

type partPreparer interface {
	Prepare(plexmeta.Part) playback.Preparation
}

type mediaInfoSubmitter interface {
	Offer(mediainfo.Request) mediainfo.SubmitResult
}

// Metrics receives fixed-cardinality prewarm lifecycle events.
type Metrics interface {
	IncMediaInfoPrewarmTriggered()
	IncMediaInfoPrewarmReplaced()
	IncMediaInfoPrewarmDiscoverySuccess()
	IncMediaInfoPrewarmDiscoveryFailure()
	IncMediaInfoPrewarmFreshCache()
	IncMediaInfoPrewarmJoinedFlight()
	IncMediaInfoPrewarmQueued()
	IncMediaInfoPrewarmRejected()
	IncMediaInfoPrewarmSkipped()
}

// ServiceOptions binds the discovery, STRM preparation, and MediaInfo queues.
// TryEnqueue only changes bounded in-memory state; all collaborators run on a
// worker after the redirect response has been written.
type ServiceOptions struct {
	Discovery        neighborDiscovery
	Playback         partPreparer
	MediaInfo        mediaInfoSubmitter
	Logger           *slog.Logger
	Metrics          Metrics
	DiscoveryTimeout time.Duration
	TriggerCooldown  time.Duration
	BeforeCount      int
	AfterCount       int
	CurrentQueueSize int
	Now              func() time.Time
}

type currentJob struct {
	playback PlaybackContext
	key      string
}

// windowState is the latest-wins queue for one client/session window. The
// state is protected by Service.mu; its worker is counted in Service.workers.
type windowState struct {
	key          string
	pending      *PlaybackContext
	pendingKey   string
	activeKey    string
	activeCancel context.CancelFunc
	wake         chan struct{}
}

// Service schedules immediate current-item analysis independently from
// speculative nearby-item discovery. Current events are FIFO and durable only
// in memory; each nearby window is latest-wins within its own bounded group.
type Service struct {
	discovery        neighborDiscovery
	playback         partPreparer
	mediaInfo        mediaInfoSubmitter
	logger           *slog.Logger
	metrics          Metrics
	discoveryTimeout time.Duration
	cooldown         time.Duration
	beforeCount      int
	afterCount       int
	now              func() time.Time

	ctx     context.Context
	cancel  context.CancelFunc
	done    chan struct{}
	workers sync.WaitGroup

	currentQueue chan currentJob

	mu            sync.Mutex
	currentQueued int
	currentActive int
	currentRecent map[string]time.Time
	windowRecent  map[string]time.Time
	windows       map[string]*windowState
	windowSeq     uint64
	closed        bool
}

// NewService starts the bounded current-item worker. Nearby workers are
// created only for admitted session windows and never exceed the fixed group
// limit.
func NewService(options ServiceOptions) (*Service, error) {
	if options.Playback == nil || options.MediaInfo == nil {
		return nil, errors.New("prewarm dependencies are unavailable")
	}
	logger := options.Logger
	if logger == nil {
		logger = slog.Default()
	}
	discoveryTimeout := options.DiscoveryTimeout
	if discoveryTimeout <= 0 {
		discoveryTimeout = defaultDiscoveryTimeout
	}
	cooldown := options.TriggerCooldown
	if cooldown <= 0 {
		cooldown = defaultTriggerCooldown
	}
	beforeCount := options.BeforeCount
	if beforeCount < 0 {
		return nil, errors.New("prewarm before count is invalid")
	}
	afterCount := options.AfterCount
	if afterCount < 0 {
		return nil, errors.New("prewarm after count is invalid")
	}
	queueSize := options.CurrentQueueSize
	if queueSize < 0 {
		return nil, errors.New("prewarm current queue size is invalid")
	}
	if queueSize == 0 {
		queueSize = defaultCurrentQueueSize
	}
	now := options.Now
	if now == nil {
		now = time.Now
	}
	ctx, cancel := context.WithCancel(context.Background())
	service := &Service{
		discovery: options.Discovery, playback: options.Playback, mediaInfo: options.MediaInfo,
		logger: logger, metrics: options.Metrics, discoveryTimeout: discoveryTimeout,
		cooldown: cooldown, beforeCount: beforeCount, afterCount: afterCount,
		now: now, ctx: ctx, cancel: cancel,
		done: make(chan struct{}), currentQueue: make(chan currentJob, queueSize),
		currentRecent: make(map[string]time.Time), windowRecent: make(map[string]time.Time),
		windows: make(map[string]*windowState),
	}
	service.workers.Add(1)
	go service.currentWorker()
	go func() {
		service.workers.Wait()
		close(service.done)
	}()
	return service, nil
}

// TryEnqueue performs validation and bounded memory operations only. It never
// contacts Plex, reads a STRM file, invokes MediaInfo, or opens SQLite/CDN.
// The current item is admitted independently of RatingKey and of nearby
// discovery, so an incomplete Plex metadata response cannot lose playback.
func (service *Service) TryEnqueue(playbackContext PlaybackContext) bool {
	if service == nil || !validPlaybackContext(playbackContext) {
		return false
	}
	now := service.now()
	currentKey := currentContextKey(playbackContext)
	var startWindow *windowState
	currentAccepted := false
	windowAccepted := false
	replaced := false
	skipped := false

	service.mu.Lock()
	if service.closed {
		service.mu.Unlock()
		return false
	}
	service.expireRecentLocked(service.currentRecent, now)
	service.expireRecentLocked(service.windowRecent, now)

	if until, exists := service.currentRecent[currentKey]; exists && now.Before(until) {
		skipped = true
	} else {
		job := currentJob{playback: playbackContext, key: currentKey}
		select {
		case service.currentQueue <- job:
			service.currentQueued++
			service.rememberRecentLocked(service.currentRecent, currentKey, now)
			currentAccepted = true
		default:
			// The queue is deliberately non-blocking so a redirect cannot wait on
			// a slow MediaInfo backend. The current request remains fail-open.
			skipped = true
		}
	}

	if service.discovery != nil && numericIdentity(playbackContext.RatingKey) && service.beforeCount+service.afterCount > 0 {
		groupKey := service.windowGroupKeyLocked(playbackContext)
		eventKey := groupKey + "\x00" + playbackContextKey(playbackContext)
		if until, exists := service.windowRecent[eventKey]; exists && now.Before(until) {
			skipped = true
		} else if window := service.windows[groupKey]; window != nil {
			if window.activeKey == eventKey || window.pendingKey == eventKey {
				skipped = true
			} else {
				if window.pending != nil || window.activeCancel != nil {
					replaced = true
				}
				copyOfContext := playbackContext
				window.pending = &copyOfContext
				window.pendingKey = eventKey
				service.rememberRecentLocked(service.windowRecent, eventKey, now)
				if window.activeCancel != nil {
					window.activeCancel()
				}
				select {
				case window.wake <- struct{}{}:
				default:
				}
				windowAccepted = true
			}
		} else if len(service.windows) >= defaultWindowGroupLimit {
			skipped = true
		} else {
			copyOfContext := playbackContext
			window := &windowState{
				key: groupKey, pending: &copyOfContext, pendingKey: eventKey,
				wake: make(chan struct{}, 1),
			}
			service.windows[groupKey] = window
			service.rememberRecentLocked(service.windowRecent, eventKey, now)
			service.workers.Add(1)
			startWindow = window
			windowAccepted = true
		}
	}
	service.mu.Unlock()

	if startWindow != nil {
		go service.windowWorker(startWindow)
	}
	if currentAccepted || windowAccepted {
		service.metric(func(metrics Metrics) { metrics.IncMediaInfoPrewarmTriggered() })
	}
	if replaced {
		service.metric(func(metrics Metrics) { metrics.IncMediaInfoPrewarmReplaced() })
	}
	if skipped {
		service.metric(func(metrics Metrics) { metrics.IncMediaInfoPrewarmSkipped() })
	}
	return currentAccepted || windowAccepted
}

func (service *Service) currentWorker() {
	defer service.workers.Done()
	for {
		select {
		case <-service.ctx.Done():
			return
		case job := <-service.currentQueue:
			service.mu.Lock()
			if service.currentQueued > 0 {
				service.currentQueued--
			}
			if service.closed {
				service.mu.Unlock()
				return
			}
			service.currentActive++
			service.mu.Unlock()

			submitted := service.submit(service.ctx, Candidate{
				RatingKey: job.playback.RatingKey,
				Part:      plexmeta.Part{ID: job.playback.PartID},
			}, job.playback.Target, job.playback.UserAgent, mediainfo.PriorityPlayback, "current")

			service.mu.Lock()
			if !submitted {
				delete(service.currentRecent, job.key)
			}
			if service.currentActive > 0 {
				service.currentActive--
			}
			service.mu.Unlock()
		}
	}
}

func (service *Service) windowWorker(window *windowState) {
	defer service.workers.Done()
	for {
		service.mu.Lock()
		if service.closed || service.windows[window.key] != window {
			service.mu.Unlock()
			return
		}
		if window.pending != nil {
			current := *window.pending
			eventKey := window.pendingKey
			window.pending = nil
			window.pendingKey = ""
			jobContext, cancel := context.WithCancel(service.ctx)
			window.activeKey = eventKey
			window.activeCancel = cancel
			service.mu.Unlock()

			service.runWindow(jobContext, current)
			cancel()

			service.mu.Lock()
			if window.activeKey == eventKey {
				window.activeKey = ""
				window.activeCancel = nil
			}
			closed := service.closed
			service.mu.Unlock()
			if closed {
				return
			}
			continue
		}
		if service.windows[window.key] == window && window.pending == nil && window.activeCancel == nil {
			delete(service.windows, window.key)
		}
		service.mu.Unlock()
		return
	}
}

func (service *Service) runWindow(ctx context.Context, current PlaybackContext) {
	if service.beforeCount+service.afterCount == 0 || ctx.Err() != nil {
		return
	}
	discoveryContext, cancel := context.WithTimeout(ctx, service.discoveryTimeout)
	candidates, err := service.discovery.Neighbors(
		discoveryContext, current, service.beforeCount, service.afterCount,
	)
	cancel()
	if err != nil {
		if ctx.Err() == nil {
			service.metric(func(metrics Metrics) { metrics.IncMediaInfoPrewarmDiscoveryFailure() })
			service.logger.Info("mediainfo_prewarm_skipped", "part", current.PartID, "reason", discoveryErrorKind(err))
		}
		service.metric(func(metrics Metrics) { metrics.IncMediaInfoPrewarmSkipped() })
		return
	}
	service.metric(func(metrics Metrics) { metrics.IncMediaInfoPrewarmDiscoverySuccess() })
	for _, candidate := range candidates {
		if ctx.Err() != nil {
			return
		}
		preparation := service.playback.Prepare(candidate.Part)
		if ctx.Err() != nil {
			return
		}
		if preparation.State != playback.PreparationReady {
			service.metric(func(metrics Metrics) { metrics.IncMediaInfoPrewarmSkipped() })
			continue
		}
		service.submit(
			ctx, candidate, preparation.Part.Target, current.UserAgent,
			mediainfo.PriorityNeighbor, "neighbor",
		)
	}
}

func (service *Service) submit(
	ctx context.Context,
	candidate Candidate,
	target, userAgent string,
	priority mediainfo.Priority,
	kind string,
) bool {
	if ctx.Err() != nil {
		return false
	}
	fingerprint, err := mediainfo.FingerprintSTRMTarget(target)
	if err != nil {
		service.metric(func(metrics Metrics) { metrics.IncMediaInfoPrewarmSkipped() })
		return false
	}
	result := service.mediaInfo.Offer(mediainfo.Request{
		Key:       mediainfo.Key{PartID: candidate.Part.ID, STRMFingerprint: fingerprint},
		RatingKey: candidate.RatingKey, Target: target,
		Priority: priority, ClientUserAgent: userAgent,
	})
	switch result.Disposition {
	case mediainfo.SubmitFreshCache:
		service.metric(func(metrics Metrics) { metrics.IncMediaInfoPrewarmFreshCache() })
	case mediainfo.SubmitJoinedExistingFlight:
		service.metric(func(metrics Metrics) { metrics.IncMediaInfoPrewarmJoinedFlight() })
	case mediainfo.SubmitNewlyQueued:
		service.metric(func(metrics Metrics) { metrics.IncMediaInfoPrewarmQueued() })
		service.logger.Info("mediainfo_prewarm_queued", "part", candidate.Part.ID, "kind", kind)
	case mediainfo.SubmitRejected:
		service.metric(func(metrics Metrics) { metrics.IncMediaInfoPrewarmRejected() })
		return false
	default:
		service.metric(func(metrics Metrics) { metrics.IncMediaInfoPrewarmRejected() })
		return false
	}
	return true
}

// Status is a credential-free coordinator snapshot.
type Status struct {
	Available         bool `json:"available"`
	NeighborAvailable bool `json:"neighbor_available"`
	Active            bool `json:"active"`
	Queued            int  `json:"queued"`
	Before            int  `json:"before"`
	After             int  `json:"after"`
}

// Status reports bounded current and window queue state without media
// identity. A window remains counted only while its worker is alive.
func (service *Service) Status() Status {
	if service == nil {
		return Status{}
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	queued := service.currentQueued
	active := service.currentActive > 0
	for _, window := range service.windows {
		if window.pending != nil {
			queued++
		}
		if window.activeCancel != nil {
			active = true
		}
	}
	return Status{
		Available: !service.closed, NeighborAvailable: !service.closed && service.discovery != nil,
		Active: active, Queued: queued,
		Before: service.beforeCount, After: service.afterCount,
	}
}

// Close cancels current and nearby workers and waits for every worker that was
// admitted before the close. The MediaInfo service owns already submitted
// jobs; this coordinator never closes that collaborator.
func (service *Service) Close(ctx context.Context) error {
	if service == nil {
		return nil
	}
	service.mu.Lock()
	if !service.closed {
		service.closed = true
		for _, window := range service.windows {
			window.pending = nil
			window.pendingKey = ""
			if window.activeCancel != nil {
				window.activeCancel()
			}
		}
		service.cancel()
	}
	service.mu.Unlock()
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-service.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (service *Service) expireRecentLocked(recent map[string]time.Time, now time.Time) {
	for key, until := range recent {
		if !now.Before(until) {
			delete(recent, key)
		}
	}
}

func (service *Service) rememberRecentLocked(recent map[string]time.Time, key string, now time.Time) {
	service.expireRecentLocked(recent, now)
	if len(recent) >= defaultRecentLimit {
		var earliestKey string
		var earliest time.Time
		for candidate, until := range recent {
			if earliestKey == "" || until.Before(earliest) {
				earliestKey, earliest = candidate, until
			}
		}
		delete(recent, earliestKey)
	}
	recent[key] = now.Add(service.cooldown)
}

func (service *Service) metric(call func(Metrics)) {
	if service != nil && service.metrics != nil {
		call(service.metrics)
	}
}

// windowGroupKeyLocked chooses an explicit playback window first. A PlayQueue
// ID is a useful fallback for clients that expose it. Without either value we
// allocate a private anonymous group instead of allowing unrelated clients to
// cancel one another; callers that want latest-wins across episode changes
// should set WindowKey to a stable client playback-session identity.
func (service *Service) windowGroupKeyLocked(playbackContext PlaybackContext) string {
	if windowKey := strings.TrimSpace(playbackContext.WindowKey); windowKey != "" {
		digest := sha256.Sum256([]byte("window\x00" + windowKey))
		return string(digest[:])
	}
	if playQueueID := strings.TrimSpace(playbackContext.PlayQueueID); playQueueID != "" {
		digest := sha256.Sum256([]byte("queue\x00" + playQueueID))
		return string(digest[:])
	}
	service.windowSeq++
	return "anonymous\x00" + strconv.FormatUint(service.windowSeq, 10)
}

func validPlaybackContext(playbackContext PlaybackContext) bool {
	return numericIdentity(playbackContext.PartID) &&
		(playbackContext.RatingKey == "" || numericIdentity(playbackContext.RatingKey)) &&
		strings.TrimSpace(playbackContext.Target) != "" && !strings.ContainsAny(playbackContext.Target, "\r\n") &&
		!strings.ContainsAny(playbackContext.UserAgent, "\r\n") &&
		!strings.ContainsAny(playbackContext.WindowKey, "\r\n") &&
		(playbackContext.PlayQueueID == "" || numericIdentity(playbackContext.PlayQueueID)) &&
		(playbackContext.PlayQueueItemID == "" || numericIdentity(playbackContext.PlayQueueItemID))
}

func currentContextKey(playbackContext PlaybackContext) string {
	digest := sha256.Sum256([]byte(playbackContext.PartID + "\x00" + playbackContext.Target + "\x00" + playbackContext.UserAgent))
	return string(digest[:])
}

func playbackContextKey(playbackContext PlaybackContext) string {
	targetDigest := sha256.Sum256([]byte(playbackContext.Target))
	userAgentDigest := sha256.Sum256([]byte(playbackContext.UserAgent))
	windowDigest := sha256.Sum256([]byte(playbackContext.WindowKey))
	return playbackContext.RatingKey + "\x00" + playbackContext.PartID + "\x00" +
		playbackContext.PlayQueueID + "\x00" + playbackContext.PlayQueueItemID + "\x00" +
		string(windowDigest[:]) + "\x00" + string(targetDigest[:]) + "\x00" + string(userAgentDigest[:])
}

func discoveryErrorKind(err error) string {
	switch {
	case errors.Is(err, ErrNoCandidates):
		return "no_candidates"
	case errors.Is(err, ErrUntrustedCurrent):
		return "identity"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	default:
		return "plex"
	}
}
