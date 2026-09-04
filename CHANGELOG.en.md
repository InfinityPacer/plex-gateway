# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.1.4] - 2026-09-04

### Added

- Plex Web can now work with
  [ShimWeave](https://github.com/InfinityPacer/ShimWeave) for more STRM sources
  that need browser-side remuxing or audio compatibility handling. Media still
  travels directly from the CDN to the browser without consuming NAS media
  bandwidth.

### Improved

- Initial playback, long seeks, and clean shutdown were verified with MKV,
  HEVC, and AAC. Local media, ordinary clients, and requests without ShimWeave
  keep their existing Plex behavior.
- Temporary direct URLs can be refreshed during playback, so long sessions and
  seeks do not depend on an expired CDN URL.

### Known limitations

- Actual format support depends on ShimWeave, the browser, the operating system,
  and hardware decoding. This release does not transcode on the Gateway or NAS
  and does not promise every MKV, 4K, HDR, Dolby Vision, or audio combination.
- In the acceptance environment, one 4K HEVC Main 10 with EAC3 source is still
  explicitly reported as unsupported by ShimWeave 0.0.1. Similar sources must
  be verified against the actual browser and hardware.

## [0.1.3] - 2026-08-30

### Added

- Plex Web can now Direct Play browser-native STRM media. Playback, pause,
  resume, and seek are verified. Sources that require DASH, remuxing, or
  audio/video transcoding remain unsupported.

### Improved

- Improved Plex iOS stability when opening STRM libraries, seasons, and item
  details, and removed an incorrect main-feature label.
- Improved 4K, HEVC, HDR, Dolby Vision, and audio technical labels for STRM
  media while preserving information already provided by Plex.

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

[Unreleased]: https://github.com/InfinityPacer/plex-gateway/compare/v0.1.4...HEAD
[0.1.4]: https://github.com/InfinityPacer/plex-gateway/compare/v0.1.3...v0.1.4
[0.1.3]: https://github.com/InfinityPacer/plex-gateway/compare/v0.1.2...v0.1.3
[0.1.2]: https://github.com/InfinityPacer/plex-gateway/compare/v0.1.1...v0.1.2
[0.1.1]: https://github.com/InfinityPacer/plex-gateway/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/InfinityPacer/plex-gateway/releases/tag/v0.1.0
