# paperless-ai-ocr

Multimodal AI transcription for [Paperless-ngx](https://github.com/paperless-ngx/paperless-ngx).

`paperless-ai-ocr` improves the searchable text stored in Paperless by sending
the original PDF or rendered page images to a vision-capable AI model. The
native Paperless OCR text is included as a hint, not treated as authoritative.
A complete, validated transcription replaces `document.content` only after the
entire document has been processed successfully.

> [!IMPORTANT]
> This project is under active development. The repository currently contains
> the approved design and implementation plan; no production release or GHCR
> image is available yet.

## Why This Exists

Traditional OCR works well for clean, simple documents, but often loses
information from:

- tables, columns, forms, and unusual page layouts;
- low-quality scans and photographed documents;
- mixed printed and handwritten annotations;
- multilingual documents;
- faint text, stamps, checkboxes, and marginal notes.

Paperless-ngx still performs its normal ingestion and native OCR. This service
adds a second, multimodal transcription stage before the existing
[`paperless-ai`](https://github.com/clusterzx/paperless-ai) metadata workflow.
If AI OCR fails, the native OCR remains untouched.

## Use Cases

### Make difficult documents searchable

Improve full-text search for scans whose native OCR is incomplete or poorly
ordered. Examples include old correspondence, photographed receipts, faint
invoices, stamped forms, and documents with annotations.

### Preserve tables and structured layouts

Transcribe invoices, bank statements, reports, and forms while retaining the
reading order that plain OCR frequently loses across columns and table cells.

### Process multilingual archives

Use a multimodal model to transcribe documents containing multiple languages or
scripts without asking it to translate, summarize, normalize, or infer missing
content.

### Improve downstream metadata extraction

Run `paperless-ai` only after corrected text has been committed to Paperless.
This prevents title, correspondent, document type, and tag suggestions from
being generated from incomplete native OCR.

### Backfill an existing Paperless library

Gradually process the full archive in the background. Newly consumed documents
have priority over backfill, so historical processing does not delay current
inbox traffic.

### Resume very large documents safely

Process documents in resumable five-page batches with no page-count cutoff. A
restart continues from stored progress rather than discarding all completed
batches.

## Processing Flow

```text
Paperless ingestion and native OCR
                |
                v
Authenticated Paperless workflow webhook ----+
                                               |
Periodic reconciliation and archive backfill -+
                                               |
                                               v
                                  Durable SQLite priority queue
                                               |
                                               v
                              Download PDF and native OCR draft
                                               |
                          +--------------------+--------------------+
                          |                                         |
                          v                                         v
                 Send PDF directly                         Render page images
                 when supported                            with Poppler
                          |                                         |
                          +--------------------+--------------------+
                                               |
                                               v
                              Validate every page transcription
                                               |
                          +--------------------+--------------------+
                          |                                         |
                     success                                     failure
                          |                                         |
                          v                                         v
             Replace document.content                  Keep native OCR unchanged
             Add ai-ocr-complete                       Add ai-ocr-failed
             Remove ai-ocr-failed                      Do not call paperless-ai
                          |
                          v
                 Invoke paperless-ai webhook
```

Only one document and one model request are active at a time. This keeps model
usage predictable and avoids overwhelming Paperless or the configured AI
gateway.

## Safety Guarantees

- Partial model output is never written to Paperless.
- The original file checksum is checked again before finalization; changed
  documents are not overwritten with stale results.
- Provider responses are treated as untrusted input and must match the expected
  page-oriented JSON structure.
- A failed transcription preserves the native Paperless OCR content.
- Successful documents are not reprocessed merely because the model or prompt
  configuration changes.
- Missed webhook deliveries are recovered by periodic polling.
- Expired processing leases are recovered after a restart.
- Credentials, PDFs, OCR text, transcriptions, images, and provider response
  bodies are excluded from logs.

## Planned Deployment

The first release will publish multi-architecture images for `linux/amd64` and
`linux/arm64`:

```text
ghcr.io/nosovk/paperless-ai-ocr:<version>
```

The intended deployment is on the same private container network as
Paperless-ngx. The service does not need public ingress: Paperless calls its
authenticated webhook directly over the private network.

An illustrative Compose service will look like this after the first image is
published:

```yaml
services:
  paperless-ai-ocr:
    image: ghcr.io/nosovk/paperless-ai-ocr:<version>
    restart: unless-stopped
    environment:
      PAPERLESS_URL: http://paperless-webserver:8000
      PAPERLESS_API_TOKEN: ${PAPERLESS_API_TOKEN}
      AI_BASE_URL: ${AI_BASE_URL}
      AI_API_KEY: ${AI_API_KEY}
      AI_MODEL: ${AI_MODEL}
      WEBHOOK_TOKEN: ${WEBHOOK_TOKEN}
      PAPERLESS_AI_WEBHOOK_URL: http://paperless-ai:3000/api/webhook/manual
      PAPERLESS_AI_WEBHOOK_KEY: ${PAPERLESS_AI_WEBHOOK_KEY}
    volumes:
      - paperless-ai-ocr-data:/app/data
    tmpfs:
      - /tmp:size=1G,mode=0700
    networks:
      - paperless
    read_only: true
    cap_drop:
      - ALL
    security_opt:
      - no-new-privileges:true

volumes:
  paperless-ai-ocr-data:

networks:
  paperless:
    external: true
```

Exact image tags, configuration options, health checks, and migration guidance
will be documented with the first release.

## Paperless Workflow

Create a Paperless workflow that runs after document consumption finishes and
sends the document ID to:

```text
POST http://paperless-ai-ocr:<port>/webhooks/paperless
Authorization: Bearer <dedicated-webhook-token>
Content-Type: application/json
```

The handler authenticates the request, durably queues the document, and returns
HTTP `202` without waiting for OCR to finish. Polling remains enabled as a
recovery mechanism and provides the initial archive backfill.

After integrating this service, disable the periodic document scanner in
`paperless-ai`. `paperless-ai-ocr` becomes responsible for invoking its webhook
after a successful content replacement, preventing metadata extraction from
racing ahead of corrected OCR.

The exact Paperless request body and workflow configuration will be provided
and tested before the first release.

## Document States

The service exposes its terminal result through Paperless tags:

| Tag | Meaning |
| --- | --- |
| `ai-ocr-complete` | The full transcription was validated and committed. |
| `ai-ocr-failed` | Retries were exhausted; native OCR remains in place. |

SQLite is authoritative for queue and processing state. Tags are an
operator-visible summary and are not the sole idempotency mechanism.

## Operations

The service will expose:

| Endpoint | Purpose |
| --- | --- |
| `GET /health` | Process liveness without dependency checks. |
| `GET /ready` | Database, Paperless, AI transport, and worker readiness. |
| `GET /metrics` | Prometheus queue, outcome, retry, latency, and page metrics. |
| `POST /webhooks/paperless` | Authenticated document enqueue endpoint. |

Processing is deliberately sequential. Webhook jobs have higher priority than
archive backfill jobs, while retries use bounded exponential backoff and honor
provider `Retry-After` responses.

## AI Gateway Requirements

The configured endpoint must provide an OpenAI-compatible multimodal API and
support at least one of these transports:

1. Native PDF or document input.
2. OpenAI-style image content for Poppler-rendered PNG pages.

Startup capability probing uses a synthetic document and no user data. The
service will fail readiness rather than silently fall back to a text-only
completion.

## Privacy And Security

This service sends document content to the configured AI endpoint. Operators
are responsible for choosing a provider and deployment model appropriate for
their documents, jurisdiction, retention requirements, and threat model.

Planned security defaults include:

- dedicated bearer authentication for the Paperless webhook;
- secrets supplied through environment variables or secret files;
- an unprivileged container with all Linux capabilities dropped;
- a read-only root filesystem;
- private-network deployment without public ingress;
- mode-0700 persistent and temporary storage;
- bounded request, response, render, and temporary-storage sizes;
- no document content or credentials in logs or Prometheus labels.

SQLite contains processing state and temporary validated batch
transcriptions. The data volume should be stored securely and included only in
appropriately encrypted backups.

## Project Status

The implementation is planned in incremental, test-driven stages covering the
queue, Paperless client, authenticated webhook, reconciliation, Poppler
rendering, AI transport, strict validation, resumable processing, finalization,
observability, container hardening, CI, GHCR publishing, and end-to-end burn-in.

Until a versioned release is published, this repository should not be deployed
for production document processing.

## Development

Development requires Go 1.26. Run the test suite and inspect the development
build metadata with:

```sh
go test ./...
go run ./cmd/paperless-ai-ocr --version
```

## License

`paperless-ai-ocr` is available under the [MIT License](LICENSE).
