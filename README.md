# aadapter

A high-performance Go adapter that accepts Anthropic-compatible `/v1/messages` requests and forwards them to a configurable Vertex AI Gateway endpoint using `rawPredict` / `streamRawPredict`.

## Features
- Single static binary (Linux/Windows friendly)
- Container-friendly HTTP service
- Automatic bearer token rotation via auth endpoint (`id_token` -> `Authorization: Bearer ...`)
- Optional retry on 401/403 with forced token refresh
- Streaming passthrough for SSE responses

## Required environment variables
- `GATEWAY_BASE_URL`
- `VERTEX_PROJECT`
- `VERTEX_LOCATION`
- `VERTEX_MODEL`
- `AUTH_URL`
- `AUTH_USER_ID`
- `AUTH_PASSWORD`

## Optional environment variables
- `ADAPTER_LISTEN_ADDR` (default `:8080`)
- `VERTEX_PUBLISHER` (default `anthropic`)
- `AUTH_OTP` (default empty)
- `AUTH_OTP_TYPE` (`TOTP` or `PUSH`, default `TOTP`)
- `AUTH_REFRESH_SKEW` (default `90s`)
- `AUTH_TIMEOUT` (default `10s`)
- `GATEWAY_TIMEOUT` (default `120s`)
- `FORCE_REFRESH_ON_401_403` (default `true`)

## Logging controls (`--verbose`, `--log-level`)
You can control runtime logs with CLI flags:

```bash
go run . --log-level=info
go run . --log-level=warning
go run . --log-level=error
go run . --log-level=debug
go run . --verbose
```

Rules:
- `--verbose` forces debug logging (same effect as `--log-level=debug`).
- `--log-level` controls minimum severity: `debug`, `info`, `warning`, `error`.

Message logging behavior:
- `debug`: logs full inbound `/v1/messages` request JSON and full upstream response body JSON (or full stream payload) without truncation, plus headers (Authorization redacted).
- `info`: one-line per request summary (method/path/model/stream) and completion summary (status/bytes/duration/target).
- `warning`: warning/error only (e.g. invalid request, 4xx, token refresh retry).
- `error`: only failures (e.g. token retrieval failure, upstream call failure, 5xx paths).

Env alternatives are also supported:
- `ADAPTER_VERBOSE=true`
- `ADAPTER_LOG_LEVEL=debug|info|warning|error`

## Base URL composition
`GATEWAY_BASE_URL` should be only your gateway origin (and optional fixed prefix), for example:
- `https://gateway.example.com`
- `https://gateway.example.com/proxy`

Do **not** append the Vertex route suffix yourself. The adapter appends:
`/v1/projects/{project}/locations/{location}/publishers/{publisher}/models/{model}:{rawPredict|streamRawPredict}`

So if `GATEWAY_BASE_URL=https://gateway.example.com`, final forwarded URL is like:
`https://gateway.example.com/v1/projects/...:rawPredict`

## Claude Messages vs Vertex Claude rawPredict compatibility
For Anthropic-native Messages API (`POST /v1/messages`), requests commonly include:
- body field `model`
- required headers `x-api-key` and `anthropic-version`

For Vertex Claude (`rawPredict`/`streamRawPredict`), Google samples show:
- model selected in URL path `/publishers/anthropic/models/{MODEL}:rawPredict` (or `:streamRawPredict`)
- body field `anthropic_version` (e.g. `vertex-2023-10-16`)
- auth via `Authorization: Bearer ...`

So the adapter should translate at least:
- Anthropic body `model` -> Vertex URL model segment
- Anthropic header `anthropic-version` -> Vertex body `anthropic_version`
- Anthropic auth header style -> Gateway/Vertex bearer auth style

## Local development
```bash
go run .
```

## Build binaries
```bash
make build-all VERSION=v0.1.0
```

This creates binaries in `dist/` for:
- linux/amd64
- linux/arm64
- windows/amd64

## Publish binaries to GitHub Releases
A GitHub Actions workflow is included at `.github/workflows/release.yml`.

It automatically builds and uploads binaries to a GitHub Release when you push a tag:

```bash
git tag v0.1.0
git push origin v0.1.0
```

The workflow uploads binaries and checksum files to the corresponding Release.
