# Changelog

[简体中文](CHANGELOG.md)

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- Added general Plex metadata micro-batching. Single-item GET requests with
  identical authentication, query, and client context can share a native Plex
  batch of 32 items by default and are split back into XML, JSON, or gzip
  responses by rating key.

### Changed

- The metadata analysis filter now removes only `asyncAugmentMetadata` and
  preserves `checkFiles`, retaining Plex `Part.accessible` and `Part.exists`
  semantics.

### Fixed

- Failed batches issue at most one request per unique rating key through the
  existing single-item Guard, fan out that result to duplicate callers, and
  temporarily suspend batching for that request group. Canceled callers do not
  fall back, and canceling all callers terminates the upstream batch.

## [0.1.2] - 2026-08-29

### Added

- Added bounded remote ffprobe, an L1 LRU plus SQLite MediaInfo cache, and
  single-item Plex metadata response enrichment. Browsing misses preserve the
  original response immediately while only playback decisions use a cold wait.
- Current-Part prewarming is submitted to P0 after a cloud redirect is ready. An
  isolated Plex management token can additionally discover a configurable P1
  nearby window, defaulting to two previous and three following items. The
  current job is only prioritized in the queue and
  cannot preempt a probe already running. Missing or invalid management
  credentials disable only nearby discovery.
- Added per-client rapid-switch coordination, cross-User-Agent failure handoff,
  and distinct prewarm metrics for fresh cache hits, joined work, new queue
  admissions, and rejections.
- Unified actual playback, nearby prewarming, and foreground metadata misses
  under one P0/P1/P2 MediaInfo scheduler. P1/P2 use a five-minute pending TTL
  and a global five-second remote-start interval while P0 keeps reserved capacity.
- Parts without Plex Streams now receive descriptive video, audio, and subtitle
  Streams without Plex IDs or playback-selection state, including HDR10, Dolby
  Vision, bit depth, bitrate, channel, and language fields.
- Infuse-style background library synchronization consumes existing MediaInfo
  cache records without turning per-item browsing into full-library cold probes.
- MediaInfo ffprobe records now use a new provider revision so records produced
  by the previous interpretation are not reused.
- When ffprobe omits the total media size, a bounded one-byte Range request with
  the same User-Agent recovers Part size from a valid `Content-Range`. Failures
  preserve the otherwise valid MediaInfo and fail open.
- Updated the background-probe fallback User-Agent to the currently verified
  Infuse Library version.
- Added a disabled-by-default experimental Apple TV Plex Dolby Vision Profile 5
  playback veto. It only reuses fresh MediaInfo already obtained by the
  decision path, stores no state, does not intercept Parts, and starts no
  additional probe.
- Added a public performance matrix. Local media and 302 routes perform no
  MediaInfo analysis I/O, and one decision waits for at most one cold probe.

### Changed

- This release persists fallback MediaInfo only in the gateway's SQLite store.
  Production Plex DB writes remain a next-stage evaluation after coverage,
  compatibility, backup, and rollback requirements are measured; the final API
  or database-helper route is not selected here.

### Fixed

- Single-item metadata browsing misses now preserve the Plex response
  immediately and only perform a non-blocking, exact-key P2 memory offer. The
  request path no longer reads SQLite synchronously, waits for MediaVault/CDN,
  or holds a five-second cold-probe window. Workers check SQLite first, so only
  genuine misses enter the remote-start limiter.
- Set the Metadata Guard defaults to global 8, per-client 4, and batch 3. After
  Plex MediaInfo writes provide high coverage and the resulting load is retested,
  global 16, per-client 4, and batch 3 remains a candidate for evaluation.

## [0.1.1] - 2026-08-28

### Added

- Configurable concurrency protection for individual and batch Plex metadata
  requests to limit native-client metadata fan-out against Plex Media Server.
- Metadata Guard metrics for admitted, queued, active, and timed-out requests.

### Fixed

- Apple TV Plex client Direct Play decisions and playback negotiation for STRM
  media while preserving Plex authorization of client credentials and Parts.

## [0.1.0] - 2026-08-27

### Added

- Transparent Plex reverse proxying with fail-open behavior for unsupported or
  unresolved requests.
- Direct Play redirects for eligible STRM Parts resolved through MediaVault.
- Authenticated handling for Plex universal playback decisions and start
  routes used by supported native clients.
- In-memory Part caching, path mapping, health checks, metrics, redacted
  tracing, and the `plex-probe` feasibility tool.
- Container image and exclusive-gateway Docker deployment guidance.

### Security

- Plex authorizes a cloud Part with the current client credentials before the
  gateway redirects playback.
- Logs and metrics exclude Plex tokens and complete signed media URLs.

[Unreleased]: https://github.com/InfinityPacer/plex-gateway/compare/v0.1.2...HEAD
[0.1.2]: https://github.com/InfinityPacer/plex-gateway/compare/v0.1.1...v0.1.2
[0.1.1]: https://github.com/InfinityPacer/plex-gateway/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/InfinityPacer/plex-gateway/releases/tag/v0.1.0
