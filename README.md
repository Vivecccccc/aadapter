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

## Base URL composition
`GATEWAY_BASE_URL` should be only your gateway origin (and optional fixed prefix), for example:
- `https://gateway.example.com`
- `https://gateway.example.com/proxy`

Do **not** append the Vertex route suffix yourself. The adapter appends:
`/v1/projects/{project}/locations/{location}/publishers/{publisher}/models/{model}:{rawPredict|streamRawPredict}`

So if `GATEWAY_BASE_URL=https://gateway.example.com`, final forwarded URL is like:
`https://gateway.example.com/v1/projects/...:rawPredict`

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
