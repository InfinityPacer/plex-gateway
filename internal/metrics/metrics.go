// Package metrics provides process-local, privacy-preserving gateway counters.
package metrics

import (
	"encoding/json"
	"net/http"
	"sync/atomic"
	"time"
)

// Metrics stores counters for the gateway process. The zero value is ready for
// use; New is provided when a pointer makes ownership clearer at a call site.
//
// The counters intentionally contain no labels. This keeps the metrics
// surface bounded and prevents request credentials or media URLs from being
// copied into an operational endpoint.
type Metrics struct {
	plexRequestsTotal                atomic.Uint64
	cloudPartHits                    atomic.Uint64
	cloudPartMisses                  atomic.Uint64
	redirectSuccess                  atomic.Uint64
	redirectFailure                  atomic.Uint64
	plexFallbackTotal                atomic.Uint64
	activeRequests                   atomic.Int64
	metadataAdmitted                 atomic.Uint64
	metadataTimeouts                 atomic.Uint64
	metadataActive                   atomic.Int64
	metadataQueued                   atomic.Int64
	metadataBatchAdmitted            atomic.Uint64
	metadataBatchTimeouts            atomic.Uint64
	metadataBatchActive              atomic.Int64
	metadataBatchQueued              atomic.Int64
	mediaInfoCacheHits               atomic.Uint64
	mediaInfoCacheMisses             atomic.Uint64
	mediaInfoProbeQueued             atomic.Uint64
	mediaInfoProbeSuccess            atomic.Uint64
	mediaInfoProbeFailure            atomic.Uint64
	mediaInfoStoreFailure            atomic.Uint64
	mediaInfoProbeActive             atomic.Int64
	mediaInfoEnriched                atomic.Uint64
	mediaInfoFailOpen                atomic.Uint64
	mediaInfoWaitActive              atomic.Int64
	mediaInfoWaitRejected            atomic.Uint64
	mediaInfoPrewarmTriggered        atomic.Uint64
	mediaInfoPrewarmReplaced         atomic.Uint64
	mediaInfoPrewarmDiscoverySuccess atomic.Uint64
	mediaInfoPrewarmDiscoveryFailure atomic.Uint64
	mediaInfoPrewarmFreshCache       atomic.Uint64
	mediaInfoPrewarmJoinedFlight     atomic.Uint64
	mediaInfoPrewarmQueued           atomic.Uint64
	mediaInfoPrewarmRejected         atomic.Uint64
	mediaInfoPrewarmSkipped          atomic.Uint64
	resolverLatency                  latencyMetrics
	redirectLatency                  latencyMetrics
	mediaInfoProbeLatency            latencyMetrics
}

type latencyMetrics struct {
	total   atomic.Uint64
	samples atomic.Uint64
	last    atomic.Uint64
	max     atomic.Uint64
}

