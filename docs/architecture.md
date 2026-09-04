# Architecture

`plex-gateway` is a fail-open Plex protocol adapter. Plex remains the source of
truth for libraries, descriptive metadata, authentication, watch state, and
playback history. Technical MediaInfo belongs to a validated producer or the
Gateway fallback store; any enrichment or Plex write is a rebuildable
projection. The gateway handles only Plex control traffic and redirects eligible
STRM-backed Direct Play requests. Playback media bytes never pass through the
gateway; the isolated analysis worker may read a bounded amount for probing.

## Request ownership

- All requests are transparently proxied to Plex by default.
- Equivalent authenticated single-item metadata `GET` requests can be combined
  within a 20-millisecond window into Plex's native comma-separated endpoint.
  A group requires identical raw query, complete request headers, representation
  context, host, protocol, and remote identity. The default batch contains at
  most 32 unique rating keys and is split
  by rating key into individual XML, JSON, or gzip responses. This is a bounded,
  non-caching protocol transform. `HEAD`,
  Range, conditional requests, bodies, ambiguous credentials, and every
  non-metadata path bypass it.
- Optional admission guards limit direct or fallback single-item `GET` and
  `HEAD` `/library/metadata/{ratingKey}` requests and comma-separated batch
  metadata reads before they enter Plex. Single-item requests use global and
  per-client limits; explicit and synthesized batch reads use a separate global
  pool. A batch failure issues at most one fallback per unique rating key through
  the single-item pool and fans the result out to duplicate callers. Different
  keys remain bounded by the Guard, and batching is temporarily suspended for
  the same request context. The current Guard defaults are global 16, per-client
  16, and batch 4.
- The metadata chain is analysis filter, per-item MediaInfo enrichment,
  coalescer, Guard, and Plex. This ordering lets each original request preserve
  its response projection while one synthesized batch consumes one batch Guard
  slot. Library listings, metadata mutations, playback paths, timeline, and
  watch-state traffic do not enter the coalescer or Guard pools.
- Successful XML or JSON Plex responses are observed to populate the in-memory
  `Part.id` cache without changing their bytes. When MediaInfo is available, an
  authenticated single-item metadata response may also fill missing whitelisted
  technical fields from the exact STRM fingerprint. A Part with no Stream
  elements may receive descriptive Streams identified only by ffprobe stream
  type and source index. The gateway never invents Plex Stream IDs or playback
  selection state, and an existing Stream set is never expanded. The transform
  is bounded and preserves the original Plex response on timeout, ambiguity,
  unsupported encoding, or any other failure. Background library crawlers
  identified by `skipRefresh` and a `-Library` product consume cache only. Hot
  records remain concurrently projectable. An eligible browsing miss preserves
  the Plex response and may only offer bounded P2 work in memory; the request
  does not wait for SQLite, MediaVault, or CDN work. A successful background
  probe persists to L1 and SQLite for later responses.
- `GET` and `HEAD` requests below `/library/parts/{partID}/...` are eligible for
  interception only when the cached `Part.file` has a configured cloud
  extension and maps to a readable local STRM file.
- Before a redirect, Plex authorizes the same Part using the current request's
  credentials. The probe never reads its response body; a Plex 3xx is accepted
  only when its `Location` matches the validated local STRM control target.
- A Plex universal playback decision is adapted only after an authenticated
  metadata read identifies the exact `Media[mediaIndex].Part[partIndex]` as a
  mapped STRM Part. The gateway changes only `directPlay=1`, `directStream=1`,
  and `hasMDE=1`; all unrelated client profile parameters, credentials, and
  headers remain intact. Plex produces the complete XML or JSON decision
  response. The gateway may then apply the same fail-open, whitelisted technical
  MediaInfo projection used for metadata responses. A start grant exists only
  when Plex explicitly marks the same Part `directplay`.
