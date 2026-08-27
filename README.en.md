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
STRM Part, the gateway opts that request into Plex Direct Play while preserving
the complete client profile and Plex's own decision response. A short-lived
grant is created only after that complete response explicitly marks the same
Part as Direct Play. If the client then requests universal `start`,
`start.mpd`, or `start.m3u8`, the gateway requires that grant from the same
playback session and exact Media/Part, authorizes the Part again, and returns
the same CDN redirect without starting Plex Transcoder or synthesizing Plex
metadata.

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
| Infuse Direct Play | Implemented; acceptance matrix incomplete | Uses the Plex Part redirect path. |
| Plex iOS ExperimentalPlayer | Limited runtime evidence | One successful session is not a general compatibility claim. |
| Plex for Apple TV | Not supported | Outside the current compatibility scope. |
| Plex Web cloud playback | Unsupported | The browser expects a DASH manifest and enforces final-origin CORS. |
| VidHub and SenPlayer | Not yet verified | Planned client-compatibility targets. |

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
| `MEDIAVAULT_URL` | disabled | Internal MediaVault HTTP(S) origin. |
| `PATH_MAPPINGS` | disabled | JSON array of Plex-to-container STRM prefixes. |
| `LISTEN_ADDR` | `:32400` | Gateway listener. |
| `PART_CACHE_TTL` | `24h` | In-memory Part cache lifetime. |
| `RESOLVER_TIMEOUT` | `15s` | End-to-end MediaVault resolution timeout. |
| `OBSERVE_MAX_BYTES` | `8388608` | Maximum metadata body copied for observation. |
| `PART_PROBE_TIMEOUT` | `15s` | Timeout for the bodyless Plex Part authorization probe. |
| `CLOUD_EXTENSIONS` | `.strm` | Comma-separated cloud control-file extensions. |
| `TRACE_ENABLED` | `false` | Enable sanitized Plex request-order tracing. |
| `METADATA_GUARD_ENABLED` | `false` | Limit single-item detailed metadata requests before they enter Plex. |
| `METADATA_GUARD_GLOBAL_CONCURRENCY` | `8` | Shared detailed metadata concurrency limit across all clients. |
| `METADATA_GUARD_CLIENT_CONCURRENCY` | `4` | Detailed metadata concurrency limit for each Plex client identifier. |
| `METADATA_GUARD_QUEUE_TIMEOUT` | `10s` | Maximum admission wait before returning `429`. |

Plex tokens are not stored in configuration. Ordinary Plex requests remain
transparent. For cloud playback, all client request headers are forwarded to
the configured MediaVault origin so it can generate a direct URL for the same
client context. The internal lookup always uses `GET`, allowing a client `HEAD`
request to receive the same 302 even when MediaVault does not support `HEAD`.
Deploy MediaVault as a trusted upstream and keep resolver requests and headers
out of logs.

Clients connect to the gateway as they would connect to Plex and authenticate
through Plex. The gateway does not need a Plex username, password, or static
token. MediaVault's API key for `/api/v1` integrations is also not required for
the STRM `/redirect` playback contract.

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

- `GET /health` returns process health.
- `GET /metrics` returns fixed-shape JSON counters plus resolver and complete
  redirect-path latency totals, samples, last values, and maxima. When metadata
  protection is enabled, it also reports admission, timeout, active, and queued
  counts. No metric contains request labels or credentials.
- Every other endpoint follows the Plex proxy or Direct Play interception
  rules described in [docs/architecture.md](docs/architecture.md).

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
