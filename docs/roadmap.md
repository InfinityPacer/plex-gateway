# Roadmap

The project advances only when the preceding evidence gate is satisfied. Each
phase should remain independently reviewable and must preserve Plex as the sole
metadata and watch-state authority.

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
only those two flags produced Plex's normal Direct Play decision and Part key.

The compatibility adapter therefore:

- performs an authenticated, bounded metadata read using the active client;
- selects `Metadata -> Media[mediaIndex] -> Part[partIndex]` without flattening;
- rewrites only `directPlay` and `directStream` for a mapped STRM Part;
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

## Deferred research

MediaInfo is not an active dependency of redirect playback. The production Plex
snapshot currently exposes STRM Parts without container, codec, resolution, or
stream elements; the current playback plane does not synthesize those fields.

MediaInfo remains a separate analysis-plane project:

1. prefer a stable MediaVault MediaInfo interface if one is published;
2. otherwise evaluate bounded remote `ffprobe` with timeout, probe-size,
   concurrency, and persistent result-cache limits;
3. keep MediaInfo records in gateway-owned storage keyed by a stable Part and
   STRM fingerprint, never by a short-lived signed CDN URL;
4. evaluate client-specific metadata response enrichment without Plex storage
   mutation;
5. do not read or write Plex SQLite.

Marker experiments remain independent of both playback and MediaInfo. Probe the
documented Plex marker API before designing marker production, and keep intro
or credits analysis outside the redirect data plane.

Transcoding, cloud-provider APIs, FUSE, MediaVault internals, and duplication of
MediaVault's STRM/direct-link responsibilities are outside the gateway core.