- After Plex accepts the exact STRM Part for Direct Play, an optional playback
  veto may inspect the fresh MediaInfo record already obtained for response
  enrichment. The built-in veto covers only Plex for Apple TV on tvOS with
  Dolby Vision Profile 5 and BL compatibility ID 0. A match returns a
  Plex-compatible unsupported envelope and creates no Direct Play grant.
- Eligible universal `start`, `start.mpd`, and `start.m3u8` requests repeat the
  Part authorization using the exact selection captured by a short-lived Direct
  Play decision grant for the same client session and Media/Part, then resolve
  to the final CDN URL. The start path does not repeat the metadata lookup or
  STRM read. Local media and every uncertain selection remain ordinary Plex
  traffic.
- A `start.mpd` request that declares ShimWeave `control-v1` is terminated with
  a bodyless control descriptor only after the same Grant and Plex Part
  authorization succeed. The descriptor separates stable content identity, a
  fixed same-origin control path, and a high-entropy bearer carried only in a
  request header. Each controlled Range request may
  overlay only its single `Range` header onto the immutable client context,
  resolves one ephemeral MediaVault URL, and receives that URL in a bodyless
  response header. The browser then creates a separate credential-free CDN
  request. The gateway never follows the URL or handles media bytes.
- Timeline, scrobble, sessions, playlists, subtitles, images, transcode
  segments, and every unrecognized endpoint remain ordinary Plex traffic.
- When Plex Web compatibility is enabled, only successful `GET /web/` and
  `GET /web/index.html` HTML responses are eligible for the bounded shell
  helper injection. Every API response and Plex Web static asset bypasses it.

## Code ownership

| Package | Responsibility |
| --- | --- |
| `cmd/plex-gateway` | Configuration, dependency assembly, and process lifecycle. |
| `internal/database` | Gateway SQLite connection lifecycle and independent per-module migrations. |
| `internal/gateway` | Plex routing, transparent proxying, protocol probes, response capture, 302/fallback output, and request metrics. |
| `internal/mediainfo` | Bounded analysis scheduling, L1/persistent cache policy, provider contracts, and technical-media records. |
| `internal/playback` | Shared STRM preparation, normalized playback attempts, Plex authorization/MediaVault resolution sequencing, and decision-grant state. |
| `internal/plexmeta` | XML and JSON Part/decision readers, native-batch splitting, and whitelisted metadata projection. |
| `internal/partcache` | Derived, expiring `Part.id` index; never an authority for access. |
| `internal/pathmap` | Lexical Plex-to-container path mapping. |
| `internal/resolver` | STRM target validation and the MediaVault `/redirect` protocol. |
| `internal/observe`, `internal/metrics`, `internal/trace` | Bounded cross-cutting observation without playback ownership. |

`gateway` may depend on `playback`; `playback` may depend on `partcache`,
`pathmap`, `plexmeta`, and `resolver`. Those lower-level packages never import
`gateway` or `playback`, which keeps protocol adapters from leaking into the
control-file and metadata readers.

## State ownership

| State | Authority and writer | Invalidation |
| --- | --- | --- |
| Plex library, descriptive metadata, and watch state | Plex only; the gateway forwards timeline/scrobble traffic unchanged. | Plex lifecycle. |
| Technical MediaInfo | A validated producer record, or Gateway SQLite for fallback-probed media; Plex fields and response enrichment are projections. Plex DB production writes are not enabled in this release. | Source or STRM fingerprint change, schema change, or freshness policy. |
| Part cache | Derived by metadata observation or an authenticated playback selection, keyed by `Part.id`. | Configured TTL or process restart. |
| Direct Play grant | Published only after a complete Plex decision explicitly accepts the exact Part; a new related decision revokes the previous grant first. | Five-minute TTL, bounded eviction, or explicit revocation. |
| ShimWeave control ticket | In-memory bearer issued only after a live Direct Play grant and repeated Plex Part authorization. The attempt key is stored as a digest and retained client headers are bounded. | Thirty-minute idle TTL, 24-hour absolute TTL, bounded eviction, or process restart. |
| MediaVault direct URL | Produced for the active authorized request and copied to `Location`, or to the bodyless `control-v1` response header. | Never persisted or reused by the gateway. |

