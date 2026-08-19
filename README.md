# aadapter

A Go adapter that accepts Anthropic-compatible `/v1/messages` requests from Claude Code and forwards them through a configurable enterprise gateway to Vertex AI Claude or Gemini models.

The configured gateway must accept the `id_token` returned by `AUTH_URL`. This authentication flow is intentionally gateway-specific; it is not a replacement for Google Application Default Credentials when calling `aiplatform.googleapis.com` directly.

## Features
- Single static binary (Linux/Windows friendly)
- Container-friendly HTTP service
- Automatic bearer token rotation via auth endpoint (`id_token` -> `Authorization: Bearer ...`)
- Optional retry on 401/403 with forced token refresh
- Strict Anthropic SSE generation for Gemini streaming responses
- Gemini 3.5/3.6 thought-signature preservation, parallel tool calls, and local stop-sequence handling
- Bounded request, response, stream-event, debug-capture, and signature-cache memory use

## Required environment variables
- `GATEWAY_BASE_URL`
- `VERTEX_PROJECT`
- `VERTEX_LOCATION`
- `VERTEX_MODEL`
- `AUTH_URL`
- `AUTH_USER_ID`
- `AUTH_PASSWORD`

## Optional environment variables
- `ADAPTER_LISTEN_ADDR` (default `127.0.0.1:8080`)
- `ADAPTER_API_KEY` (recommended; Claude Code may send it as `x-api-key`)
- `ALLOW_UNAUTHENTICATED` (default `false`; must be explicitly enabled when binding a non-loopback address without `ADAPTER_API_KEY`)
- `VERTEX_PUBLISHER` (default `anthropic`)
- `AUTH_OTP` (default empty)
- `AUTH_OTP_TYPE` (`TOTP` or `PUSH`, default `TOTP`)
- `AUTH_REFRESH_SKEW` (default `90s`)
- `AUTH_TIMEOUT` (default `10s`)
- `GATEWAY_TIMEOUT` (default `120s`)
- `REQUEST_READ_TIMEOUT` (default `30s`; does not limit response stream duration)
- `FORCE_REFRESH_ON_401_403` (default `true`)
- `INSECURE_SKIP_TLS_VERIFY` (default `false`; emergency/private-gateway opt-in only)
- `MAX_REQUEST_BODY_BYTES` (default `33554432`)
- `MAX_RESPONSE_BODY_BYTES` (default `67108864`)
- `MAX_STREAM_EVENT_BYTES` (default `10485760`)
- `MAX_DEBUG_CAPTURE_BYTES` (default `1048576`)
- `SIGNATURE_TTL` (default `6h`)
- `SIGNATURE_MAX_SESSIONS` (default `1024`)
- `SIGNATURE_MAX_ENTRIES_PER_SESSION` (default `2048`)
- `VERTEX_ANTHROPIC_VERSION` (default `vertex-2023-10-16`)
- `VERTEX_API_FORMAT` (`anthropic` or `gemini`, default `anthropic`)
- `MODEL_OVERRIDE` (default `true`)

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
- `debug`: logs request/response JSON and stream payloads up to `MAX_DEBUG_CAPTURE_BYTES`, plus headers. Authorization, API keys, and cookies are redacted.
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

## Vertex API formats

`VERTEX_API_FORMAT=anthropic` keeps the original Anthropic partner-model behavior:

- Vertex publisher defaults to `anthropic`
- `/v1/messages` forwards to `:rawPredict`
- streaming forwards to `:streamRawPredict`
- body field `model` is removed
- body field `anthropic_version` is set from `VERTEX_ANTHROPIC_VERSION`

`VERTEX_API_FORMAT=gemini` enables Google Gemini native conversion for Claude Code:

- Vertex publisher defaults to `google`
- `VERTEX_MODEL` is the configurable model segment after `/models/`; supported production targets are `gemini-3.5-flash` and `gemini-3.6-flash`
- `/v1/messages` forwards to `:generateContent`
- streaming forwards to `:streamGenerateContent?alt=sse`
- `/v1/messages/count_tokens` forwards to `:countTokens`
- Anthropic Messages requests are converted to Gemini `contents`, `systemInstruction`, `tools`, `toolConfig`, and `generationConfig`
- Gemini responses and Gemini SSE are converted back to Anthropic Messages/SSE for Claude Code
- Claude-only fields such as `context_management` are recognized and not forwarded to Gemini
- Claude Code's legacy `BatchTool` meta-tool is omitted because Gemini supports parallel function calls natively
- `tool_choice.disable_parallel_tool_use=true` and Anthropic `container` execution are rejected because Vertex Gemini cannot preserve those semantics
- `output_config.effort` maps to Gemini `generationConfig.thinkingConfig.thinkingLevel` (`low`, `medium`, `high`, `max`, and Claude Code `xhigh` are handled explicitly)
- Gemini thought signatures are carried in valid `redacted_thinking` blocks and also kept in a bounded cache isolated by Claude Code's `X-Claude-Code-Session-Id`
- Text or media that Claude Code appends after tool results (for example Skill bodies, hook context, or nested project rules) is folded into the corresponding Gemini `functionResponse`
- Multimodal tool results use Gemini `functionResponse.parts` and reject MIME types outside the supported image/PDF/plain-text set before forwarding
- `stop_sequences` are enforced locally so the Anthropic response contains the exact matching `stop_sequence`
- Gemini 3.5 preserves supported `temperature`/`top_p` controls but omits its fixed `top_k`; Gemini 3.6 omits custom sampling controls that it ignores

Example Gemini setup:

```bash
export VERTEX_API_FORMAT=gemini
export VERTEX_MODEL=gemini-3.5-flash
export VERTEX_LOCATION=global
# Optional. This is the default when VERTEX_API_FORMAT=gemini.
export VERTEX_PUBLISHER=google
```

The adapter does not enforce model-specific Vertex region restrictions; `VERTEX_LOCATION` is forwarded as configured so gateway-specific routing can be used. `gemini-3.7-flash` is not a published Vertex model and is rejected at startup.

## Claude Code 2.1.88

Set Claude Code's Anthropic base URL to this service and, when `ADAPTER_API_KEY` is configured, use the same value as Claude Code's API key. The adapter supports the request shapes used by Claude Code 2.1.88, including `context_management`, `output_config.effort`, token counting, base64/URL media, tool errors, parallel tool calls, and streaming tool responses.

Claude Code 2.1.88 itself was withdrawn upstream; this compatibility target refers to its Anthropic HTTP protocol behavior, not to the unrelated CLI packaging regressions in that release.

## Claude Messages vs Vertex Claude rawPredict compatibility
For Anthropic-native Messages API (`POST /v1/messages`), requests commonly include:
- body field `model`
- required headers `x-api-key` and `anthropic-version`

For Vertex Claude (`rawPredict`/`streamRawPredict`), Google samples show:
- model selected in URL path `/publishers/anthropic/models/{MODEL}:rawPredict` (or `:streamRawPredict`)
- body field `anthropic_version` (e.g. `vertex-2023-10-16`)
- auth via `Authorization: Bearer ...`

Implemented rewrite behavior in this adapter:
- Adapter always removes request body `model` from the forwarded body.
- When `MODEL_OVERRIDE=true` (default), adapter always uses `VERTEX_MODEL` for the Vertex URL model segment.
- When `MODEL_OVERRIDE=false`, adapter uses request body `model` when provided; otherwise falls back to `VERTEX_MODEL`.
- Adapter always sets forwarded body `anthropic_version` from `VERTEX_ANTHROPIC_VERSION`.
- Request body/header `anthropic_version` values are ignored.
- Auth remains gateway-style bearer token managed by the token provider.

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
- darwin/amd64
- darwin/arm64
- windows/amd64

## Publish binaries to GitHub Releases
A GitHub Actions workflow is included at `.github/workflows/release.yml`.

It automatically builds and uploads binaries to a GitHub Release when you push a tag:

```bash
git tag v0.1.0
git push origin v0.1.0
```

The workflow uploads binaries and checksum files to the corresponding Release.