// Snapshot is the stable JSON representation of the process counters.
// Counter names match the operational metric names used by the gateway.
type Snapshot struct {
	PlexRequestsTotal uint64 `json:"plex_requests_total"`
	CloudPartHits     uint64 `json:"cloud_part_hits"`
	CloudPartMisses   uint64 `json:"cloud_part_misses"`
	RedirectSuccess   uint64 `json:"redirect_success"`
	RedirectFailure   uint64 `json:"redirect_failure"`
	PlexFallbackTotal uint64 `json:"plex_fallback_total"`
	ActiveRequests    int64  `json:"active_requests"`

	MetadataGuardAdmittedTotal            uint64 `json:"metadata_guard_admitted_total"`
	MetadataGuardTimeoutsTotal            uint64 `json:"metadata_guard_timeouts_total"`
	MetadataGuardActive                   int64  `json:"metadata_guard_active"`
	MetadataGuardQueued                   int64  `json:"metadata_guard_queued"`
	MetadataBatchGuardAdmittedTotal       uint64 `json:"metadata_batch_guard_admitted_total"`
	MetadataBatchGuardTimeoutsTotal       uint64 `json:"metadata_batch_guard_timeouts_total"`
	MetadataBatchGuardActive              int64  `json:"metadata_batch_guard_active"`
	MetadataBatchGuardQueued              int64  `json:"metadata_batch_guard_queued"`
	MediaInfoCacheHitsTotal               uint64 `json:"mediainfo_cache_hits_total"`
	MediaInfoCacheMissesTotal             uint64 `json:"mediainfo_cache_misses_total"`
	MediaInfoProbeQueuedTotal             uint64 `json:"mediainfo_probe_queued_total"`
	MediaInfoProbeSuccessTotal            uint64 `json:"mediainfo_probe_success_total"`
	MediaInfoProbeFailureTotal            uint64 `json:"mediainfo_probe_failure_total"`
	MediaInfoStoreFailureTotal            uint64 `json:"mediainfo_store_failure_total"`
	MediaInfoProbeActive                  int64  `json:"mediainfo_probe_active"`
	MediaInfoEnrichedTotal                uint64 `json:"mediainfo_enriched_total"`
	MediaInfoFailOpenTotal                uint64 `json:"mediainfo_fail_open_total"`
	MediaInfoWaitActive                   int64  `json:"mediainfo_wait_active"`
	MediaInfoWaitRejectedTotal            uint64 `json:"mediainfo_wait_rejected_total"`
	MediaInfoPrewarmTriggeredTotal        uint64 `json:"mediainfo_prewarm_triggered_total"`
	MediaInfoPrewarmReplacedTotal         uint64 `json:"mediainfo_prewarm_replaced_total"`
	MediaInfoPrewarmDiscoverySuccessTotal uint64 `json:"mediainfo_prewarm_discovery_success_total"`
	MediaInfoPrewarmDiscoveryFailureTotal uint64 `json:"mediainfo_prewarm_discovery_failure_total"`
	MediaInfoPrewarmFreshCacheTotal       uint64 `json:"mediainfo_prewarm_fresh_cache_total"`
	MediaInfoPrewarmJoinedFlightTotal     uint64 `json:"mediainfo_prewarm_joined_flight_total"`
	MediaInfoPrewarmQueuedTotal           uint64 `json:"mediainfo_prewarm_queued_total"`
	MediaInfoPrewarmRejectedTotal         uint64 `json:"mediainfo_prewarm_rejected_total"`
	MediaInfoPrewarmSkippedTotal          uint64 `json:"mediainfo_prewarm_skipped_total"`

	ResolverLatencyMSTotal uint64 `json:"resolver_latency_ms_total"`
	ResolverLatencySamples uint64 `json:"resolver_latency_samples"`
	ResolverLatencyMSLast  uint64 `json:"resolver_latency_ms_last"`
	ResolverLatencyMSMax   uint64 `json:"resolver_latency_ms_max"`

	RedirectLatencyMSTotal uint64 `json:"redirect_latency_ms_total"`
	RedirectLatencySamples uint64 `json:"redirect_latency_samples"`
	RedirectLatencyMSLast  uint64 `json:"redirect_latency_ms_last"`
	RedirectLatencyMSMax   uint64 `json:"redirect_latency_ms_max"`

	MediaInfoProbeLatencyMSTotal uint64 `json:"mediainfo_probe_latency_ms_total"`
	MediaInfoProbeLatencySamples uint64 `json:"mediainfo_probe_latency_samples"`
	MediaInfoProbeLatencyMSLast  uint64 `json:"mediainfo_probe_latency_ms_last"`
	MediaInfoProbeLatencyMSMax   uint64 `json:"mediainfo_probe_latency_ms_max"`
}

// New creates an empty process-local metrics registry.
func New() *Metrics {
	return &Metrics{}
}

// IncPlexRequests records a request sent through the Plex path.
func (m *Metrics) IncPlexRequests() {
	m.plexRequestsTotal.Add(1)
}

// IncCloudPartHits records a Part cache hit for a cloud-backed media item.
func (m *Metrics) IncCloudPartHits() {
	m.cloudPartHits.Add(1)
}