## Multi-volume path mapping

Plex paths and gateway container paths are related by explicit mappings. A
deployment may mount one or many physical volumes; no global `/strm` root is
assumed.

```json
[
  {"plex_prefix":"/media/cloud","local_prefix":"/strm"},
  {"plex_prefix":"/media/archive","local_prefix":"/archive-strm"}
]
```

Mappings use normalized, component-boundary-aware longest-prefix matching.
Ambiguous duplicates, relative paths, and mapped results that escape the local
prefix are rejected at startup. Unmatched paths fall back to Plex.

## MediaVault resolver

The first non-empty STRM line is interpreted as one of these inputs:

1. any MediaVault `/redirect` URL, retaining its path and query, including
   query, compact pickcode, filename-suffixed, share, and provider-specific
   templates;
2. an absolute local path produced by MediaVault's local-path STRM mode;
3. an external HTTP(S) direct URL produced by another supported template.

The gateway does not interpret pickcodes, share identifiers, cloud paths, or
provider credentials. MediaVault owns STRM generation, source-specific path
mapping, cloud authentication, and direct-link resolution.

For URL inputs, only the path and query are retained and the configured
MediaVault origin is used. This prevents arbitrary STRM files from turning the
gateway into a general-purpose SSRF client. Local-path STRM values are sent as
the `path` query parameter so compact and URL-based MediaVault libraries can be
served by the same gateway.

The resolver disables automatic redirect following. A valid upstream 3xx with
an HTTP(S) `Location` becomes one `302 Found` response from the gateway. The
full redirect URL, query values, Plex tokens, cookies, and API keys are never
logged.

The complete client request header set is cloned onto the initial MediaVault
`/redirect` request, including bounded internal redirect hops. Query-carried
`X-Plex-*` context is promoted to headers when the client did not already send
the corresponding header. A ShimWeave control ticket retains that authorized
snapshot within a 64 KiB limit; later calls may replace only one non-empty
`Range` header and cannot replace Plex Token, Cookie, Authorization, or other
identity context. Direct-link resolution is always an internal `GET` control
request, so a client `HEAD` can still receive the same result when MediaVault
does not implement `HEAD`. Because Plex credentials and cookies are included,
the configured MediaVault origin is a trusted upstream and must not point to an
untrusted service.

## Failure policy

Redirect handling is an enhancement. Local media, unreadable or invalid STRM
files, path-mapping failures, MediaVault errors, unsupported redirects, and
resolver timeouts all fall back to the original Plex request. Cache misses fall
back directly to Plex without probing or reading media bytes.
This preserves existing local playback and lets Plex remain available when the
cloud path is degraded. Fail-open behavior is a non-configurable invariant; a
partial strict mode would make different playback paths fail inconsistently.

The optional metadata admission guards are the deliberate exception to this
redirect failure policy. Once enabled, a request that cannot acquire the
applicable limits within its queue timeout receives `429` instead of bypassing
protection and increasing Plex load. This protects Plex control-plane
availability and does not change cloud redirect fallback behavior.

Metadata coalescing remains fail-open through the Guard rather than directly to
Plex. A non-200 batch, missing or duplicate item, unsupported encoding,
oversized body, timeout, or malformed envelope is never interpreted as a
single-item result. Each unique rating key performs at most one fallback with
its original filtered request, and duplicate callers share the captured result;
canceled callers are discarded. When every caller leaves, the upstream batch is
canceled. Synthesized responses remove batch validators,
recompute `Content-Length`, and use private no-cache semantics because their
wire representation differs from the batch response.

