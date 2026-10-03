# Pull-through cache

With upstream repositories configured, Pier also caches public artifacts
(Maven Central, Clojars, ...). A `GET` for an object that is not in the
bucket fetches it from the first upstream that has it. Pier stores it in
the bucket (with computed checksum sidecars) and serves it. Pier serves
subsequent requests from the bucket, without contacting the upstream.
Pier reads proxied artifacts through the same OIDC authn and CEL authz as
private ones: Pier contacts the upstream only for an authorized request.
With no upstream configured, Pier is a plain private repository — a miss
is a `404`.

Pier splits the namespace by `reserved_groups`:

- Paths under a reserved group (dot-boundary prefix match: `com.acme`
  covers `com.acme` and `com.acme.internal`, not `com.acmeX`) are
  **internal-upload-only**: Pier never pulls them from upstream, and a
  miss is a `404`.
- All other paths belong to the pull-through cache and are **read-only**.
  Pier rejects `PUT`/`POST` there with `403` unconditionally — even for a
  publish-authorized token, and even before authentication — because such
  an object can at any moment come from the upstream.
- `DELETE` stays unrestricted: it is the operator's purge tool for cache
  entries (Pier re-pulls a purged object on the next request).

```yaml
upstream:
  - name: central
    url: https://repo.maven.apache.org/maven2/
  - name: clojars
    url: https://repo.clojars.org/
reserved_groups:
  - com.acme
```

Pier tries upstreams in order; the first `200` wins. A pull cannot exceed
`max_pull_bytes` (see [Configuration](configuration.md)).

### Authenticated upstreams

Public upstreams need no credentials. For a repository behind
HTTP basic auth, add `username` and `password` to that upstream
entry; omit both for a public one. The two keys are all-or-nothing —
setting only one is a configuration error.

```yaml
upstream:
  - name: artifactory
    url: https://artifactory.example.com/artifactory/libs-release
    username: ci-bot
    password: ARTIFACTORY_PASSWORD   # name of an environment variable
```

The secret never lives in the config file: `password` is the *name* of
an environment variable, resolved at load time (and again on each
SIGHUP reload). A missing or unset variable fails the load, so Pier
catches a misconfigured upstream before serving. Pier sends the resolved
value as an `Authorization: Basic` header on every fetch (and peek) to
that repository only.

Behavior notes:

- A cold `GET` streams the object to the client and to S3 in a single
  pass (S3 multipart upload, 8 MiB part buffers). Memory per cold pull
  stays within one part buffer; there is no disk I/O.
- A cold `Range` GET answers `206` up front and streams the requested
  range to the client as it arrives from the upstream. Pier caches the
  full object in the same pass. An unsatisfiable range answers `416`
  without pulling the body. If the client disconnects before the cache
  fill finishes, Pier aborts the fill (as with any interrupted download).
  Against an upstream that sends no `Content-Length`, Pier serves the
  range after the full download instead — `Content-Range` needs the
  total size.
- Pier coalesces concurrent cold requests for the same path
  (single-flight): one upstream fetch, all clients served.
- `HEAD` on a cold miss peeks the upstream (status, size, content type)
  without storing; the first `GET` does the actual pull.
- Pier computes checksum sidecars (`.md5`, `.sha1`, `.sha256`,
  `.sha512`) from the pulled bytes. When you request a checksum and its
  base object is in the bucket, Pier computes it from the cached base and
  never contacts the upstream for it.
- Pier replaces an upstream `Content-Type: application/octet-stream` with
  the type derived from the extension, so cached objects match internal
  uploads.
- If the fetch succeeds but storing to S3 fails, the client still
  receives the upstream bytes; the cache simply has no entry
  (best-effort).
- A non-404 upstream error across all upstreams yields `502`; Pier stores
  nothing.

Floating versions (`LATEST`, `RELEASE`, ranges) work on the pull-through
side out of the box: the resolver reads the upstream's
`maven-metadata.xml` through the cache. Two mechanisms keep that
metadata — and the private metadata — usable:

- **Metadata TTL** (`metadata_ttl`, default `24h`): Pier trusts a cached
  `maven-metadata.xml` only while it is fresh. Once the TTL has passed,
  the next `GET` revalidates it from the upstream and replaces the
  cached copy. If the revalidation fails (the upstream 404s or errors),
  Pier serves and logs the stale copy — a cached answer beats a `404`.
  The TTL applies to pulled metadata only (artifact-level and
  snapshot-level). Pier never re-pulls cached artifacts: if an upstream
  object changes, purge the cached copy with `DELETE` or an S3 lifecycle
  rule. The mechanism below keeps private metadata fresh. Set
  `metadata_ttl: 0s` to disable revalidation (cache-forever).
- **Metadata synthesis**: for a <abbr title="groupId:artifactId:version">GAV</abbr> that pull-through does not serve (a reserved
  group, or no upstream at all), Pier synthesizes a missing
  `maven-metadata.xml` from the versions present in the bucket. The
  document has the same shape the `deploy` plugin writes, with
  `<release>` set to the highest non-snapshot version and `<latest>` to
  the highest version overall. Pier stores it with checksum sidecars on
  first synthesis, and any artifact `PUT` for the <abbr title="groupId:artifactId:version">GAV</abbr> deletes the
  metadata so the next read re-synthesizes it. This is how
  `deploy:deploy-file` (which deploys no metadata) becomes resolvable by
  version range/`LATEST`. A later real `mvn deploy` overwrites the
  synthesized document. Pier never synthesizes snapshot-level metadata
  (`g/a/X-SNAPSHOT/maven-metadata.xml`); the `deploy` plugin manages it.
