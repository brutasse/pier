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

## Design decisions

- **Plugin-maintained metadata is last-writer-wins.** The deploy
  plugin's read-modify-write spans separate `GET` and `PUT` requests,
  which Pier cannot serialize, so concurrent deploys of different
  versions of the same GAV resolve last-writer-wins on the
  plugin-maintained document. Pier does not merge concurrent plugin
  writes. It does serialize its own read-modify-write — artifact `PUT`
  + metadata invalidation, metadata synthesis, and plugin metadata
  `PUT`s — per GAV under an in-process lock, relying on S3's consistent
  read-after-write. Metadata written directly to S3 (bypassing Pier)
  is not tracked — only Pier-mediated `PUT`s trigger the synthesis
  invalidation.
- **In-process per-GAV serialization, no cross-instance coordination.**
  The per-GAV locks are process-local: simple, at the cost that with
  multiple replicas, concurrent operations on the same GAV are not
  serialized across instances — a bounded stale-document window,
  healed by the next artifact `PUT` for the GAV.
- **Single repository root.** One flat repository per instance; the
  namespace is the group id, with `reserved_groups` splitting internal
  uploads from the pull-through cache. No named repositories.
- **`DELETE` removes one object.** No delete by prefix: bulk removal
  (a version, a group, a stale cached prefix) is an operator action on
  S3 — lifecycle rules, `aws s3 rm`, rclone — not a repository
  operation.