// IncCloudPartMisses records a Part cache miss.
func (m *Metrics) IncCloudPartMisses() {
	m.cloudPartMisses.Add(1)
}

// IncRedirectSuccess records a successful cloud redirect resolution.
func (m *Metrics) IncRedirectSuccess() {
	m.redirectSuccess.Add(1)
}

// IncRedirectFailure records a failed cloud redirect resolution.
func (m *Metrics) IncRedirectFailure() {
	m.redirectFailure.Add(1)
}

// IncPlexFallback records a request that used Plex after cloud handling could
// not complete.
func (m *Metrics) IncPlexFallback() {
	m.plexFallbackTotal.Add(1)
}

// IncMediaInfoCacheHits records a fresh L1 result.
func (m *Metrics) IncMediaInfoCacheHits() { m.mediaInfoCacheHits.Add(1) }

// IncMediaInfoCacheMisses records an exact key not satisfied by L1.
func (m *Metrics) IncMediaInfoCacheMisses() { m.mediaInfoCacheMisses.Add(1) }

// IncMediaInfoProbeQueued records a newly scheduled singleflight job.
func (m *Metrics) IncMediaInfoProbeQueued() { m.mediaInfoProbeQueued.Add(1) }

// IncMediaInfoProbeSuccess records a complete normalized probe result.
func (m *Metrics) IncMediaInfoProbeSuccess() { m.mediaInfoProbeSuccess.Add(1) }

// IncMediaInfoProbeFailure records a bounded resolver or probe failure.
func (m *Metrics) IncMediaInfoProbeFailure() { m.mediaInfoProbeFailure.Add(1) }

// IncMediaInfoStoreFailure records a result available in L1 but not persisted.
func (m *Metrics) IncMediaInfoStoreFailure() { m.mediaInfoStoreFailure.Add(1) }

// IncMediaInfoProbeActive marks one worker as executing a job.
func (m *Metrics) IncMediaInfoProbeActive() { m.mediaInfoProbeActive.Add(1) }

// DecMediaInfoProbeActive marks one worker job complete.
func (m *Metrics) DecMediaInfoProbeActive() { decrementGauge(&m.mediaInfoProbeActive) }

// ObserveMediaInfoProbeLatency records the complete resolver and ffprobe job.
func (m *Metrics) ObserveMediaInfoProbeLatency(duration time.Duration) {
	m.mediaInfoProbeLatency.observe(duration)
}

// IncMediaInfoEnriched records a Plex metadata response that gained technical
// fields from an identity-matched MediaInfo record.
func (m *Metrics) IncMediaInfoEnriched() { m.mediaInfoEnriched.Add(1) }

// IncMediaInfoFailOpen records an eligible response returned unchanged after
// enrichment could not complete safely.
func (m *Metrics) IncMediaInfoFailOpen() { m.mediaInfoFailOpen.Add(1) }

// IncMediaInfoWaitActive marks one buffered metadata response waiting for an
// identity-matched record.
func (m *Metrics) IncMediaInfoWaitActive() { m.mediaInfoWaitActive.Add(1) }

// DecMediaInfoWaitActive releases one metadata enrichment waiter.
func (m *Metrics) DecMediaInfoWaitActive() { decrementGauge(&m.mediaInfoWaitActive) }

// IncMediaInfoWaitRejected records an eligible response that skipped waiting
// because the bounded enrichment pool was full.
func (m *Metrics) IncMediaInfoWaitRejected() { m.mediaInfoWaitRejected.Add(1) }

// IncMediaInfoPrewarmTriggered records an accepted in-memory redirect event.
func (m *Metrics) IncMediaInfoPrewarmTriggered() { m.mediaInfoPrewarmTriggered.Add(1) }

// IncMediaInfoPrewarmReplaced records a pending or active nearby-item window
// replaced by a newer playback in the same client session.
func (m *Metrics) IncMediaInfoPrewarmReplaced() { m.mediaInfoPrewarmReplaced.Add(1) }

