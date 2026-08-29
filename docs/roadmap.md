# Roadmap

The project advances only when the preceding evidence gate is satisfied. Each
phase should remain independently reviewable and must preserve Plex as the sole
library, descriptive-metadata, and watch-state authority. Technical MediaInfo
belongs to a validated producer or the Gateway fallback store and is projected
to clients or Plex without changing those ownership boundaries.

## Phase 0: Plex behavior probe

- transparent reverse proxy with opt-in sanitized tracing;
- metadata probe for `Part.id`, `Part.key`, and `Part.file`;
- isolated STRM scan and Part stability observations;
- bounded Infuse request-order capture;
- no Part interception, resolver, or redirect behavior.

Exit gate: `docs/plex-behavior.md` contains enough sanitized runtime evidence to
identify the actual Direct Play endpoint and stable Part identity.

## Phase 1: Direct-play gateway MVP

- in-memory Part cache populated from unmodified Plex responses;
- configurable, multi-volume Plex-to-local STRM path mapping;
- resolver interface and MediaVault query/path URL compatibility;
- single 302 response to the final direct URL;
- current-client Plex authorization before every cloud redirect;
- bodyless Part authorization with strict STRM redirect matching;
- direct Plex fallback on a Part cache miss;
- fail-open fallback to Plex for local media, cache misses, and resolver errors.

Exit gate: local media is unaffected and one isolated cloud movie and episode
pass playback, seek, watch-state, and next-episode acceptance.

## Phase 2: Client compatibility

Phase 2 is complete for the Direct Play paths verified with Infuse, Plex iOS
ExperimentalPlayer, and Plex for Apple TV. Compatibility claims remain limited
to observed runtime evidence; additional client targets are not scheduled.

Runtime evidence from Plex Web and the official mobile client established a
separate compatibility leaf: their original universal decision requested
neither Direct Play nor Direct Stream and Plex rejected an STRM Part before the
client reached `/library/parts`. With the same complete client profile, changing
those playback flags plus `hasMDE` produced Plex's normal Direct Play decision
and Part key.

The compatibility adapter therefore:

- performs an authenticated, bounded metadata read using the active client;
- selects `Metadata -> Media[mediaIndex] -> Part[partIndex]` without flattening;
- rewrites `directPlay`, `directStream`, and `hasMDE` for a mapped STRM Part;
- keeps Plex responsible for the decision response and playback session;
- fails open for local media, ambiguous indices, authorization failures, and
  every unrecognized request.

Official mobile and Plex Web traces then demonstrated that a successful Direct
Play decision can still be followed by `start.mpd` or `start.m3u8` because STRM
metadata has no media streams. The start adapter selects and authorizes the
exact Part captured by the same session's confirmed Direct Play decision grant,
resolves MediaVault, and returns the final CDN 302 without repeating metadata
selection. Production playback has successfully followed this redirect path
with Plex iOS ExperimentalPlayer and Plex for Apple TV. Other official-client
playback paths are not supported by the current compatibility scope.

Plex Web remains outside the cloud-playback compatibility gate. Its browser
player fetches `start.mpd` as a DASH manifest; a redirect to the original media
file can be rejected by browser CORS policy and is not itself a DASH manifest.
Supporting that path would require a media proxy, packaging service, or client
modification, each outside the redirect-only data plane. Local Plex Web media,
manifest segments, and every genuine transcode request remain Plex-owned.

## MediaInfo Phase D

MediaInfo is not a dependency of redirect playback. Eligible single-item
metadata and Direct Play decision responses can be enriched from the fail-open
L1 and SQLite cache. A cold decision may wait up to `MEDIAINFO_COLD_WAIT` for an
interactive probe; missing, slow, or failed probes preserve the original Plex
response. Part and universal-start redirects never wait for MediaInfo. All
clients use the same projection rules. An L1 hit does not synchronously read
SQLite for the request, although access renewal may touch SQLite asynchronously.
A metadata browsing miss returns the original Plex response immediately and may
only offer bounded P2 work in memory. It never waits for SQLite, MediaVault, or
the CDN. Background work writes L1 and SQLite only after a successful probe.
A default-disabled experimental veto may reuse an already available fresh record
for the Apple TV Plex DV Profile 5 combination, but it performs no additional
probe and owns no Part state.

MediaInfo probing and persistence remain isolated from redirect playback. Phase
D prioritizes the Gateway fallback while preserving the broader lifecycle design:

1. use L1 and SQLite for single-instance fallback storage;
2. accept optional `PLEX_TOKEN` from an ignored deployment `app.env` for
   nearby-item discovery; current-item prewarming remains token-independent;
3. use bounded remote `ffprobe` with a default `5s` cold playback-decision wait
   ceiling; metadata browsing does not consume that budget;
4. support exact current-Part analysis and a configurable nearby-item window
   after a cloud redirect is ready. P0 actual playback, P1 nearby items, and P2
   foreground metadata misses share one exact-key scheduler. P1/P2 remote starts
   are rate-limited without blocking Part, universal start, or 302. Queue
   priority cannot preempt a probe already running;
   defer season, show, and configured STRM-root batch tasks;
5. enrich authorized Plex metadata responses first. This release writes fallback
   records only to Gateway SQLite. Production Plex DB writing is a next-stage
   evaluation across an official Plex API, another supported PMS interface, and
   an isolated database helper, with coverage, backup, and rollback tests before
   any path is selected. Parts without Plex Streams may receive descriptive
   ffprobe-indexed Streams, while existing Stream sets are never expanded and
   Plex IDs or playback-selection state are never synthesized. Background
   `-Library` synchronization remains cache-only. Foreground fan-out can only
   offer bounded, expiring P2 work and never waits for that background queue;
6. add Redis or replace SQLite with PostgreSQL only after measured
   multi-instance or capacity requirements exist.

The performance matrix in [performance-matrix.md](performance-matrix.md) is a
delivery gate for this work. Local media must remain transparent, Part and
universal-start routes must remain free of analysis I/O, an L1 hit must not
block on SQLite, and one decision may consume at most one configured cold-wait
budget. The current Guard defaults are global 8, per-client 4, and batch 3.
Equivalent single-item metadata GET requests may use the bounded native Plex
micro-batch adapter before Guard admission; invalid batches return through the
single-item limits. Guard changes remain evidence-driven rather than tied to a
future Plex MediaInfo write strategy.

The complete MoviePilot, MediaVault, 115 share, ownership, seeding, cleanup,
MediaInfo, and projection design is documented in
[media-lifecycle-architecture.md](media-lifecycle-architecture.md). Gateway
fallback is an addition to that design, not a replacement for its Phase A-F
scope.

Marker experiments remain independent of both playback and MediaInfo. Probe the
documented Plex marker API before designing marker production, and keep intro
or credits analysis outside the redirect data plane.

Transcoding, cloud-provider APIs, FUSE, MediaVault internals, and duplication of
MediaVault's STRM/direct-link responsibilities are outside the gateway core.
