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
pier -config pier.yaml
```

```sh
docker run -p 8080:8080 -v $(pwd)/pier.yaml:/config/pier.yaml ghcr.io/brutasse/pier:v0.1.0
```

S3 credentials are never in the config file: the standard AWS SDK
credential chain applies (environment variables, shared credentials file,
instance profile). See [`configs/example.yaml`](https://github.com/brutasse/pier/blob/main/configs/example.yaml) for a fully commented
example.
