# Operations

## Observability

- `/healthz` — liveness, on the main listener.
- `/metrics` — Prometheus, on the port given by `metrics_port` in the
  config (off unless set):
  `pier_http_requests_total{method,status,action}`,
  `pier_http_request_duration_seconds`, `pier_auth_failures_total{reason}`,
  `pier_policy_denials_total{action}`, `pier_upload_bytes_total`,
  `pier_download_bytes_total`, `pier_upstream_fetches_total{repo,result}`
  (result: ok, not_found, error) and `pier_upstream_bytes_total{repo}`
  (successful pulls only).
- `/debug/pprof/` — profiling, on the port given by `pprof_port` in the
  config (off unless set). Both keys naming the same port share a
  single server.
- Structured JSON logs, one line per request (method, path, status,
  duration, matched rule, token `sub`). Tokens and full claims are never
  logged.

## Security notes

- The only long-lived secret in the system is the S3 credential (SDK
  credential chain). Tokens are short-lived (~10 min for GitHub), signed,
  audience-bound, and never stored.
- Fail-closed: any verification or policy error denies the request.
- Checksum verification on upload and immutable releases protect the
  repository from broken or malicious re-deploys.
- Replay is not mitigated with nonces (stateless); the exposure window is
  the token lifetime, and the only effect a token can have is what the
  policy already allows for that token.
