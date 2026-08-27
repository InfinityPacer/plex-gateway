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
	plexRequestsTotal     atomic.Uint64
	cloudPartHits         atomic.Uint64
	cloudPartMisses       atomic.Uint64
	redirectSuccess       atomic.Uint64
	redirectFailure       atomic.Uint64
	plexFallbackTotal     atomic.Uint64
	activeRequests        atomic.Int64
	metadataAdmitted      atomic.Uint64
	metadataTimeouts      atomic.Uint64
	metadataActive        atomic.Int64
	metadataQueued        atomic.Int64
	metadataBatchAdmitted atomic.Uint64
	metadataBatchTimeouts atomic.Uint64
	metadataBatchActive   atomic.Int64
	metadataBatchQueued   atomic.Int64
	resolverLatency       latencyMetrics
	redirectLatency       latencyMetrics
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

	MetadataGuardAdmittedTotal      uint64 `json:"metadata_guard_admitted_total"`
	MetadataGuardTimeoutsTotal      uint64 `json:"metadata_guard_timeouts_total"`
	MetadataGuardActive             int64  `json:"metadata_guard_active"`
	MetadataGuardQueued             int64  `json:"metadata_guard_queued"`
	MetadataBatchGuardAdmittedTotal uint64 `json:"metadata_batch_guard_admitted_total"`
	MetadataBatchGuardTimeoutsTotal uint64 `json:"metadata_batch_guard_timeouts_total"`
	MetadataBatchGuardActive        int64  `json:"metadata_batch_guard_active"`
	MetadataBatchGuardQueued        int64  `json:"metadata_batch_guard_queued"`

	ResolverLatencyMSTotal uint64 `json:"resolver_latency_ms_total"`
	ResolverLatencySamples uint64 `json:"resolver_latency_samples"`
	ResolverLatencyMSLast  uint64 `json:"resolver_latency_ms_last"`
	ResolverLatencyMSMax   uint64 `json:"resolver_latency_ms_max"`

	RedirectLatencyMSTotal uint64 `json:"redirect_latency_ms_total"`
	RedirectLatencySamples uint64 `json:"redirect_latency_samples"`
	RedirectLatencyMSLast  uint64 `json:"redirect_latency_ms_last"`
	RedirectLatencyMSMax   uint64 `json:"redirect_latency_ms_max"`
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
	return Snapshot{
		PlexRequestsTotal: m.plexRequestsTotal.Load(),
		CloudPartHits:     m.cloudPartHits.Load(),
		CloudPartMisses:   m.cloudPartMisses.Load(),
		RedirectSuccess:   m.redirectSuccess.Load(),
		RedirectFailure:   m.redirectFailure.Load(),
		PlexFallbackTotal: m.plexFallbackTotal.Load(),
		ActiveRequests:    m.activeRequests.Load(),

		MetadataGuardAdmittedTotal:      m.metadataAdmitted.Load(),
		MetadataGuardTimeoutsTotal:      m.metadataTimeouts.Load(),
		MetadataGuardActive:             m.metadataActive.Load(),
		MetadataGuardQueued:             m.metadataQueued.Load(),
		MetadataBatchGuardAdmittedTotal: m.metadataBatchAdmitted.Load(),
		MetadataBatchGuardTimeoutsTotal: m.metadataBatchTimeouts.Load(),
		MetadataBatchGuardActive:        m.metadataBatchActive.Load(),
		MetadataBatchGuardQueued:        m.metadataBatchQueued.Load(),

		ResolverLatencyMSTotal: resolverTotal,
		ResolverLatencySamples: resolverSamples,
		ResolverLatencyMSLast:  resolverLast,
		ResolverLatencyMSMax:   resolverMax,

		RedirectLatencyMSTotal: redirectTotal,
		RedirectLatencySamples: redirectSamples,
		RedirectLatencyMSLast:  redirectLast,
		RedirectLatencyMSMax:   redirectMax,
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
