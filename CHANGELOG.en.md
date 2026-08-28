# Changelog

[简体中文](CHANGELOG.md)

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- Added bounded remote ffprobe, an L1 LRU plus SQLite MediaInfo cache, and
  single-item Plex metadata response enrichment. Cold-wait timeouts preserve the
  original response while probing continues in the background.
- Current-Part prewarming now starts after a cloud redirect is ready. An isolated
  Plex management token can additionally discover and rate-limit a configurable
  nearby window, defaulting to two previous and three following items. Missing or
  invalid management credentials disable only nearby discovery.
- Added per-client rapid-switch coordination, cross-User-Agent failure handoff,
  and distinct prewarm metrics for fresh cache hits, joined work, new queue
  admissions, and rejections.
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

[Unreleased]: https://github.com/InfinityPacer/plex-gateway/compare/v0.1.1...HEAD
[0.1.1]: https://github.com/InfinityPacer/plex-gateway/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/InfinityPacer/plex-gateway/releases/tag/v0.1.0
