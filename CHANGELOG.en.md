# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- Plex Web can Direct Play browser-native STRM media while video remains a
  browser-to-CDN 302 path.

## [0.1.2] - 2026-08-30

### Added

- Plex for Apple TV can now identify HDR and Dolby Vision information for STRM
  media. The gateway fills missing MediaInfo while preserving Plex-owned media
  fields and playback decisions.
- STRM video, audio, and subtitle details are detected and cached automatically.
  Playback prioritizes the current item, while an optional Plex management token
  enables nearby-episode prewarming.
- Added a disabled-by-default experimental Apple TV Dolby Vision Profile 5
  playback veto.
- Added a live MediaInfo cache-reset command that backs up the Gateway database
  before clearing the cache without stopping the container.

### Changed

- Long-season browsing is faster. The gateway combines duplicate media-detail
  requests, protects Plex Server, and returns Plex results before filling a cold
  MediaInfo cache in the background.
- MediaInfo, request protection, and sanitized tracing are enabled by default.
  The official image includes ffprobe, stores data under `/app_data`, and loads
  runtime configuration from `app.env`.
- Infuse, Plex iOS, and Plex for Apple TV Direct Play are verified. This release
  does not write to the Plex database, and local media remains transparent.

### Fixed

- Fixed repeated waits and unnecessary Plex requests when remote probing is
  canceled or combined metadata requests fail.

## [0.1.1] - 2026-08-28

### Added

- Added Plex metadata request protection and metrics so opening many items at
  once does not overwhelm Plex Media Server.

### Fixed

- Fixed Apple TV Plex playback sessions failing to start for some STRM items.
  The gateway enters 302 playback only after Plex confirms the current user,
  media selection, and Direct Play decision. Other requests remain transparent.

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