Cloud STRM support is Direct Play only. The decision adapter opts an eligible
Part into Plex's own Direct Play decision path; it does not synthesize a
decision, create a transcode session, or send a cloud URL to Plex Transcoder.
Universal start routes are redirected only after the same STRM selection and
authorization checks; manifest segments and genuine transcode traffic remain
Plex-owned.

All clients use the same MediaInfo projection rules. Part and universal-start
redirects never wait for MediaInfo. An L1 hit does not synchronously read SQLite
for the request; access renewal may touch SQLite asynchronously. A playback
decision L1 miss may use the persistent record or one bounded P0 probe, and
timeout or failure preserves the existing Direct Play path. Metadata browsing
never waits for that work: an eligible foreground miss may only be offered to
the bounded P2 scheduler while the original Plex response is returned
immediately. Successful worker results populate L1 and SQLite for later
responses.

Projection bypass is derived only from the current Plex response, not from the
identity of a database writer. Local non-STRM Parts bypass MediaInfo logic. For a
STRM Part, complete Media/Part technical fields, a real media size, and positive
IDs on every Stream prove that the Stream rows are materialized in the Plex data
model. Such item metadata and Direct Play decisions are preserved byte-for-byte
and do not read the STRM, cache, resolver, or probe worker. Optional descriptions
such as language and title are not bypass requirements.

Gateway-generated descriptive Streams intentionally omit Plex IDs and playback
selection state. They can project codec, resolution, HDR/Dolby Vision, bitrate,
channel, and language facts, but they are not equivalent to materialized Plex
Stream rows. Real-client A/B showed that the official iOS client may display the
Media-level 4K and HEVC labels while still showing no audio or omitting a
playback-page technical badge. IDs are not synthesized because clients may use
them for track selection. A single-item offline Plex DB proof of concept restored
the native Stream shape and made the Gateway choose byte-for-byte passthrough;
this is evidence for a future isolated writer, not a production write path in
this release.

## MediaInfo scheduling contract

The in-memory scheduler owns all remote-probe admission. It has three fixed
priority classes:

1. P0 represents the item that reached an actual playback decision or successful
   cloud redirect. Its queue reserves 16 entries and is never displaced by
   background work.
2. P1 represents the configured two previous and three following neighbors of an
   actual playback item. Its queue admits at most 50 entries.
3. P2 represents an eligible foreground metadata miss. Its queue admits at most
   50 entries, while the HTTP request returns the original Plex response.

P1 and P2 pending entries expire after five minutes and share one global
five-second remote start interval after both L1 and SQLite miss. Fresh SQLite
records complete without waiting for that interval. P0 bypasses the interval and
runs before all pending background work, but it does not cancel a probe that has
already started. Expired, rejected, or superseded work never reaches MediaVault
or the CDN and does not create negative-cache state.

The exact task identity is Plex server ID, Part ID, and STRM fingerprint. A
queued or running identity owns one flight across all clients and priorities.
When a higher-priority request observes the same queued identity, the existing
job is promoted in place rather than duplicated. Promotion to P0 refreshes the
unclaimed job with the active playback User-Agent and request identity and
removes its background expiry. A P0 arrival joins an already running flight; if
that attempt fails with a different User-Agent, at most one P0 retry may be
retained. P2 never registers User-Agent fallback probes.

The HTTP metadata path may only offer a P2 task through bounded in-memory work.
SQLite lookup, MediaVault resolution, ffprobe, persistence, and retry execution
belong to workers. Background synchronization identified by `-Library` plus
`skipRefresh` remains cache-only and cannot populate P2. Queue state is not
persisted and is discarded on cache reset, shutdown, or process restart.

Detailed numeric metadata reads pass through an independent analysis-parameter
filter before enrichment, coalescing, and admission control. When enabled, it
removes only `asyncAugmentMetadata`, preserving all retained raw query segments,
headers, and `checkFiles`. Plex therefore retains ownership of synchronous
`Part.accessible` and `Part.exists` fields while browse bursts cannot schedule
asynchronous augmentation. The filter, coalescer, and Guard have separate
switches for compatibility and capacity tests.

