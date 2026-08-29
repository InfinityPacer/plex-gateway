# plex-gateway

> **MediaVault**, the cloud STRM direct-play flow in `plex-gateway` is built on
> [MediaVault](https://doc.mediavault.qzz.io/). It consumes STRM files generated
> by MediaVault and resolves them through its `/redirect` interface. MediaVault
> community, [Telegram @mediavault3](https://t.me/mediavault3).

> **Status:** Experimental. The transparent proxy and STRM redirect path are
> implemented, but client compatibility remains intentionally narrow and must
> be validated against each Plex client and playback mode.

`plex-gateway` is a fail-open Plex protocol adapter for Direct Play of
STRM-backed cloud media. Plex remains the metadata, authentication, library,
and watch-state authority. The gateway observes Plex metadata, resolves an
eligible STRM through MediaVault, and returns one HTTP 302 response so the
client downloads media directly from the cloud CDN.

Local Plex media is never rewritten. Cache misses, mapping failures, invalid
STRM files, MediaVault failures, and every unsupported endpoint fall back to
the original Plex request unconditionally.

Before returning a cloud redirect, the gateway asks Plex to authorize the same
Part with the current client's request credentials. A Part cache miss is sent
to Plex unchanged; the gateway never reads a media prefix to infer STRM state.

Some official and third-party clients begin with a universal playback decision
instead of requesting the Part directly. For an authenticated, exactly selected
STRM Part, the gateway preserves the complete client profile and asks Plex to
produce its full Direct Play decision response. The response may then receive
the same fail-open technical MediaInfo projection used for metadata. A
short-lived grant is created only after Plex explicitly marks the same Part as
Direct Play. If the client then requests universal `start`,
`start.mpd`, or `start.m3u8`, the gateway requires that grant from the same
playback session and exact Media/Part, authorizes the Part again, and returns
the same CDN redirect without starting Plex Transcoder or synthesizing Plex
metadata.

The gateway applies the same MediaInfo projection rules to every client. A
disabled-by-default experimental playback veto checks the Plex for Apple TV,
tvOS, Dolby Vision Profile 5 combination only after Plex returns Direct Play
for the same STRM Part and MediaInfo enrichment already has a fresh record. A
match returns a Plex-compatible unsupported decision; every other case keeps
the existing playback path. The veto stores no verdict, does not intercept the
Part path, and starts no additional probe. Apple TV hardware supports Profile
5, while compatibility still varies with Plex version, container, and source,
so the veto remains disabled by default.

Native clients that accept a raw media redirect on those routes can use this
path. Plex Web cloud playback is not supported: its browser player fetches
`start.mpd` as a DASH manifest, while the redirect target is the original media
file and may not permit cross-origin browser requests. Adding CORS headers to
the gateway's redirect cannot change the final media origin's response. Local
media in Plex Web remains ordinary proxied Plex traffic.

## Compatibility status

| Client or path | Status | Boundary |
| --- | --- | --- |
| Local Plex media | Supported by transparent proxy | The gateway does not rewrite local Parts. |
| Infuse Direct Play | Direct Play verified | Uses the Plex Part redirect path. |
| Plex iOS ExperimentalPlayer | Direct Play verified | STRM uses the universal decision/start redirect path. |
| Plex for Apple TV | Direct Play verified | HDR STRM uses the universal decision/start redirect path. Apple TV 4K supports Dolby Vision Profile 5 Direct Play, while Plex version, container, and source combinations may still affect compatibility; the gateway does not reject it by default. |
| Plex Web cloud playback | Unsupported | The browser expects a DASH manifest and enforces final-origin CORS. |

This is an independent community project. It is not affiliated with, endorsed
by, or sponsored by Plex, MediaVault, Infuse, or their respective owners.

## Container image

Release images use
`ghcr.io/infinitypacer/plex-gateway:X.Y.Z` and
`ghcr.io/infinitypacer/plex-gateway:latest`. Pin a version tag when deployments
must not follow the next release automatically. See
[CHANGELOG.en.md](CHANGELOG.en.md)
for user-visible changes.

## Supported STRM targets

- MediaVault query form: `/redirect?path=...&pickcode=...`;
- MediaVault pickcode form: `/redirect/{pickcode}`;
- MediaVault pickcode plus filename form;
- MediaVault share form using `fid` and `source=share:...`;
- absolute local paths used by compact STRM mode;
- third-party HTTP(S) direct URLs, returned to the client without an outbound
  request from the gateway.

MediaVault URL inputs are rewritten to the configured internal MediaVault
origin. Short same-origin `/redirect` chains are resolved inside the gateway;
the first cross-origin HTTP(S) location is returned without contacting the CDN.

## Configuration

The gateway remains a transparent proxy when `MEDIAVAULT_URL` and
`PATH_MAPPINGS` are both unset. Configure them together to enable cloud
redirect handling.

Before using `docker-compose.example.yml`, copy `app.env.example` to `app.env`
and set the deployment addresses, path mappings, and optional token. The
Compose example reads it through `env_file: ./app.env`.

```sh
PLEX_URL=http://plex:32400 \
MEDIAVAULT_URL=http://mediavault:7811 \
PATH_MAPPINGS='[
  {"plex_prefix":"/media/cloud","local_prefix":"/strm"},
  {"plex_prefix":"/media/archive","local_prefix":"/archive-strm"}
]' \
go run ./cmd/plex-gateway
```

Important environment variables:

| Variable | Default | Purpose |
| --- | --- | --- |
| `PLEX_URL` | required | Internal Plex origin; credentials are rejected. |
| `PLEX_TOKEN` | disabled | Optional Plex management token used only for startup validation and nearby-item discovery. Current-item prewarming remains available without it. |
| `MEDIAVAULT_URL` | disabled | Internal MediaVault HTTP(S) origin. |
| `PATH_MAPPINGS` | disabled | JSON array of Plex-to-container STRM prefixes. |
| `LISTEN_ADDR` | `:32400` | Gateway listener. |
| `PART_CACHE_TTL` | `24h` | In-memory Part cache lifetime. |
| `RESOLVER_TIMEOUT` | `15s` | End-to-end MediaVault resolution timeout. |
| `OBSERVE_MAX_BYTES` | `8388608` | Maximum metadata body copied for observation. |
| `PART_PROBE_TIMEOUT` | `15s` | Timeout for the bodyless Plex Part authorization probe. |
| `CLOUD_EXTENSIONS` | `.strm` | Comma-separated cloud control-file extensions. |
| `TRACE_ENABLED` | `true` | Enable sanitized Plex request-order tracing. |
| `METADATA_ANALYSIS_FILTER_ENABLED` | `true` | Remove `asyncAugmentMetadata` from detailed metadata reads to avoid Plex background analysis while preserving `checkFiles`. |
| `METADATA_GUARD_ENABLED` | `true` | Limit single-item detailed metadata requests before they enter Plex. |
| `METADATA_GUARD_GLOBAL_CONCURRENCY` | `16` | Shared detailed metadata concurrency limit across all clients. |
| `METADATA_GUARD_CLIENT_CONCURRENCY` | `16` | Detailed metadata concurrency limit for each Plex client identifier. |
| `METADATA_GUARD_BATCH_ENABLED` | `true` | Limit comma-separated batch metadata reads before they enter Plex. |
| `METADATA_GUARD_BATCH_CONCURRENCY` | `4` | Shared batch metadata concurrency limit across all clients. |
| `METADATA_GUARD_QUEUE_TIMEOUT` | `10s` | Maximum admission wait before returning `429`. |
| `METADATA_COALESCE_ENABLED` | `true` | Combine equivalent authenticated single-item metadata GETs into native Plex batches. |
| `METADATA_COALESCE_WINDOW` | `20ms` | Micro-batch collection window, up to `100ms`. |
| `METADATA_COALESCE_MAX_ITEMS` | `32` | Unique ratingKey limit per synthesized batch, from `2` to `32`. |
| `METADATA_COALESCE_TIMEOUT` | `5s` | Total timeout for the Plex batch request and response split. |
| `MEDIAINFO_ENABLED` | `true` | Enable the MediaInfo cache, bounded probes, and single-item metadata enrichment; initialization failures do not affect transparent proxying. |
| `DATABASE_PATH` | `./data/plex-gateway.db` | Gateway SQLite database path; the container image defaults to `/app_data/plex-gateway.db`. |
| `MEDIAINFO_PLAYBACK_QUEUE_SIZE` | `16` | Independent P0 queue capacity for actual playback work. |
| `MEDIAINFO_NEIGHBOR_QUEUE_SIZE` | `50` | P1 queue capacity for nearby-item prewarming. |
| `MEDIAINFO_METADATA_QUEUE_SIZE` | `50` | P2 queue capacity for foreground metadata misses. |
| `MEDIAINFO_PENDING_TTL` | `5m` | Lifetime of P1/P2 work that has not started. |
| `MEDIAINFO_BACKGROUND_INTERVAL` | `5s` | Global minimum interval between P1/P2 remote probes after L1 and SQLite miss. |
| `MEDIAINFO_USER_AGENT` | `Infuse-Library/8.5.1` | Fallback User-Agent for background probes without an active client context. |
| `MEDIAINFO_COLD_WAIT` | `5s` | Cold-cache wait ceiling for an actual playback decision; metadata browsing misses never use this wait. |
| `PLAYBACK_VETO_ENABLED` | `false` | Enable the experimental Apple TV Plex Dolby Vision Profile 5 veto; it only reuses fresh MediaInfo already obtained by the decision path. |
| `MEDIAINFO_RESPONSE_MAX_BYTES` | `8388608` | Maximum size of one Plex metadata response buffered for enrichment. |
| `MEDIAINFO_ENRICHMENT_CONCURRENCY` | `8` | Maximum single-item metadata responses buffered for concurrent L1 projection. |
| `MEDIAINFO_PREWARM_BEFORE` | `2` | Number of nearby items before the current item to prewarm. |
| `MEDIAINFO_PREWARM_AFTER` | `3` | Number of nearby items after the current item to prewarm. |

Ordinary Plex requests remain transparent, while cloud playback sends client
headers to the trusted MediaVault origin under the contract below. The optional
management `PLEX_TOKEN` is injected through the environment and sent only to the
configured Plex origin for nearby-item discovery. It is not written to the
database, logs, or tasks and is never sent to MediaVault, the CDN, or ffprobe.
Compose deployments can place
it in an ignored `app.env`; see [app.env.example](app.env.example).

For cloud playback, all client request headers are forwarded to
the configured MediaVault origin so it can generate a direct URL for the same
client context. The internal lookup always uses `GET`, allowing a client `HEAD`
request to receive the same 302 even when MediaVault does not support `HEAD`.
Deploy MediaVault as a trusted upstream and keep resolver requests and headers
out of logs.

Clients connect to the gateway as they would connect to Plex and authenticate
through Plex. The gateway does not need a Plex username or password, and
playback does not depend on the management token. MediaVault's API key for `/api/v1` integrations is also not required for
the STRM `/redirect` playback contract.

### Playback veto

With `PLAYBACK_VETO_ENABLED=false`, no veto function is created and the decision
path performs only one nil check. When enabled, the gateway returns an
unsupported decision only for complete MediaInfo matching Plex for Apple TV,
tvOS, Dolby Vision Profile 5, and BL compatibility ID 0. It reuses the fresh
record already obtained by MediaInfo enrichment, performs no additional cache,
SQLite, MediaVault, or CDN work, stores no state, and never enters Part or
universal start handling. Missing, stale, conflicting, or unmatched evidence
always fails open.

This is an experimental switch for deployments with a known source
compatibility issue, not a claim that Apple TV generally lacks Dolby Vision
Profile 5 support. See
[docs/architecture.md](docs/architecture.md) and
[docs/performance-matrix.md](docs/performance-matrix.md).

### MediaInfo response enrichment

For an authenticated single-item STRM metadata request, the gateway can fill
missing Media, Part, and Stream technical fields from its L1 or SQLite record.
When a Plex Part has no Stream elements, ffprobe stream types and source indexes
are used to create descriptive video, audio, and subtitle Streams with fields
such as HDR10, Dolby Vision, bit depth, codec, codecID, bitrate, channel layout, and
language code. Generated Streams never contain Plex Stream IDs or playback
selection fields such as `selected`, `default`, or `decision`. When Plex already
has Streams, only missing fields on identity-matched Streams are filled. Missing
sibling Streams are not created and existing Plex values are not overwritten.
This release writes fallback probe results only to the gateway's SQLite store and
does not write to the Plex DB. Production Plex DB writes are a next-stage
evaluation after coverage, compatibility, backup, and rollback validation.

The L1 hot path returns without synchronously reading SQLite for the request.
Access renewal may touch SQLite asynchronously, so persistence I/O stays out of
the request critical path. Hot-cache projection remains concurrent. A metadata
browsing miss immediately preserves the Plex response and only offers bounded P2
work in memory; it never waits for SQLite, MediaVault, or the CDN. Successful
background work later persists the result to L1 and SQLite. Actual playback
decisions retain an independent P0 cold-probe budget so playback negotiation can
receive technical fields without blocking home or season screens.

When ffprobe cannot report the total media size, the gateway sends one
`Range: bytes=0-0` request to the same temporary direct URL with the same
User-Agent used for that MediaVault resolution and ffprobe task. The request is
capped at two seconds, accepts only a `206` response
with a valid `Content-Range` total, and never reads the media response body.
Timeouts, redirects, full `200` responses, and malformed headers are ignored
without discarding otherwise valid MediaInfo or delaying the 302 playback path.
The recovered size is persisted with the MediaInfo record, so fresh cache hits
do not repeat the CDN request.

Background library synchronization can enumerate an entire library one item at
a time. Requests with `skipRefresh` whose product ends in `-Library` may consume
an existing MediaInfo cache record but never admit a cold probe. Ordinary
single-item cold misses also return the original response immediately and only
offer bounded P2 work; successful cloud 302 and nearby-window work retain
their existing boundaries. Browsing a library therefore cannot expand without
bound into full-library CDN ffprobe work. Any cache miss, timeout, unsupported
structure, or projection failure returns the original Plex response.

### Nearby-item MediaInfo prewarming

The gateway performs one bounded in-memory enqueue only after Plex has
authorized the cloud Part, MediaVault has returned the final direct URL, and the
gateway has written the 302. This signal means that the cloud redirect is ready;
it does not claim that the client followed the redirect or actually started
playback. Current-item prewarming does not require a management token. A valid
`PLEX_TOKEN` additionally enables nearby-item discovery.

The current item enters the P0 actual-playback queue. The background
coordinator prefers explicit Plex playQueue order and permits movie, cross-show,
and multi-Media/Part entries. Every candidate is stored under its own PartID and
STRM fingerprint. The current item can only be prioritized in the queue and
cannot preempt a probe already running. Without a usable queue, Plex episode and
season response order is used, including specials and entries with missing
indices.

The default window is two items before and three after, with following items
submitted first. The combined configurable window is capped at 50. Prepared
neighbors are offered to P1 immediately without blocking. Only genuine L1 and
SQLite misses are subject to the global five-second minimum remote-start
interval, while the MediaInfo worker defaults to one concurrent probe to avoid
MediaVault/CDN bursts. A rapid A/B/C switch submits each new current item to P0
immediately and cancels only old-window entries that were not offered. Offered
or running work retains its own identity and is deduplicated by singleflight. A
running background probe is not preempted by a current item.

The gateway persists parsed MediaInfo, not short-lived CDN URLs or raw head/tail
bytes. MediaVault upload precaching applies only to files uploaded through
MediaVault. A future stable MediaInfo or precache API can become the preferred
provider, with CDN ffprobe as fallback. `MEDIAINFO_NEGATIVE_TTL` suppresses
repeated failures; the coordinator does not perform blind retries without the
final probe error class. Plex discovery, STRM reads, fingerprinting, and SQLite
access all run after the 302 path. A missing or invalid token disables only
nearby-item discovery, not current-item prewarming, transparent proxying,
metadata enrichment, or playback redirects.

### Metadata concurrency protection

Some clients fan out many `GET/HEAD /library/metadata/{ratingKey}` requests
when opening a large library. When `METADATA_GUARD_ENABLED` is enabled, the
gateway applies global and per-client concurrency limits before these
single-item detailed metadata requests enter Plex. Client identifiers are used
only for in-process admission and retained as digests. They are not written to
logs or metrics.

The guard does not cover library listings, timeline or watch-state traffic,
playback decisions, `/library/parts`, or other Plex paths. Requests wait for at
most `METADATA_GUARD_QUEUE_TIMEOUT`. A timed-out request receives `429` instead
of bypassing the guard and reaching Plex. The feature does not cache metadata
or modify Plex responses.

`METADATA_GUARD_BATCH_ENABLED` protects
`GET/HEAD /library/metadata/1,2,...` with a separate global concurrency pool.
Batch reads do not consume the global or per-client slots reserved for
interactive single-item metadata requests, and metadata mutations do not enter
the batch pool.

The current Guard defaults are global 16 for single-item metadata, per-client
16, and batch 4. These values were selected from bounded NAS load tests and
real Apple TV long-season traces; raising them remains an evidence-driven
operator decision.

## Advertise the gateway through Plex

In Plex Web, open the server's **Settings > Network > Custom server access
URLs** and add the externally reachable gateway origin, including its scheme
and public port when the port is not implicit:

```text
https://plex-gateway.example.com:443
```

This is the address Plex should advertise to clients that obtain server
connection information through plex.tv. Enter the client-facing reverse-proxy
or gateway address, not the internal `PLEX_URL` and not the MediaVault address.
Clients still sign in through Plex; the gateway does not receive a separately
configured Plex username or password.

The advertised URL must route every Plex path to the gateway. The gateway then
proxies ordinary control traffic to the internal Plex origin and handles only
the eligible Direct Play paths described below.

### Make the gateway the only reachable Plex frontend

Plex's custom server access URLs add connection candidates; they do not remove
automatically discovered or previously cached addresses. If every client must
pass through the gateway, deployment topology must enforce that ownership:

Plex `network_mode: host` is incompatible with this exclusive mode. It binds
the host's port `32400` before the gateway can use it and exposes a direct Plex
route through LAN discovery, account connection candidates, and client caches.
Clients may then bypass the gateway even when Plex advertises a custom gateway
URL. A custom URL alone does not replace those direct routes.

- put Plex and the gateway on the same user-defined Docker network;
- run Plex in bridge mode rather than host mode;
- do not publish Plex's container port `32400` on the host;
- do not include the gateway address or Docker subnet in Plex **Allowed Networks**;
- publish host port `32400` to the gateway's container port `32400` so existing
  Plex connections also terminate at the gateway;
- configure the gateway's `PLEX_URL` with Plex's internal service name, such as
  `http://plex:32400`, to avoid a loop through the host-facing gateway;
- disable Plex GDM when automatic LAN discovery would expose a direct Plex
  route, and advertise only client-facing gateway URLs in Plex Network
  settings.

```yaml
services:
  plex:
    networks: [media]
    # No host-facing ports. Preserve the deployment's existing volumes,
    # devices, environment, and restart policy.

  plex-gateway:
    ports:
      - "32400:32400"
    environment:
      PLEX_URL: "http://plex:32400"
    networks: [media]

networks:
  media:
    name: media
```

A reverse proxy should target the gateway listener, not Plex. Plex may still
report its private container address to plex.tv; that address must remain
unreachable from client networks. The acceptance condition is that every
reachable advertised or cached Plex address terminates at the gateway.

At startup, the gateway checks whether internal Plex still authenticates the
client by using a fresh invalid Plex Token. It begins listening only when Plex
returns `401` or `403`. If Plex treats the gateway network as unauthenticated
access, the gateway refuses to start instead of inheriting that permission.
Restart the gateway after changing Plex network authentication settings.

Running Plex on another machine, keeping Plex in host mode, or leaving a Plex
host port reachable can still provide an explicit optional gateway endpoint,
but it cannot guarantee that clients will not select a direct Plex connection.
That topology is outside the exclusive-gateway deployment contract.

## Endpoints

- `GET /health` returns process health plus credential-free MediaInfo cache,
  probe-queue, and nearby-item prewarm summaries.
- `GET /metrics` returns fixed-shape JSON counters plus resolver and complete
  redirect-path latency totals, samples, last values, and maxima. When metadata
  protection is enabled, it also reports admission, timeout, active, and queued
  counts. MediaInfo scheduler outcomes are additionally split into fixed
  P0/P1/P2 sections. No metric contains request labels or credentials.
- Every other endpoint follows the Plex proxy or Direct Play interception
  rules described in [docs/architecture.md](docs/architecture.md).

### Resetting the MediaInfo hot and cold caches

Run this command from the Compose project directory:

```sh
./scripts/reset-mediainfo-cache.sh
```

The script hot-clears the cache through a container-loopback maintenance
endpoint, so the Gateway and Plex proxy remain online. It first backs up the
complete SQLite database under `app_data/backups/`, then clears L1, the current
MediaInfo queues, and the rebuildable `media_info_records`. The database,
migration history, and tables owned by other modules are preserved.

Set the Compose service name when it is not `plex-gateway`:

```sh
PLEX_GATEWAY_COMPOSE_SERVICE=gateway ./scripts/reset-mediainfo-cache.sh
```

The image also contains `/usr/local/bin/plex-gateway-reset-mediainfo-cache`, so
it can be invoked directly:

```sh
docker exec plex-gateway plex-gateway-reset-mediainfo-cache
```

The maintenance endpoint accepts only container-local loopback requests. It is
not exposed through the public Gateway endpoint or reverse proxy.

## Probe a Plex item

`plex-probe` records Part identifiers and paths for one metadata item. Keep the
token in an environment variable rather than a command-line argument:

```sh
PLEX_URL=http://plex:32400 \
PLEX_TOKEN='...' \
go run ./cmd/plex-probe \
  -rating-key 12345 \
  -output plex-probe-report.json
```

Probe reports are mode `0600` and omit the Plex origin and token. They retain
Plex-visible file paths and rating keys, so sanitize them before sharing.

## Verification

```sh
go test ./...
go test -race ./...
go vet ./...
docker build -t plex-gateway:mvp .
docker run --rm plex-gateway:mvp version
```

Copyright (C) 2026 InfinityPacer.

Licensed under the GNU General Public License v3.0 only
(`GPL-3.0-only`). See [LICENSE](LICENSE).
