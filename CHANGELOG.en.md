# Changelog

[简体中文](CHANGELOG.md)

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

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

[Unreleased]: https://github.com/InfinityPacer/plex-gateway/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/InfinityPacer/plex-gateway/releases/tag/v0.1.0
