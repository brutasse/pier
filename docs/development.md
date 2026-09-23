# Development

```sh
make test               # unit tests, no network
make build              # -> bin/pier
make test-integration   # e2e: needs PIER_TEST_S3_ENDPOINT (S3Mock/rclone serve s3)
```

Integration test:

```sh
# start an S3-compatible store (rclone serve s3 works too):
#   docker run --rm -p 9090:9090 adobe/s3mock
PIER_TEST_S3_ENDPOINT=http://127.0.0.1:9090 \
PIER_TEST_S3_USER=testuser PIER_TEST_S3_PASS=testpass \
PIER_TEST_S3_BUCKET=pier-integration \
  go test -tags integration ./test/integration
```

## Out of scope (v1)

- `maven-metadata.xml`: concurrent deploys of different versions are
  last-writer-wins on the plugin-maintained document: the plugin's
  read-modify-write spans separate `GET` and `PUT` requests, which
  Pier cannot serialize. Pier's own read-modify-write — artifact
  `PUT` + metadata invalidation, metadata synthesis, and plugin
  metadata `PUT`s — is serialized per GAV under an in-process lock
  and relies on S3's consistent read-after-write. Metadata written
  directly to S3 (bypassing Pier) is not tracked — only Pier-mediated
  `PUT`s trigger the synthesis invalidation.
- Single Pier instance: the per-GAV serialization is in-process; with
  multiple replicas a bounded stale-document window reappears, healed
  by the next artifact `PUT` for the GAV.
- No named repositories (single root) and no delete by prefix.