`/metrics` keeps aggregate scheduler counters and a fixed, label-free P0/P1/P2
breakdown for offers, admissions, joins, promotions, capacity drops, expiry, and
fingerprint supersession. `/health` reports the current queue depth for each
class. Neither surface contains Part identities, URLs, credentials, or full
User-Agent values.

`PLAYBACK_VETO_ENABLED=false` leaves a nil hook in the decision handler. The
experimental hook is disabled by default. When enabled, it only consumes the
fresh MediaInfo record already obtained by that request and checks the built-in
Apple TV Plex Dolby Vision Profile 5 combination. It has no cache, persistence,
Part interception, remote I/O, or positive allow semantics. Missing or
conflicting facts preserve Plex's Direct Play response.

## Client compatibility boundary

The redirect plane requires a client that can consume the final raw media URL.
Native players are not subject to browser cross-origin fetch enforcement and
may accept redirects from either Part or eligible universal start routes.

ShimWeave integration is negotiated per request and remains outside the normal
redirect path. The gateway owns Plex authorization and temporary URL exchange;
the extension owns browser capability checks, media reads, remuxing, and audio
compatibility handling. Requests without a valid `control-v1` negotiation,
including local media and browser-native playback, retain Plex's original
behavior. Gateway protocol support therefore does not imply that every media
format is playable in a particular browser or device.

Plex Web sets `crossorigin=anonymous` on its HTML media element. When a cloud
Part redirects from the Gateway origin to a CDN without CORS headers, the
browser rejects an otherwise natively playable file before Plex Web falls back
to `start.mpd`. `PLEX_WEB_DIRECT_PLAY_ENABLED=true` modifies only the small Plex
Web shell and loads a versioned same-origin helper. The helper removes
`crossorigin` only when an audio or video source points at the Gateway origin's
`/library/parts/.../file` or `.strm` path. Those are the Part key shapes Plex
uses for STRM control files; ordinary local Parts retain their media extension
and Plex Web behavior. Cloud Parts may follow the existing 302 as ordinary
no-CORS media requests.

The helper is loaded before Plex's application bundles, has no token or media
state, and performs no network request of its own. The shell response is
requested from Plex with identity encoding, buffered up to 2 MiB, and modified
only for a successful HTML response containing `</head>`. Unsupported response
shapes are returned unchanged. Changed responses discard upstream validators
and byte-range metadata and use `no-cache`; the versioned helper itself is
immutable-cacheable. Disabling the feature removes both the shell modification
and helper endpoint.

This compatibility path does not expand the media data plane. The browser still
downloads bytes directly from the final CDN. It only supports containers and
codecs the browser can Direct Play. If Plex Web needs DASH, remuxing, or video
or audio transcoding, an ordinary MP4 or MKV response is not a manifest and this
helper cannot make that path playable. A real A/B confirmed this boundary: Plex
served a local item through DASH with copied video and transcoded audio, while a
STRM `start.mpd` redirect exposed the original MP4 where the browser expected a
DASH manifest and ended with `s1002`. The gateway does not proxy media, package
segments, or synthesize manifests. Local media retains Plex's normal Web
compatibility.

## Playback veto contract

The veto is a best-effort compatibility override, not an authorization or
capability engine. It runs only after Plex has accepted the exact Part for
Direct Play and MediaInfo enrichment already has a complete, fresh record.
Returning a veto replaces that decision response and prevents creation of its
short-lived start grant. It does not block a direct Part request and therefore
must not be treated as a security boundary.

## Analysis-plane boundary

MediaVault's documented ffprobe assistance and upload-time head/tail precache
are internal recognition and scan optimizations, and MediaInfoKeeper is
Emby-specific. None is treated as a public MediaInfo API. The precache covers
only files uploaded through MediaVault and cannot be consumed by the gateway
without a stable interface. The gateway does not inspect MediaVault internals,
mount its private cache, or duplicate those features in the playback plane.

