# Getting a token: oidc-cli

[`oidc-cli`](https://github.com/ctron/oidc-cli) is a standalone command
line tool for working with OIDC tokens. Use it to negotiate a token with
your IdP, on the user's behalf, for use against Pier.

It implements the authorization code flow with PKCE: a local `localhost`
server receives the browser callback, and the access token plus a
refresh token are cached on disk for reuse.

## Installation

```sh
cargo install oidc-cli
```

or download a
[released binary](https://github.com/ctron/oidc-cli/releases) (also
available via `brew`, `snap` and `winget`).

## Setup

Register a public client with your IdP (PKCE enabled; see
[IdP notes](#idp-notes)), then create the local client entry — this runs
the interactive login in your default browser:

```sh
oidc create public pier --issuer https://id.example.com --client-id pier
```

The client (name `pier`) and the negotiated tokens are stored in
`~/.config/oidc/config.yaml` (mode 0600); override the path with
`-c file` or `OIDC_CONFIG`.

## Usage

```sh
# Print the access token. A cached token is reused until expiry,
# then refreshed with the refresh token:
oidc token pier
export PIER_TOKEN=$(oidc token pier)   # then use any tool that sends it

# curl:
curl -s -H "Authorization: $(oidc token pier -b)" \
  -o app.jar https://maven.example.com/com/example/app/1.2.3/app-1.2.3.jar

# Inspect the token's claims:
oidc inspect $(oidc token pier)

# List clients and token validity:
oidc list -d
```

## Maven

The Maven plugins do not natively send OIDC tokens — attach the token via
the wagon `httpHeaders` settings from
[Publishing from GitHub Actions](publishing.md):

```sh
export PIER_TOKEN=$(oidc token pier)
mvn -s settings.xml deploy:deploy-file -Dfile=target/app.jar \
  -DgroupId=com.example -DartifactId=app -Dversion=1.2.3 \
  -Dpackaging=jar -DrepositoryId=pier -Durl=https://maven.example.com
mvn -Dmaven.resolver.transport=wagon dependency:get \
  -Dartifact=com.example:myapp:1.2.3
```

A long build cannot refresh a token mid-run: if the access token expires
while Maven is running, the remaining transfers fail with 401 — refresh
with `oidc token pier -f` and re-run the goal.

## IdP notes

- The token's `aud` claim is decided by the IdP — oidc-cli has no
  audience option. It must be one of Pier's
  `auth.issuers[].audiences`:
  - Dex: `aud` is the client's `id`, so register the client as `pier`.
  - Keycloak: a public client with PKCE enabled, plus an audience mapper
    adding `pier`.

  Tokens are cached as issued, without local verification: a wrong
  audience is noticed as Pier's 401 `audience mismatch`, not earlier.
- If the IdP requires a pre-registered redirect URI (Keycloak, Okta,
  Auth0, ...), pin the callback port (`--port 8080`) and register
  `http://localhost:8080` byte for byte. If the callback never arrives,
  the browser may resolve `localhost` to a different loopback address
  than the one the local server bound to — force one side with
  `--bind only4` / `--bind only6`.
- Headless machines: there is no device flow. Log in on a machine with a
  browser and copy the client entry (or the whole config file) over, or
  bootstrap from an existing refresh token:

  ```sh
  oidc create public pier --issuer https://id.example.com \
    --client-id pier --refresh-token <refresh-token>
  ```

- The cached access token is reused until expiry, then refreshed
  automatically. When the refresh token has expired (or is missing),
  re-login with `oidc create public pier --force …`.