// IncMediaInfoPrewarmDiscoverySuccess records a usable nearby-item window.
func (m *Metrics) IncMediaInfoPrewarmDiscoverySuccess() {
	m.mediaInfoPrewarmDiscoverySuccess.Add(1)
}

// IncMediaInfoPrewarmDiscoveryFailure records a bounded Plex discovery error.
func (m *Metrics) IncMediaInfoPrewarmDiscoveryFailure() {
	m.mediaInfoPrewarmDiscoveryFailure.Add(1)
}

// IncMediaInfoPrewarmFreshCache records a prewarm satisfied by fresh MediaInfo.
func (m *Metrics) IncMediaInfoPrewarmFreshCache() { m.mediaInfoPrewarmFreshCache.Add(1) }

// IncMediaInfoPrewarmJoinedFlight records a prewarm joined to active work.
func (m *Metrics) IncMediaInfoPrewarmJoinedFlight() { m.mediaInfoPrewarmJoinedFlight.Add(1) }

// IncMediaInfoPrewarmQueued records a newly queued MediaInfo probe.
func (m *Metrics) IncMediaInfoPrewarmQueued() { m.mediaInfoPrewarmQueued.Add(1) }

// IncMediaInfoPrewarmRejected records a MediaInfo admission rejection.
func (m *Metrics) IncMediaInfoPrewarmRejected() { m.mediaInfoPrewarmRejected.Add(1) }

// IncMediaInfoPrewarmSkipped records a duplicate, unsafe, unavailable, or
// otherwise non-actionable speculative event.
func (m *Metrics) IncMediaInfoPrewarmSkipped() { m.mediaInfoPrewarmSkipped.Add(1) }

// IncMetadataGuardAdmitted records a detailed metadata request admitted to
// Plex after both client and global limits were acquired.
func (m *Metrics) IncMetadataGuardAdmitted() {
	m.metadataAdmitted.Add(1)
}

// IncMetadataGuardTimeouts records a request rejected after its bounded queue
// wait expired.
func (m *Metrics) IncMetadataGuardTimeouts() {
	m.metadataTimeouts.Add(1)
}

// IncMetadataGuardActive marks one admitted metadata request as active.
func (m *Metrics) IncMetadataGuardActive() {
	m.metadataActive.Add(1)
}

// DecMetadataGuardActive marks one admitted metadata request as complete.
func (m *Metrics) DecMetadataGuardActive() {
	decrementGauge(&m.metadataActive)
}

// IncMetadataGuardQueued marks one metadata request as waiting for admission.
func (m *Metrics) IncMetadataGuardQueued() {
	m.metadataQueued.Add(1)
}

// DecMetadataGuardQueued marks one metadata request as no longer waiting.
func (m *Metrics) DecMetadataGuardQueued() {
	decrementGauge(&m.metadataQueued)
}

// IncMetadataBatchGuardAdmitted records a comma-separated metadata read that
// acquired the batch pool before entering Plex.
func (m *Metrics) IncMetadataBatchGuardAdmitted() {
	m.metadataBatchAdmitted.Add(1)
}

// IncMetadataBatchGuardTimeouts records a batch read rejected after its queue
// wait expired.
func (m *Metrics) IncMetadataBatchGuardTimeouts() {
	m.metadataBatchTimeouts.Add(1)
}

// IncMetadataBatchGuardActive marks one admitted batch read as active.
func (m *Metrics) IncMetadataBatchGuardActive() {
	m.metadataBatchActive.Add(1)
}

// DecMetadataBatchGuardActive marks one admitted batch read as complete.
func (m *Metrics) DecMetadataBatchGuardActive() {
	decrementGauge(&m.metadataBatchActive)
}

// IncMetadataBatchGuardQueued marks one batch read as waiting for admission.
func (m *Metrics) IncMetadataBatchGuardQueued() {
	m.metadataBatchQueued.Add(1)
}

