# Pier

A private Maven repository on top of S3-compatible object
storage, fitting in one stateless Go binary.

- **Every request** is authorized by an OIDC token, verified against the
  configured issuers — without any tokens or shared secrets. GitHub
  Actions OIDC (trusted publishing) is one example of a source; so is
  any IdP you configure (Authentik, Keycloak, ...).
- Authorization is a list of **CEL rules** evaluated per request against
  the verified token claims, the Maven coordinates of the path, and the
  action (`download` / `upload` / `delete`) — the token source does not
  decide, the rules do.

```
 GitHub Actions ──OIDC token──┐
                              ├─→ Pier ────────────────→ S3-compatible
 IdP (SSO, ...) ──OIDC token──┘      │                   object storage
                                     ▼
                               CEL policy (per request)
```

## Maven protocol

The service speaks the plain-HTTP Maven repository layout:

| Operation | HTTP | Path |
|-----------|------|------|
| resolve artifact | `GET`/`HEAD` | `com/example/foo/1.0.0/foo-1.0.0.jar` |
| resolve metadata | `GET`/`HEAD` | `com/example/foo/maven-metadata.xml` |
| deploy artifact/pom | `PUT` | same as above |
| deploy metadata | `POST` (or `PUT`) | `com/example/foo/maven-metadata.xml` |
| deploy checksum | `PUT`/`POST` | `...jar.sha1` (also `.md5`, `.sha256`, `.sha512`) |
| delete | `DELETE` | any of the above |

Behavior notes:

- Downloads support HTTP byte ranges (`Range: bytes=...`): satisfiable
  ranges return `206 Partial Content` with `Content-Range`, so large
  artifacts can be fetched partially or resumed; unsatisfiable ranges
  return `416`.
- `maven-metadata.xml` is stored exactly as the deploy client sends it
  (the Maven deploy plugin generates the full XML, including snapshot
  versions). If it was never deployed, `GET` returns `404`.
- Checksum uploads are **verified against the stored base object**: a
  mismatch is rejected with `409` so a broken deploy cannot poison the
  repo. The base object must exist first.
- Non-SNAPSHOT objects are **immutable** by default (`immutable_releases:
  true`): re-uploading a published GAV (`groupId:artifactId:version`)
  returns `409`. Snapshots and `maven-metadata.xml` can be re-uploaded.
- `401` = missing/invalid token (the body states the reason, e.g.
  `audience mismatch`, `token expired`, `JWKS unavailable`); `403` =
  valid token, no rule allows the action.
