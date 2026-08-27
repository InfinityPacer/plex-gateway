# Plex STRM Phase 0 behavior report

Status: Phase 0 complete. Metadata scan, library-refresh stability, and Infuse
Direct Play request observations were completed on August 23, 2026.

## Safety boundary

Run initial discovery read-only. On a production Plex server, isolate the test
directory, library, client, and observation window before any scan, refresh,
restart, or playback experiment. Keep environment-specific notes in
`docs/plex-behavior.local.md`, which is excluded from Git.

## Questions to answer

1. Does Plex create a `Media/Part` for a `.strm` item?
2. Is `Part.file` the original Plex-visible `.strm` path?
3. Does `Part.id` remain stable across refresh and restart?
4. Which portion of `Part.key` changes across refresh and restart?
5. Which endpoint does Infuse request for Direct Play?
6. Do seek, resume, next episode, timeline, and scrobble remain Plex control-flow requests while media Range requests go to the final CDN?

## Metadata observation procedure

1. Select one isolated test item and record its `ratingKey`.
2. Run `plex-probe` and retain the first JSON report.
3. Refresh only the test library, run the probe again with `-baseline`, and record the comparison.
4. Restart Plex only in an agreed maintenance window, rerun the probe, and record the comparison.
5. Preserve `Part.id`, `Part.key`, and `Part.file`; do not retain tokens or full signed URLs.

## Client trace procedure

1. Start the gateway with `TRACE_ENABLED=true` and point only the test client at it.
2. Capture a bounded trace for: detail page, play, seek, pause, resume, next episode, and stop.
3. Correlate records by the monotonic `sequence` field.
4. Record endpoint order and status codes in this document. Do not paste tokens, query values, cookies, client identifiers, private addresses, or signed URLs.

## Observations

### Plex metadata

An isolated movie library containing only `Example Movie.strm` was scanned by a
production Plex instance. The STRM body used a deliberately non-working local
MediaVault redirect URL, so this observation proves Plex metadata behavior only;
it does not prove playback or redirect compatibility.

`GET /library/metadata/{ratingKey}` returned one Part. The values below preserve
the observed shape while removing environment-specific identifiers and paths:

```text
Part.id   = {numericPartId}
Part.key  = /library/parts/{numericPartId}/{numericChangeStamp}/file
Part.file = /media/cloud-test/Example Movie.strm
```

Confirmed behavior:

- Plex creates a normal `Media/Part` for the `.strm` item.
- `Part.file` remains the Plex-visible `.strm` filesystem path.
- `Part.key` uses `/library/parts/{partId}/{changeStamp}/file`; the final path
  segment does not retain the `.strm` filename or extension on this server.
- The numeric Part ID can be parsed independently of the change stamp and final
  path segment.

### Part stability

The isolated library was refreshed with `force=1`, then the same rating key was
read again with `plex-probe -baseline`.

```text
Part.id:   unchanged
Part.key:  unchanged
Part.file: unchanged
```

This establishes stability across one library refresh only. A Plex restart was
not performed because the observation target is a production server and no
maintenance window was authorized.

### Infuse request order

Infuse 8.4.4 on macOS was routed through the transparent Phase 0 gateway for one
bounded session. The route applied only to the test client and was removed
immediately afterward. Health and `/identity` requests passed transparently to
Plex and returned HTTP 200.

The relevant request sequence was:

```text
GET /library/metadata/{ratingKey}/thumb/{changeStamp}
GET /library/metadata/{ratingKey}/extras
GET /library/metadata/{ratingKey}
GET /library/metadata/{ratingKey}?includeMarkers=...
GET /library/parts/{partId}/{changeStamp}/file
```

Observed client headers identified the caller as:

```text
X-Plex-Product:  Infuse-Library
X-Plex-Platform: macOS
X-Plex-Device:   Mac
User-Agent:      Infuse-Library/8.4.4
```

The final playback request was exactly the `Part.key` returned by metadata:

```text
GET /library/parts/{numericPartId}/{numericChangeStamp}/file
```

Plex returned HTTP 301 after approximately 20 seconds because the deliberately
non-working STRM target could not complete playback. Infuse then displayed a
server error. This proves the Direct Play endpoint selection but does not prove
successful seek, pause/resume, timeline updates, or next-episode behavior; those
require a valid MediaVault redirect and belong to the Phase 1 acceptance run.

The raw JSON trace remains private because even redacted endpoint order can
reveal personal library usage patterns. The final report will summarize only
method, endpoint shape, response status, and ordering.

## Phase 1 gate

The Phase 0 evidence establishes `/library/parts/{partId}/...` as the Infuse
Direct Play interception point and supports Part ID as the future cache identity.
Phase 1 may now design Part interception, response-derived cache population,
cache-miss fallback, path mapping, and resolver behavior while preserving the
default transparent Plex path.

## Cleanup

The temporary client-specific network rule, trace container, packet-capture job,
test Plex library, and test STRM file were removed after observation. A final
readback confirmed that the pre-existing production library set matched its
baseline.
