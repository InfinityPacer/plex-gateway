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
- Optional admission guards limit single-item `GET` and `HEAD`
  `/library/metadata/{ratingKey}` requests and comma-separated batch metadata
  reads before they enter Plex. Single-item requests use global and per-client
  limits; batch reads use a separate global pool so maintenance traffic cannot
  consume interactive metadata slots. Neither pool caches or rewrites Plex
  responses. Library listings, metadata mutations, playback paths, timeline,
  and watch-state traffic do not enter these pools.
- Successful XML or JSON Plex responses are observed to populate the in-memory
  `Part.id` cache without changing their bytes. When MediaInfo is available, an
  authenticated single-item metadata response may also fill missing whitelisted
  technical fields from the exact STRM fingerprint. The transform is bounded,
  never creates Stream identities, and preserves the original Plex response on
  timeout, ambiguity, unsupported encoding, or any other failure.
- `GET` and `HEAD` requests below `/library/parts/{partID}/...` are eligible for
  interception only when the cached `Part.file` has a configured cloud
  extension and maps to a readable local STRM file.
- Before a redirect, Plex authorizes the same Part using the current request's
  credentials. The probe never reads its response body; a Plex 3xx is accepted
  only when its `Location` matches the validated local STRM control target.
- A Plex universal playback decision is adapted only after an authenticated
  metadata read identifies the exact `Media[mediaIndex].Part[partIndex]` as a
  mapped STRM Part. The gateway changes only `directPlay=1` and
  `directStream=1`; all client profile parameters, credentials, headers, and
  Plex's response remain intact. The complete XML or JSON decision response is
  observed without rewriting it; a start grant exists only when the same Part
  is explicitly marked `directplay`.
- Eligible universal `start`, `start.mpd`, and `start.m3u8` requests repeat the
  Part authorization using the exact selection captured by a short-lived Direct
  Play decision grant for the same client session and Media/Part, then resolve
  to the final CDN URL. The start path does not repeat the metadata lookup or
  STRM read. Local media and every uncertain selection remain ordinary Plex
  traffic.
- Timeline, scrobble, sessions, playlists, subtitles, images, transcode
  segments, and every unrecognized endpoint remain ordinary Plex traffic.

## Code ownership

| Package | Responsibility |
| --- | --- |
| `cmd/plex-gateway` | Configuration, dependency assembly, and process lifecycle. |
| `internal/database` | Gateway SQLite connection lifecycle and independent per-module migrations. |
| `internal/gateway` | Plex routing, transparent proxying, protocol probes, response capture, 302/fallback output, and request metrics. |
| `internal/mediainfo` | Bounded analysis scheduling, L1/persistent cache policy, provider contracts, and technical-media records. |
| `internal/playback` | Shared STRM preparation, normalized playback attempts, Plex authorization/MediaVault resolution sequencing, and decision-grant state. |
| `internal/plexmeta` | XML and JSON Part/decision readers plus whitelisted metadata projection. |
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
| Technical MediaInfo | A validated producer record, or Gateway SQLite for fallback-probed media; Plex fields and response enrichment are projections. | Source or STRM fingerprint change, schema change, or freshness policy. |
| Part cache | Derived by metadata observation or an authenticated playback selection, keyed by `Part.id`. | Configured TTL or process restart. |
| Direct Play grant | Published only after a complete Plex decision explicitly accepts the exact Part; a new related decision revokes the previous grant first. | Five-minute TTL, bounded eviction, or explicit revocation. |
| MediaVault direct URL | Produced for the active authorized request and copied only to `Location`. | Never persisted or reused by the gateway. |

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

The complete client request header set is cloned onto every MediaVault
`/redirect` request, including bounded internal redirect hops. Query-carried
`X-Plex-*` context is promoted to headers when the client did not already send
the corresponding header. Direct-link resolution is always an internal `GET`
control request, so a client `HEAD` can still receive the same gateway 302 even
when MediaVault does not implement `HEAD`. Because Plex credentials and cookies
are included, the configured MediaVault origin is a trusted upstream and must
not point to an untrusted service.

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

Cloud STRM support is Direct Play only. The decision adapter opts an eligible
Part into Plex's own Direct Play decision path; it does not synthesize a
decision, create a transcode session, or send a cloud URL to Plex Transcoder.
Universal start routes are redirected only after the same STRM selection and
authorization checks; manifest segments and genuine transcode traffic remain
Plex-owned.

## Client compatibility boundary

The redirect plane requires a client that can consume the final raw media URL.
Native players are not subject to browser cross-origin fetch enforcement and
may accept redirects from either Part or eligible universal start routes.

Plex Web requests `start.mpd` as a DASH manifest. A redirect-only gateway
cannot make the final media origin emit browser CORS headers, and an ordinary
MP4 or MKV response is not a DASH manifest. The gateway therefore does not
claim cloud STRM support for Plex Web. Proxying media bytes, repackaging media,
or modifying the Plex Web client would violate or expand the playback-plane
boundary; none is enabled by this project. Local media remains transparently
proxied and retains Plex's normal Web compatibility.

## Analysis-plane boundary

MediaVault's documented ffprobe assistance is an internal recognition and
renaming feature, and MediaInfoKeeper is Emby-specific. Neither is treated as a
public MediaInfo API. The gateway does not inspect MediaVault internals or
duplicate those features in the playback plane.

MediaInfo Phase D begins with a provider boundary, L1 and SQLite persistence,
bounded remote probing, proactive prewarming, and metadata response enrichment.
A stable MediaVault or MoviePilot provider can replace remote probing without
changing playback. Plex persistence remains a feasibility question across an
official API, another supported PMS interface, and an isolated database helper.
Intro and credits work starts with an isolated Plex marker API probe. See
[media-lifecycle-architecture.md](media-lifecycle-architecture.md).

## Cache lifecycle

The Part cache is in memory and has a configurable TTL. Metadata responses
refresh entries by stable `Part.id`; the full `Part.key` is retained only as
observed metadata because its change stamp may vary. A restart begins with an
empty cache; normal metadata, decision, start, and play queue traffic repopulate
it. A direct cold-start Part request falls back to Plex until one of those
authenticated metadata paths observes the Part.

Direct URLs are intentionally not persisted. They are short-lived credentials
owned by MediaVault and the cloud provider.

## Deployment boundary

The gateway requires:

- network access to Plex and MediaVault;
- read-only mounts for each configured STRM tree;
- no Plex database, MediaVault database, 115 cookie, or cloud-provider API
  access. Playback requires no static Plex Token; MediaInfo discovery and
  prewarming may optionally use `PLEX_TOKEN` injected from an ignored
  deployment `app.env`.

Before listening, the process verifies that a fresh invalid Plex Token is
rejected by the internal Plex origin. Plex **Allowed Networks** must not include
the Gateway address or Docker subnet; otherwise transparent proxy requests
would inherit network-level anonymous access and startup fails closed. Cloud
playback failures after startup remain fail-open to authenticated Plex traffic.

The MediaVault API key documented for `/api/v1` integrations is not required by
the STRM `/redirect` contract and is not a gateway configuration value. Active
playback authorization comes only from the client request; the optional
management Token is used only for background discovery, prewarming, and
reconciliation. Users do not provide their Plex username or password to the
gateway.

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
