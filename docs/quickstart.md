# Quickstart

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
pier serve -config pier.yaml
```

Observability endpoints are opt-in: set `metrics_port: 9090` for
Prometheus and/or `pprof_port: 9091` for profiling in the config
([operations](operations.md)).

The config path may also come from the `PIER_CONFIG` environment
variable when `-config` is not given.

```sh
docker run -p 8080:8080 -v $(pwd)/pier.yaml:/config/pier.yaml ghcr.io/brutasse/pier:v0.1.0
```

S3 credentials are never in the config file: the standard AWS SDK
credential chain applies (environment variables, shared credentials file,
instance profile). See [`configs/example.yaml`](https://github.com/brutasse/pier/blob/main/configs/example.yaml) for a fully commented
example.
