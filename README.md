# Pier

A private, stateless Maven repository on top of S3-compatible object
storage. One Go binary, no database, no user accounts.

- **Every request** is authorized by an OIDC token, verified against the
  configured issuers — no deploy tokens, no shared secrets. GitHub
  Actions OIDC (trusted publishing) is one example of a source; so is
  any IdP you configure (Exoscale SSO, Keycloak, ...).
- Authorization is a list of **CEL rules** evaluated per request against
  the verified token claims, the Maven coordinates of the path, and the
  action (`download` / `upload` / `delete`) — the token source does not
  decide, the rules do.
- Optional **pull-through cache**: public or basic-authenticated
  upstreams (Maven Central, Clojars, ...) are cached on demand, with
  metadata TTL revalidation and metadata synthesis for private groups.
- **Stateless**: all state lives in S3; the binary hot-reloads its
  configuration on `SIGHUP`.

```
 GitHub Actions ──OIDC token──┐
                              ├─→ pier ────────────────→ S3-compatible
 IdP (SSO, ...) ──OIDC token──┘      │                   object storage
                                    ▼
                               CEL policy (per request)
```

## Documentation

The full documentation lives at
[https://brutasse.github.io/pier/](https://brutasse.github.io/pier/):
quickstart, publishing from GitHub Actions, downloading, pull-through
cache, `oidc-cli`, policy rules, configuration, operations, development.

## Quickstart

Install the binary (Linux x86_64/arm64; always the latest release):

```sh
curl -fsSL https://raw.githubusercontent.com/brutasse/pier/v0.1.0/install.sh | sh
```

Or pull the Docker image (multi-arch; always the latest release):

```sh
docker pull ghcr.io/brutasse/pier:v0.1.0
```

Fetch a config, edit it, and run it:

```sh
curl -fsSL https://raw.githubusercontent.com/brutasse/pier/main/configs/example.yaml -o pier.yaml
# edit pier.yaml, then:
pier -config pier.yaml
```

Observability endpoints are opt-in: set `metrics_port: 9090` for
Prometheus and/or `pprof_port: 9091` for profiling in the config
([operations](https://brutasse.github.io/pier/operations/)).

The config path may also come from the `PIER_CONFIG` environment
variable when `-config` is not given.

```sh
docker run -p 8080:8080 -v $(pwd)/pier.yaml:/config/pier.yaml ghcr.io/brutasse/pier:v0.1.0
```

S3 credentials are never in the config file: the standard AWS SDK
credential chain applies (environment variables, shared credentials file,
instance profile). See [`configs/example.yaml`](https://github.com/brutasse/pier/blob/main/configs/example.yaml) for a fully commented
example.

## Development

```sh
make test               # unit tests, no network
make build              # -> bin/pier
make test-integration   # e2e: needs PIER_TEST_S3_ENDPOINT (S3Mock/rclone serve s3)
```