// DecMetadataBatchGuardQueued marks one batch read as no longer waiting.
func (m *Metrics) DecMetadataBatchGuardQueued() {
	decrementGauge(&m.metadataBatchQueued)
}

// ObserveResolverLatency records one MediaVault direct-link lookup without
// attaching request, client, Part, or URL labels.
func (m *Metrics) ObserveResolverLatency(duration time.Duration) {
	m.resolverLatency.observe(duration)
}

// ObserveRedirectLatency records the complete successful cloud control path,
// including Plex Part authorization and MediaVault resolution.
func (m *Metrics) ObserveRedirectLatency(duration time.Duration) {
	m.redirectLatency.observe(duration)
}

// IncActiveRequests marks one request as active.
func (m *Metrics) IncActiveRequests() {
	m.activeRequests.Add(1)
}

// DecActiveRequests marks one request as no longer active. A duplicate close
// or an unmatched decrement is ignored so the exposed gauge cannot become
// negative.
func (m *Metrics) DecActiveRequests() {
	for {
		current := m.activeRequests.Load()
		if current <= 0 {
			return
		}
		if m.activeRequests.CompareAndSwap(current, current-1) {
			return
		}
	}
}

// AddActiveRequests adjusts the active-request gauge. Positive deltas are
// added atomically; negative deltas are clamped at zero.
func (m *Metrics) AddActiveRequests(delta int64) {
	if delta >= 0 {
		m.activeRequests.Add(delta)
		return
	}
	for {
		current := m.activeRequests.Load()
		next := current + delta
		if next < 0 {
			next = 0
		}
		if m.activeRequests.CompareAndSwap(current, next) {
			return
		}
	}
}

// BeginRequest increments the active-request gauge and returns an idempotent
// release function suitable for a defer at the HTTP boundary.
func (m *Metrics) BeginRequest() func() {
	m.IncActiveRequests()
	var released atomic.Bool
	return func() {
		if released.CompareAndSwap(false, true) {
			m.DecActiveRequests()
		}
	}
}

