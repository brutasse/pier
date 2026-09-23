# Pull-through cache

With upstream repositories configured, Pier also caches public
artifacts (Maven Central, Clojars, ...): a `GET` for an object that is not
in the bucket fetches it from the first upstream that has it, stores it
in the bucket (with computed checksum sidecars) and serves it. Subsequent
requests are served from the bucket without contacting the upstream.
Proxied artifacts are read through the same OIDC authn and CEL authz as
private ones: the upstream is contacted only for an authorized request.
With no upstream configured, Pier is a plain private repository — a
miss is a `404`.

The namespace is split by `reserved_groups`:

- Paths under a reserved group (dot-boundary prefix match: `com.acme`
  covers `com.acme` and `com.acme.internal`, not `com.acmeX`) are
  **internal-upload-only**: they are never pulled from upstream, and a
  miss is a `404`.
- All other paths belong to the pull-through cache and are **read-only**:
  `PUT`/`POST` there is rejected with `403` unconditionally — even for a
  publish-authorized token, and even before authentication — because
  such an object may at any moment come from the upstream.
- `DELETE` is unrestricted: it is the operator's purge tool for cache
  entries (a purged object is re-pulled on the next request).

```yaml
upstream:
  - name: central
    url: https://repo.maven.apache.org/maven2/
  - name: clojars
    url: https://repo.clojars.org/
reserved_groups:
  - com.acme
```

Upstreams are tried in order; the first `200` wins. A pull is capped at
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
SIGHUP reload). A missing or unset variable fails the load, so a
misconfigured upstream is caught before serving. The resolved value is
sent as an `Authorization: Basic` header on every fetch (and peek) to
that repository only.

Behavior notes:

- A cold `GET` streams the object to the client and to S3 in a single
  pass (S3 multipart upload, 8 MiB part buffers). Memory per cold pull is
  bounded by one part buffer; there is no disk I/O.
- A cold `Range` GET answers `206` up front and streams the requested
  range to the client as it arrives from the upstream, while the full
  object is cached in the same pass. An unsatisfiable range answers
  `416` without pulling the body. If the client disconnects before the
  cache fill finishes, the fill is aborted (as with any interrupted
  download). Against an upstream that sends no `Content-Length`, the
  range is served after the full download instead — the total size is
  needed for `Content-Range`.
- Concurrent cold requests for the same path are coalesced
  (single-flight): one upstream fetch, all clients served.
- `HEAD` on a cold miss peeks the upstream (status, size, content type)
  without storing; the first `GET` does the actual pull.
- Checksum sidecars (`.md5`, `.sha1`, `.sha256`, `.sha512`) are computed
  from the pulled bytes. When a checksum is requested and its base object
  is already cached, the checksum is computed from the cached base — the
  upstream is never contacted for it.
- An upstream `Content-Type: application/octet-stream` is replaced with
  the type derived from the extension, so cached objects match internal
  uploads.
- If the fetch succeeds but storing to S3 fails, the client still
  receives the upstream bytes; the cache simply has no entry
  (best-effort).
- A non-404 upstream error across all upstreams yields `502`; nothing is
  stored.

Floating versions (`LATEST`, `RELEASE`, ranges) work on the pull-through
side out of the box: the resolver reads the upstream's
`maven-metadata.xml` through the cache. Two mechanisms keep that
metadata — and the private metadata — usable:

- **Metadata TTL** (`metadata_ttl`, default `24h`): a cached
  `maven-metadata.xml` is only trusted while fresh. Once the TTL has
  passed, the next `GET` revalidates it from the upstream and replaces
  the cached copy. If the revalidation fails (the upstream 404s or
  errors), the stale copy is served and logged — a cached answer is
  preferred over a `404`. The TTL applies to pulled metadata only
  (artifact-level and snapshot-level): cached artifacts are never
  re-pulled — if an upstream object changes, purge the cached copy
  with `DELETE` or an S3 lifecycle rule — and private metadata is kept
  fresh by the mechanism below. Set `metadata_ttl: 0s` to disable
  revalidation (cache-forever).
- **Metadata synthesis**: for a <abbr title="groupId:artifactId:version">GAV</abbr> that is not served by pull-through
  (a reserved group, or no upstream at all), a missing
  `maven-metadata.xml` is synthesized from the versions present in the
  bucket — the same document shape the `deploy` plugin writes, with
  `<release>` set to the highest non-snapshot version and `<latest>` to
  the highest version overall. It is stored with checksum sidecars on
  first synthesis, and any artifact `PUT` for the <abbr title="groupId:artifactId:version">GAV</abbr> deletes the
  metadata so the next read re-synthesizes it. This is what makes
  `deploy:deploy-file` (which deploys no metadata) resolvable by version
  range/`LATEST`. A later real `mvn deploy` overwrites the synthesized
  document; snapshot-level metadata
  (`g/a/X-SNAPSHOT/maven-metadata.xml`) is never synthesized — it stays
  managed by the `deploy` plugin.
