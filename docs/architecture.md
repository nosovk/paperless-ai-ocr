# Architecture

## Components

- The HTTP server exposes liveness, readiness, metrics, and the authenticated
  Paperless webhook.
- The reconciler resolves webhook candidates and incrementally scans the
  Paperless archive for backfill.
- SQLite stores candidates, jobs, leases, batch checkpoints, finalization
  checkpoints, and reconciliation progress.
- The worker downloads and inspects one PDF, selects direct PDF or rendered
  image transport, validates model output, and stores batch checkpoints.
- The finalizer replaces Paperless content, updates tags, and dispatches the
  document URL to Paperless AI.

## Data Flow

1. Paperless sends `{ "document_id": 123 }` with bearer authentication. The
   strict handler accepts one positive integer field and returns `202` only
   after the candidate is stored durably.
2. Reconciliation resolves the current Paperless checksum and creates a job.
   It also scans one archive API page per pass by default. Webhook priority is
   higher than backfill priority.
3. The single worker leases the highest-priority due job, downloads the
   original PDF, verifies the Paperless checksum, and reads the bounded native
   OCR draft.
4. The worker sends either one eligible PDF or batches of Poppler-rendered PNG
   pages to the configured OpenAI-compatible Responses API. The native OCR is a
   hint, not an authority.
5. Every response must match the expected page range and canonical JSON schema.
   Completed batch transcriptions are checkpointed in SQLite.
6. After all batches validate, the worker reconstructs the full transcription.
   The finalizer checks the checksum again before replacing
   `document.content`.
7. The finalizer adds `ai-ocr-complete`, removes `ai-ocr-failed`, and calls the
   configured Paperless AI webhook. The complete tag records committed validated
   content, not downstream dispatch. Only HTTP `202` confirms dispatch, and an
   ambiguous or non-`202` outcome can leave the complete tag present while the
   queue job fails.

Only complete validated output reaches Paperless. Temporary PDFs and images are
created under `/tmp` and removed with the per-job workspace. Validated batch
transcriptions remain in SQLite.

## Hallucination And Audit Sources

Schema validation verifies structure, page coverage, and response bounds. It
cannot establish textual truth. An LLM can hallucinate, omit, duplicate, or
alter content while producing schema-valid output.

The original PDF remains in Paperless and is the audit source. Successful AI OCR
overwrites the native OCR stored in `document.content`, and this service does
not retain a separate native OCR copy. Export or back up native OCR before
processing if it must remain available after success. A terminal OCR failure
before successful content replacement leaves native OCR unchanged. AI
transcription must not be treated as authoritative legal, financial, medical,
or evidentiary content.

## Idempotency And Change Detection

Jobs are identified by document ID and source checksum. Existing active or
terminal work for the same checksum is reused. Model and prompt identifiers are
stored with the job, but model-only or prompt-only changes do not reprocess a
completed same-checksum document.

Finalization stores checkpoints around externally visible effects. A source
checksum change prevents stale OCR from being written. Tags are operator-visible
summaries, not the sole idempotency mechanism.

## Retry And Failure Semantics

There are three distinct layers:

1. Provider attempts: each batch has `MODEL_ATTEMPTS` attempts, default 3 and
   maximum 10. Retryable provider failures use bounded exponential backoff from
   one to 30 seconds and honor a valid provider `Retry-After` delay.
2. Durable job retries: cancellation, document deadlines, shutdown release, and
   finalization interruptions before a terminal decision move the job to
   `retry`, normally after one minute. Completed batch checkpoints are reused.
   There is no configurable global maximum number of durable job attempts.
3. Terminal failures: most Paperless document lookup, original download, PDF
   inspection, validation, rendering, unsupported document bounds, and
   exhausted or non-retryable OCR provider failures record terminal intent,
   preserve Paperless native OCR, add `ai-ocr-failed`, and do not call Paperless
   AI. Cancellation, deadline, lost-lease, and checkpoint-reconstruction paths
   have separate handling and do not all enter this terminal path.

Paperless AI dispatch has an additional duplicate-effect boundary. The runtime
reserves dispatch before making the HTTP request. Its production
`paperlessai.Client` does not return retry-safe errors, so every transport error
or non-`202` response after reservation is an ambiguous terminal outcome and is
not sent again. The internal finalizer protocol can reset a reservation if a
future dispatcher explicitly supplies retry-safe classification, but that path
is not reachable with the current production client.

## Startup And Readiness

The HTTP listener starts before dependency initialization, but `/ready` remains
`503`. Readiness becomes `200` only after expired lease recovery, Paperless
ping, synthetic AI capability probe, Poppler inspector and renderer
initialization, and initial reconciliation succeed. A later reconciliation
failure makes the service unready and stops background processing.

## Durable Storage

The database is `/app/data/paperless-ai-ocr.db`, opened with mode `0600`, one
connection, foreign keys, a five-second busy timeout, and SQLite WAL mode. The
service does not automatically delete completed jobs, failed jobs, finalization
records, or completed batch transcriptions.

See [Operations](operations.md) for backup and restore procedures and
[Threat model](threat-model.md) for trust boundaries.
