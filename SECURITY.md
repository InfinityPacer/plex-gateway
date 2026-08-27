# Security policy

## Sensitive data

Do not include any of the following in issues, pull requests, trace samples, or
support bundles:

- `X-Plex-Token` values or Plex client identifiers;
- MediaVault credentials, cloud-provider cookies, or API keys;
- complete signed CDN URLs or redirect query values;
- private hostnames, addresses, filesystem paths, or raw library activity logs.

Trace mode records paths and query parameter names but not query values,
cookies, request bodies, response bodies, client identifiers, or remote
addresses. Review traces before sharing because endpoint order can still reveal
library usage patterns.

`plex-probe` reports exclude the Plex origin and token, but intentionally retain
Part file paths and rating keys for stability analysis. Treat raw reports as
private and publish only sanitized field shapes or comparisons.

## Deployment trust boundary

Plex must authenticate requests arriving from the Gateway container network.
Do not add the Gateway address or Docker subnet to Plex **Allowed Networks**.
At startup, `plex-gateway` sends a request with a fresh invalid Plex Token and
starts only when Plex rejects it with `401` or `403`. This prevents transparent
proxy requests from inheriting Plex's network-level anonymous access. Restart
the Gateway after changing Plex network authentication settings so the boundary
is checked again.

The configured MediaVault origin receives the complete active client header
set during direct-link resolution, including Plex authentication headers and
cookies. Configure only an operator-controlled MediaVault service, keep that
control path on a protected network, and use HTTPS whenever it crosses a trust
boundary.

The health and metrics endpoints do not contain request labels or credentials,
but metrics expose aggregate activity counters. Restrict them at the reverse
proxy when those counters should not be public.

## Reporting a vulnerability

Do not open a public issue containing exploit details or credentials. Email
[InfinityPacer@gmail.com](mailto:InfinityPacer@gmail.com) with the subject
`plex-gateway security` and provide the smallest sanitized reproduction
possible.
