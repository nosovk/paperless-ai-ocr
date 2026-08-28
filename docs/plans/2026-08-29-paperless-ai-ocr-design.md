# Paperless AI OCR Design

## Goal

Build a public Go service that improves Paperless-ngx full-text content by
transcribing every document with a multimodal model. The model receives both
the original PDF and Paperless's native OCR text as an untrusted draft. A
validated transcription atomically replaces the document's `content` before
the existing Paperless AI metadata processor is invoked.

## Scope

The first release supports:

- new documents discovered through an authenticated Paperless workflow webhook;
- polling reconciliation so missed webhooks are recovered;
- an automatic, low-priority backfill of the entire existing archive;
- one active document and one active model request at a time;
- direct PDF input when the configured OpenAI-compatible endpoint supports it;
- page-image fallback using Poppler when direct PDF input is unavailable;
- resumable batches for arbitrarily long PDFs;
- replacement of Paperless `content` only after complete validation;
- success and terminal-failure tags;
- dispatch to Paperless AI only after successful OCR;
- health, readiness, and Prometheus metrics endpoints;
- a multi-architecture public image published to GHCR.

The first release does not provide a user-facing dashboard, edit Paperless
metadata other than OCR state tags and `content`, or automatically reprocess
successful documents solely because the model or prompt version changed.

## Processing Flow

Paperless performs normal ingestion and native OCR first. A `Consumption
Finished` workflow sends the document ID to the service. The webhook validates
a dedicated bearer token, durably enqueues the ID, and immediately returns
HTTP 202.

The worker downloads the current document record and original PDF, calculates
the source checksum, and captures the native OCR draft. It attempts the
configured direct-PDF multimodal contract when capability probing has confirmed
support. Otherwise it uses `pdfinfo` to determine page count and `pdftoppm` to
render only the next batch of five pages at 200 DPI.

Each model request includes visual document input, page identity, and the
relevant native OCR draft. The prompt requires faithful transcription and
forbids summarization, translation, normalization, completion, and inference.
Unreadable content is represented explicitly rather than guessed.

Validated batch results are stored in SQLite. Partial output is never written
to Paperless. After all pages are complete, the service joins and validates the
transcription, fetches the source checksum again, and aborts if the document
changed during processing. It then replaces `content`, applies
`ai-ocr-complete`, removes `ai-ocr-failed`, and invokes the Paperless AI webhook.

If all retries are exhausted, the native OCR remains untouched,
`ai-ocr-failed` is applied, and Paperless AI metadata processing is not invoked.

## Discovery And Priority

Webhook jobs have higher priority than archive backfill jobs. A reconciler
periodically scans Paperless for documents that do not have a current terminal
state in the local database. This recovers missed webhooks and gradually
backfills the existing archive.

Successful documents are not automatically reprocessed when only the model or
prompt version changes. They are reprocessed when their original-file checksum
changes. Failed jobs may be retried explicitly or according to retry policy.

## Persistence

SQLite is stored under `/app/data`. Migrations create:

- `jobs`: document identity, source checksum, priority, state, attempts, lease,
  model, prompt version, safe error category, and timestamps;
- `batches`: page range, render settings, state, attempts, validated result,
  and timestamps;
- `settings`: schema and reconciliation checkpoints.

Job states are `pending`, `processing`, `retry`, `completed`, and `failed`.
Batch states use the same lifecycle. On startup, expired processing leases are
returned to a retryable state. Constraints prevent more than one current job
for the same document and source checksum.

The database never stores API credentials, PDFs, rendered images, or the native
OCR draft. Batch transcription is sensitive document content; the data volume
is mounted from a mode-0700 host directory and must be included in encrypted
backups. Completed batch text may be deleted after the final Paperless update
to reduce retained sensitive data.

## Model Contract

The service targets an OpenAI-compatible AI gateway but isolates provider
details behind an internal interface. Startup capability probing uses a
synthetic document and no user data. Configuration selects or confirms one of
these transports:

1. Native PDF/document input.
2. OpenAI-style image content using rendered PNG pages.

The service fails readiness if neither transport works. It does not silently
fall back to text-only completion.

The response uses structured JSON containing ordered page records. Validation
requires valid JSON, unique and contiguous requested page numbers, non-empty
text, bounded output size, no unknown pages, and no provider refusal. Model
output is treated as untrusted input.

## Large Documents

There is no page-count cutoff. Documents are processed in sequential batches of
five pages. Pages are rendered on demand, and temporary images are deleted as
soon as the corresponding validated batch result is committed.

Defaults:

- render resolution: 200 DPI;
- active documents: 1;
- active model requests: 1;
- model attempts per batch: 3;
- render timeout: 5 minutes per batch;
- model timeout: 3 minutes per batch;
- document deadline: 6 hours;
- temporary-render budget: 1 GiB.

Insufficient disk space, deadline expiry, rendering failure, invalid model
output, and provider rate limiting produce explicit retry or terminal error
categories. Retry delays honor `Retry-After` and otherwise use capped
exponential backoff with jitter.

## Security

The Paperless webhook requires a dedicated bearer token and compares it in
constant time. The Paperless API token, AI API key, downstream Paperless AI
webhook key, and inbound webhook token are provided only through environment or
secret files.

Logs contain document IDs, page ranges, durations, states, and safe error
categories. They never contain credentials, request bodies, OCR text,
transcriptions, PDFs, image data, or unredacted provider responses.

The container runs as an unprivileged user, drops all Linux capabilities,
enables `no-new-privileges`, has a read-only root filesystem, and receives
writable mounts only for `/app/data` and a bounded temporary directory. It is
not exposed through Traefik; Paperless reaches it over the private Compose
network.

## Operational Endpoints

- `GET /health` reports process liveness without testing dependencies.
- `GET /ready` reports database migration status, Paperless connectivity,
  successful model capability probing, and worker initialization.
- `GET /metrics` exposes queue depth, job outcomes, retries, provider latency,
  processed pages, rendered bytes, and reconciliation results without document
  content or credentials.
- `POST /webhooks/paperless` authenticates and queues a document ID.

## Deployment

The repository is public at `github.com/nosovk/paperless-ai-ocr` under the MIT
license. GitHub Actions tests and builds with Go 1.26, publishes linux/amd64 and
linux/arm64 images, creates an OCI manifest, SBOM, and provenance, and pushes:

```text
ghcr.io/nosovk/paperless-ai-ocr:<version>
```

Homelab deployment pins the manifest digest. Private deployment configuration,
domains, tokens, model names, SOPS files, runtime paths, and Paperless workflows
remain in the homelab repository.

The existing Paperless AI periodic scanner is disabled. Only this service
dispatches a document to Paperless AI after corrected `content` is committed,
which prevents metadata processing from racing ahead with native OCR.

## Verification

Tests use fake Paperless and AI HTTP servers plus synthetic PDFs. They cover
authentication, reconciliation, queue priority, retries, lease recovery,
direct-PDF and rendered-page transports, response validation, checksum races,
atomic Paperless update ordering, tag behavior, downstream dispatch, log
redaction, restart resumption, long-document batching, and failure preservation
of native OCR.

Runtime acceptance uses disposable documents to confirm:

- webhook enqueue and polling recovery;
- full archive backfill at concurrency one;
- corrected content is searchable in Paperless;
- failed AI OCR leaves native content unchanged;
- Paperless AI runs only after successful content replacement;
- restart resumes an incomplete long document without repeating completed
  batches;
- successful documents are not reprocessed merely after a model or prompt
  configuration change.
