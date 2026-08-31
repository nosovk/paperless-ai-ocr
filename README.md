# paperless-ai-ocr

Multimodal AI transcription for
[Paperless-ngx](https://github.com/paperless-ngx/paperless-ngx).

`paperless-ai-ocr` sends an original PDF or Poppler-rendered page images to a
vision-capable model, validates a complete page-oriented response, replaces
`document.content`, and then invokes the configured
[`paperless-ai`](https://github.com/clusterzx/paperless-ai) webhook. Paperless
native OCR is supplied to the model as a bounded hint.

> [!WARNING]
> LLM transcription can omit, invent, or alter text while still returning
> schema-valid output. The original PDF remains in Paperless as the audit
> source. A successful AI OCR update overwrites Paperless native OCR in
> `document.content`; export or back it up before processing if it must be
> retained. Do not treat AI transcription as authoritative evidence.

## Processing Summary

```text
Paperless webhook or periodic reconciliation
                    |
                    v
         Durable SQLite priority queue
                    |
                    v
       Download original PDF and native OCR
                    |
          +---------+---------+
          |                   |
          v                   v
 Direct PDF input      Poppler page images
          |                   |
          +---------+---------+
                    |
                    v
       Validate all page transcriptions
                    |
          +---------+---------+
          |                   |
       success              failure
          |                   |
          v                   v
 Replace content       Leave native OCR unchanged
 Add complete tag      Add failed tag
 Invoke paperless-ai   Do not dispatch
```

Only one document and one model request are active at a time. Webhook work has
priority over archive backfill. Documents are limited to 10,000 pages.

## Important Behavior

- Direct PDF transport is used only for PDFs of at most five pages and 8 MiB.
  Other documents use Poppler-rendered images. There is no text-only fallback.
- `BATCH_SIZE` controls image batches and accepts values from 1 through 5.
- Partial model output is never written to Paperless.
- Successful AI OCR replaces the native OCR in `document.content`; this service
  does not retain a separate native OCR copy. Export or back up native OCR before
  processing when it must remain available.
- An OCR terminal failure before successful content replacement leaves native
  OCR unchanged.
- The source checksum is checked before processing and finalization.
- Completed documents with the same checksum are not reprocessed solely
  because the model or prompt changes.
- A successful Paperless AI dispatch requires HTTP `202`. Ambiguous downstream
  outcomes are not automatically retried because that could duplicate effects.
- `ai-ocr-complete` confirms committed validated content, not successful
  Paperless AI dispatch. An ambiguous dispatch can leave the tag present while
  the queue job is failed.
- SQLite retains completed jobs and validated batch transcriptions. There is no
  automatic retention cleanup.

See [Architecture](docs/architecture.md) for the complete data flow and failure
semantics.

## Deployment

The container runs as UID/GID `65532`, includes Poppler, and stores SQLite at
`/app/data/paperless-ai-ocr.db`. `/app/data` and `/tmp` must be writable; the
root filesystem can remain read-only.

Use [`compose.example.yaml`](compose.example.yaml) as the hardened baseline:

```sh
docker compose -f compose.example.yaml up -d
```

The service requires environment configuration. `_FILE` variants are not
implemented. See [Configuration](docs/configuration.md) for every variable and
its validation rules.

## Paperless Workflow

Configure a Paperless post-consumption workflow to send exactly:

```http
POST /webhooks/paperless HTTP/1.1
Authorization: Bearer <WEBHOOK_TOKEN>
Content-Type: application/json

{ "document_id": 123 }
```

The body must be one JSON object containing only a positive integer
`document_id`. A durably accepted request returns HTTP `202`; OCR continues
asynchronously. Periodic reconciliation recovers missed webhook delivery and
performs archive backfill.

Disable the periodic document scanner in `paperless-ai`. This service dispatches
to Paperless AI only after successful OCR finalization.

## Endpoints

| Endpoint | Behavior |
| --- | --- |
| `GET /health` | Returns `200` when the process can serve HTTP. |
| `GET /ready` | Returns `200` only after startup dependencies and initial reconciliation succeed. |
| `GET /metrics` | Returns bounded Prometheus text, or `503` if collection fails. |
| `POST /webhooks/paperless` | Authenticates and durably queues a document candidate. |

Readiness waits for expired lease recovery, Paperless connectivity, the AI
capability probe, Poppler initialization, and initial reconciliation.

## Documentation

- [Configuration](docs/configuration.md)
- [Architecture](docs/architecture.md)
- [Operations](docs/operations.md)
- [Threat model](docs/threat-model.md)
- [Security policy](SECURITY.md)
- [Contributing](CONTRIBUTING.md)

## Privacy

Document PDFs or rendered images, native OCR, and model output leave the
container for the configured AI gateway. Operators must select endpoints whose
location, access controls, training policy, logging, and retention are suitable
for their documents. SQLite also retains validated transcriptions locally.

See [Threat model](docs/threat-model.md) before deployment and follow
[Operations](docs/operations.md) for encrypted backup and safe diagnostics.

## License

`paperless-ai-ocr` is available under the [MIT License](LICENSE).