The MediaInfo fallback begins with a provider boundary, L1 and SQLite
persistence, bounded remote probing, proactive prewarming, and metadata response
enrichment.
A provider revision separates records whenever normalized probe semantics
change, preventing an old interpretation from being projected after an upgrade.
A successful ffprobe result that omits format size may perform one same-UA
`bytes=0-0` request against the same direct URL. Only a valid partial response
is accepted, the body is not consumed, and every error preserves the other
probe fields. This fallback remains inside the MediaInfo worker and never enters
the synchronous 302 path.
A stable MediaVault or MoviePilot provider can replace remote probing without
changing playback. A production Plex projection writer remains an explicit
evaluation across an official API, another supported PMS interface, and an
isolated database helper with coverage, version/schema allowlists, consistent
backup, rollback, compare-and-set identity checks, and API readback. The
single-item offline database proof of concept establishes client value and the
Gateway bypass contract only; this release does not select or enable a writer.
Intro and credits work starts with an isolated Plex marker API probe. See
[media-lifecycle-architecture.md](media-lifecycle-architecture.md).

Performance acceptance and reproducible microbenchmarks are defined in
[performance-matrix.md](performance-matrix.md). Local proxying and the 302 path
remain higher priority than analysis coverage; a feature that moves MediaInfo,
SQLite, or CDN I/O into Part or universal-start handling is an architecture
regression.

## Cache lifecycle

The Part cache is in memory and has a configurable TTL. Metadata responses
refresh entries by stable `Part.id`; the full `Part.key` is retained only as
observed metadata because its change stamp may vary. A restart begins with an
empty cache; normal metadata, decision, and play queue traffic repopulate it. A
direct cold-start Part request falls back to Plex until one of those authenticated
metadata paths observes the Part.

Direct URLs are intentionally not persisted. They are short-lived credentials
owned by MediaVault and the cloud provider.

## Deployment boundary

The gateway requires:

- network access to Plex and MediaVault;
- read-only mounts for each configured STRM tree;
- no Plex database, MediaVault database, 115 cookie, or cloud-provider API
  access. Playback and current-item MediaInfo prewarming require no static Plex
  Token; nearby-item discovery may optionally use `PLEX_TOKEN` injected from an
  ignored deployment `app.env`.

Before listening, the process verifies that a fresh invalid Plex Token is
rejected by the internal Plex origin. Plex **Allowed Networks** must not include
the Gateway address or Docker subnet; otherwise transparent proxy requests
would inherit network-level anonymous access and startup fails closed. Cloud
playback failures after startup remain fail-open to authenticated Plex traffic.

The MediaVault API key documented for `/api/v1` integrations is not required by
the STRM `/redirect` contract and is not a gateway configuration value. Active
playback authorization comes only from the client request; the optional
management Token is used only to validate and discover nearby Plex items.
Current-item prewarming does not require it. Users do not provide their Plex
username or password to the gateway.

Client-provided Plex authentication headers and query parameters are forwarded
unchanged to Plex. Cloud playback headers are additionally forwarded to
MediaVault while the internal direct-link lookup uses `GET`. The gateway's
public endpoint should be placed behind HTTPS; Plex and MediaVault may remain
on an internal Docker network.

When the gateway is the exclusive Plex frontend, Plex has no host-published
port. Host ports used by existing Plex clients and by a reverse proxy both map
to the gateway listener, while `PLEX_URL` resolves Plex by its service name on
their shared internal network. This prevents cached host addresses from
bypassing the adapter without requiring client-specific firewall rules. Plex
host networking is incompatible with that topology because Plex would bind the
host's port `32400` itself and remain directly discoverable. A remote Plex or a
separately reachable Plex host port can be used only as a non-exclusive setup;
the gateway cannot prevent clients from selecting that direct route.
