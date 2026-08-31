# Configuration

The service reads runtime configuration only from environment variables at
startup. It does not support configuration files or `_FILE` environment
variants. Blank required values and invalid optional values stop startup.

URLs must use `http` or `https`, include a hostname, and contain no userinfo or
fragment. Operators control all configured destinations; the service does not
apply an IP-address denylist.

## Required Variables

| Variable | Purpose |
| --- | --- |
| `PAPERLESS_URL` | Base URL for the Paperless-ngx API. |
| `PAPERLESS_API_TOKEN` | Paperless API token. |
| `AI_BASE_URL` | Base URL for the OpenAI-compatible Responses API. |
| `AI_API_KEY` | Bearer credential for the AI gateway. |
| `AI_MODEL` | Model identifier sent to the AI gateway. |
| `WEBHOOK_TOKEN` | Dedicated bearer token for inbound Paperless webhooks. |
| `PAPERLESS_AI_WEBHOOK_URL` | Full Paperless AI manual webhook URL. |
| `PAPERLESS_AI_WEBHOOK_KEY` | Value sent in the Paperless AI `x-api-key` header. |

Use distinct, least-privilege credentials. Supply secrets through the deployment
platform without printing resolved environment values.

## Optional Variables

| Variable | Default | Validation and effect |
| --- | --- | --- |
| `HTTP_PORT` | `8080` | Integer from 1 through 65535. |
| `POLL_INTERVAL` | `15m` | Positive Go duration between reconciliation passes. |
| `RENDER_DPI` | `200` | Positive integer used by Poppler. |
| `BATCH_SIZE` | `5` | Integer from 1 through 5; applies to image batches. |
| `MODEL_ATTEMPTS` | `3` | Integer from 1 through 10 provider attempts per batch. |
| `RENDER_TIMEOUT` | `5m` | Positive Go duration for a Poppler render operation. |
| `MODEL_TIMEOUT` | `3m` | Positive Go duration for each AI HTTP request. |
| `DOCUMENT_DEADLINE` | `6h` | Positive Go duration for one claimed document attempt. |
| `TEMPORARY_RENDER_BUDGET` | `1GiB` | Positive integer plus `B`, `KiB`, `MiB`, or `GiB`. |

Go duration examples include `30s`, `5m`, and `2h30m`. Byte units are
case-sensitive.

Concurrency is fixed at one active document and one active model request. It is
not configurable.

## Model Transport

Startup probes the configured model with synthetic data. It first tests direct
PDF input, then image input if the provider explicitly reports PDF attachments
as unsupported. Startup fails if neither transport works. No user document is
used by the probe, and there is no text-only fallback.

When direct PDF capability is available, a document is sent directly only when
it has at most five pages and is at most 8 MiB. Larger or longer PDFs are
rendered to PNG pages. An explicit unsupported-attachment response during a
direct request also switches that document to image transport.

Each rendered page must be at most 8 MiB. A document may contain at most 10,000
pages.

The complete provider JSON request is limited to 32 MiB after attachments are
base64-encoded and the model name, prompts, native OCR draft, and response schema
are included. Five individually valid 8 MiB images cannot fit that aggregate
limit. Therefore, `BATCH_SIZE=5` and the per-image limit do not guarantee that a
batch is sendable. A locally detected aggregate oversize request is a terminal
OCR provider failure for that job; it is not sent to the gateway.

## Change Effects

Configuration is loaded once, so changes require a restart. Changing
`AI_MODEL`, render settings, or the compiled prompt does not create replacement
work for a completed job with the same document checksum. A new Paperless
checksum can create new work during webhook resolution or reconciliation.

Before changing a model or prompt, follow the rollout guidance in
[Operations](operations.md#model-and-prompt-changes).
