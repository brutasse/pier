# Configuration

See [`configs/example.yaml`](https://github.com/brutasse/pier/blob/main/configs/example.yaml) for the commented reference. Keys:

| Key | Default | Notes |
|-----|---------|-------|
| `listen` | — | required, e.g. `":8080"` |
| `metrics_port` | `0` | `/metrics` (Prometheus) port; `0` disables; restart-only |
| `pprof_port` | `0` | `/debug/pprof/` port; `0` disables; restart-only |
| `s3.bucket` | — | required |
| `s3.prefix` | `""` | key prefix inside the bucket |
| `s3.region` | — | required |
| `s3.endpoint` | — | S3-compatible endpoint; empty = AWS. Scheme defaults to `https` |
| `s3.force_path_style` | true with endpoint, else false | most custom endpoints need `true` |
| `immutable_releases` | `true` | reject re-publish of non-SNAPSHOT <abbr title="groupId:artifactId:version">GAVs</abbr> (409) |
| `max_upload_bytes` | 512 MiB | upload size cap (413) |
| `max_pull_bytes` | 512 MiB | size cap for objects pulled from upstream (502) |
| `upstream[].name` / `upstream[].url` | — | pull-through sources, tried in order; empty = pull-through off |
| `upstream[].username` / `upstream[].password` | — | basic auth for a private upstream; `password` is the name of the env var holding the password; set both or neither |
| `reserved_groups[]` | — | groups reserved for internal uploads (see [pull-through](pull-through.md)) |
| `metadata_ttl` | `24h` | freshness window for pulled `maven-metadata.xml`; `0s` disables revalidation |
| `auth.disabled` | `false` | **dev only** — disables all auth |
| `auth.issuers[].name` | — | internal label |
| `auth.issuers[].well_known` | — | OIDC well-known URL (or JWKS URL) |
| `auth.issuers[].expected_iss` | — | the `iss` claim to accept |
| `auth.issuers[].audiences` | — | allowed `aud` values |
| `policy.default_deny` | `true` | |
| `policy.rules[]` | — | see [Writing policy rules](policies.md) |

You terminate TLS at the edge (load balancer / reverse proxy); the
service listens on plain HTTP. Pier fetches JWKS and issuer metadata
over HTTPS from each configured issuer and caches them for an hour.
Pier refreshes the cache on an unknown `kid`, with rate limiting.

## Configuration reload (SIGHUP)

`kill -HUP <pid>` reloads `policy.rules`, `auth`, `s3`,
`immutable_releases`, `max_upload_bytes`, `max_pull_bytes`,
`upstream`, `reserved_groups` and `metadata_ttl` without a restart. Pier
first validates the new file fully (YAML, CEL compilation, and a storage
liveness check against the configured bucket), then activates it
atomically. Requests in flight keep the configuration they started with.
On failure Pier keeps the current configuration and logs the error.

`listen` is the one exception: it cannot change on reload, and Pier
rejects a file with a different listen address in full. `metrics_port`
and `pprof_port` also cannot change on reload. Pier builds the admin
servers once at startup, so a reload that changes them has no effect
until a restart.