// Snapshot returns a point-in-time, race-free copy of all counters.
func (m *Metrics) Snapshot() Snapshot {
	resolverTotal, resolverSamples, resolverLast, resolverMax := m.resolverLatency.snapshot()
	redirectTotal, redirectSamples, redirectLast, redirectMax := m.redirectLatency.snapshot()
	mediaInfoTotal, mediaInfoSamples, mediaInfoLast, mediaInfoMax := m.mediaInfoProbeLatency.snapshot()
	return Snapshot{
		PlexRequestsTotal: m.plexRequestsTotal.Load(),
		CloudPartHits:     m.cloudPartHits.Load(),
		CloudPartMisses:   m.cloudPartMisses.Load(),
		RedirectSuccess:   m.redirectSuccess.Load(),
		RedirectFailure:   m.redirectFailure.Load(),
		PlexFallbackTotal: m.plexFallbackTotal.Load(),
		ActiveRequests:    m.activeRequests.Load(),

		MetadataGuardAdmittedTotal:            m.metadataAdmitted.Load(),
		MetadataGuardTimeoutsTotal:            m.metadataTimeouts.Load(),
		MetadataGuardActive:                   m.metadataActive.Load(),
		MetadataGuardQueued:                   m.metadataQueued.Load(),
		MetadataBatchGuardAdmittedTotal:       m.metadataBatchAdmitted.Load(),
		MetadataBatchGuardTimeoutsTotal:       m.metadataBatchTimeouts.Load(),
		MetadataBatchGuardActive:              m.metadataBatchActive.Load(),
		MetadataBatchGuardQueued:              m.metadataBatchQueued.Load(),
		MediaInfoCacheHitsTotal:               m.mediaInfoCacheHits.Load(),
		MediaInfoCacheMissesTotal:             m.mediaInfoCacheMisses.Load(),
		MediaInfoProbeQueuedTotal:             m.mediaInfoProbeQueued.Load(),
		MediaInfoProbeSuccessTotal:            m.mediaInfoProbeSuccess.Load(),
		MediaInfoProbeFailureTotal:            m.mediaInfoProbeFailure.Load(),
		MediaInfoStoreFailureTotal:            m.mediaInfoStoreFailure.Load(),
		MediaInfoProbeActive:                  m.mediaInfoProbeActive.Load(),
		MediaInfoEnrichedTotal:                m.mediaInfoEnriched.Load(),
		MediaInfoFailOpenTotal:                m.mediaInfoFailOpen.Load(),
		MediaInfoWaitActive:                   m.mediaInfoWaitActive.Load(),
		MediaInfoWaitRejectedTotal:            m.mediaInfoWaitRejected.Load(),
		MediaInfoPrewarmTriggeredTotal:        m.mediaInfoPrewarmTriggered.Load(),
		MediaInfoPrewarmReplacedTotal:         m.mediaInfoPrewarmReplaced.Load(),
		MediaInfoPrewarmDiscoverySuccessTotal: m.mediaInfoPrewarmDiscoverySuccess.Load(),
		MediaInfoPrewarmDiscoveryFailureTotal: m.mediaInfoPrewarmDiscoveryFailure.Load(),
		MediaInfoPrewarmFreshCacheTotal:       m.mediaInfoPrewarmFreshCache.Load(),
		MediaInfoPrewarmJoinedFlightTotal:     m.mediaInfoPrewarmJoinedFlight.Load(),
		MediaInfoPrewarmQueuedTotal:           m.mediaInfoPrewarmQueued.Load(),
		MediaInfoPrewarmRejectedTotal:         m.mediaInfoPrewarmRejected.Load(),
		MediaInfoPrewarmSkippedTotal:          m.mediaInfoPrewarmSkipped.Load(),

		ResolverLatencyMSTotal: resolverTotal,
		ResolverLatencySamples: resolverSamples,
		ResolverLatencyMSLast:  resolverLast,
		ResolverLatencyMSMax:   resolverMax,

		RedirectLatencyMSTotal: redirectTotal,
		RedirectLatencySamples: redirectSamples,
		RedirectLatencyMSLast:  redirectLast,
		RedirectLatencyMSMax:   redirectMax,

		MediaInfoProbeLatencyMSTotal: mediaInfoTotal,
		MediaInfoProbeLatencySamples: mediaInfoSamples,
		MediaInfoProbeLatencyMSLast:  mediaInfoLast,
		MediaInfoProbeLatencyMSMax:   mediaInfoMax,
	}
}

func decrementGauge(gauge *atomic.Int64) {
	for {
		current := gauge.Load()
		if current <= 0 {
			return
		}
		if gauge.CompareAndSwap(current, current-1) {
			return
		}
	}
}

func (m *latencyMetrics) observe(duration time.Duration) {
	milliseconds := duration.Milliseconds()
	if milliseconds < 0 {
		milliseconds = 0
	}
	value := uint64(milliseconds)
	m.total.Add(value)
	m.samples.Add(1)
	m.last.Store(value)
	for {
		current := m.max.Load()
		if value <= current || m.max.CompareAndSwap(current, value) {
			return
		}
	}
}

func (m *latencyMetrics) snapshot() (total, samples, last, max uint64) {
	return m.total.Load(), m.samples.Load(), m.last.Load(), m.max.Load()
}

// Handler returns an HTTP handler that serves the current snapshot as JSON.
// Only GET is accepted because the endpoint is read-only.
func (m *Metrics) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method_not_allowed"})
			return
		}
		writeJSON(w, http.StatusOK, m.Snapshot())
	})
}

// NewHandler returns a JSON metrics handler. A nil registry is treated as an
// empty registry to keep endpoint wiring safe during optional initialization.
func NewHandler(m *Metrics) http.Handler {
	if m == nil {
		m = New()
	}
	return m.Handler()
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	// The values written here are fixed-shape numeric snapshots or a fixed
	// method error, so encoding cannot expose request-scoped secrets.
	_ = json.NewEncoder(w).Encode(value)
}
