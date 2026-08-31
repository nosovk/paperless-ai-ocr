# Operations

## Startup Checks

Monitor both endpoints:

- `/health` reports process liveness and does not check dependencies.
- `/ready` reports dependency-aware readiness. Startup waits for lease
  recovery, Paperless ping, AI capability probing, Poppler initialization, and
  initial reconciliation.

A running but unready container should not receive webhook traffic. Inspect only
the safe structured logs and aggregate metrics described below.

## Backup

There is no built-in backup command. SQLite uses WAL mode, so copying only the
main `.db` file while the service is running can produce an inconsistent backup.

Use one of these approaches:

1. Stop the container cleanly, back up the complete `/app/data` volume, and
   restart it.
2. Use a SQLite-aware online backup tool that includes a consistent WAL
   snapshot.

Encrypt backups because the database contains document IDs, checksums, safe
diagnostics, processing state, and validated transcriptions. Restrict access and
define an operator retention policy; the service performs no automatic cleanup
of completed data.

## Restore

1. Stop the service.
2. Preserve the current data volume separately until validation is complete.
3. Restore a consistent backup to `/app/data/paperless-ai-ocr.db` and any
   SQLite companion files supplied by the backup method.
4. Ensure the database file is owned by the runtime UID/GID and can be secured
   to mode `0600`.
5. Start the same application version first and wait for `/ready`.
6. Check queue metrics and Paperless tags before resuming webhook delivery.

Startup migrations run automatically. Do not edit queue state with direct SQL.

## Upgrade

1. Read release notes and back up the data volume.
2. Pin the target immutable image version rather than a mutable tag.
3. Stop the existing container cleanly so the active lease is returned to
   retry state when possible.
4. Start one replacement container against the existing data volume.
5. Wait for `/ready`, then monitor retries, failures, queue depth, and provider
   latency totals.

Do not run multiple service replicas against the same operational queue. The
runtime is deliberately single-document and single-model-request.

## Model And Prompt Changes

Test a new endpoint or model on synthetic documents in a separate environment.
Confirm the startup probe selects the expected transport and manually compare
transcription against original PDFs and native OCR.

The original PDF remains in Paperless after processing. Successful AI OCR
overwrites native OCR in `document.content`, and this service does not keep a
separate native OCR copy. Export or back up native OCR before processing if an
operator must inspect it after success. A terminal OCR failure before successful
content replacement leaves native OCR unchanged.

A model-only or compiled prompt-only change does not reprocess completed
same-checksum documents. The project exposes no supported bulk reprocessing
command. Do not force reprocessing by mutating SQLite. Plan any required
reprocessing outside the service and preserve audit sources.

## Backfill

Periodic reconciliation scans the Paperless archive and stores its pagination
checkpoint. It processes one archive API page per pass by default. Webhook
candidates are resolved before archive pages, and webhook jobs have higher queue
priority than backfill jobs.

Use `POLL_INTERVAL` to control how often passes run. Large archives progress
incrementally. A reconciliation error makes the service unready rather than
silently skipping the failed scan.

## Tags And Outcomes

| Tag | Meaning |
| --- | --- |
| `ai-ocr-complete` | The validated transcription was committed and the complete-tag checkpoint was reached. It does not confirm Paperless AI dispatch. |
| `ai-ocr-failed` | OCR reached its terminal failure path before successful content replacement; native OCR remains unchanged. |

SQLite is authoritative for processing state. Tags are not sufficient to infer
every intermediate finalization checkpoint or every failed queue outcome. For
example, an ambiguous Paperless AI dispatch can fail the queue job after content
replacement while leaving `ai-ocr-complete` present and without adding
`ai-ocr-failed`.

## Manual Retry

There is no public manual retry CLI or API. An internal queue method is not a
supported operator interface. Do not publish or use direct SQL state mutations;
they can violate leases, finalization checkpoints, and duplicate-effect guards.

Automatic durable retries are narrow: cancellation or document deadline,
shutdown release, expired lease recovery, and finalization interruptions before
a terminal decision. Most Paperless document lookup, original download, PDF
inspection, validation, rendering, local request-size, and exhausted provider
errors instead enter terminal OCR intent and add `ai-ocr-failed`. Lost-lease and
checkpoint-reconstruction errors have separate handling and are not operator
retry interfaces. Terminal failures require investigation and an externally
planned recovery or source-document change.

## Metrics

`GET /metrics` exposes fixed labels only and never uses document IDs. If current
queue depth collection fails, the endpoint returns HTTP `503` with
`metrics unavailable`. The exposition contains no `# TYPE` declarations, so
Prometheus treats the emitted samples as untyped. The descriptions below state
their intended operational semantics, not an emitted Prometheus type.

| Metric | Intended semantics and labels |
| --- | --- |
| `paperless_ai_ocr_queue_depth` | Current value by `state`: `pending`, `processing`, `retry`, `completed`, `failed`. |
| `paperless_ai_ocr_job_outcomes_total` | Cumulative finalized outcomes by `outcome`: `success`, `failure`. `failure` is recorded only when an OCR terminal-intent path is successfully finalized. Success-finalization errors, source races, and ambiguous Paperless AI dispatch outcomes are not counted as failures; they are also not counted as successes. |
| `paperless_ai_ocr_retries_total` | Cumulative durable retry transitions observed by the runtime. |
| `paperless_ai_ocr_provider_latency_seconds` | Cumulative latency sum with `operation`: `probe`, `transcribe`; not a histogram. |
| `paperless_ai_ocr_provider_requests_total` | Cumulative observations recorded with provider latency by `operation`: `probe`, `transcribe`. Each transcription attempt is observed. One successful overall startup probe is observed once even if capability detection issued both PDF and image HTTP requests; a failed startup probe is not recorded. |
| `paperless_ai_ocr_processed_pages_total` | Cumulative validated checkpointed pages. |
| `paperless_ai_ocr_rendered_bytes_total` | Cumulative rendered page bytes read for model input. |
| `paperless_ai_ocr_recovered_leases_total` | Cumulative expired leases recovered at startup. |
| `paperless_ai_ocr_reconciliation_results_total` | Cumulative values by `result`: `candidates_resolved`, `candidates_discarded`, `documents_seen`, `jobs_created`, `pages_processed`, `scans_completed`. |

## Safe Diagnostics

Logs use a fixed field allowlist: level, event, document ID, page range,
duration, queue state, and safe error category. Document IDs are present in
job-level logs. Credentials, document bodies, OCR, transcriptions, filenames,
images, and raw provider or HTTP responses are not allowlisted.

Production wiring emits startup, ready, shutdown, job claimed, job finished,
and categorized background failure events. Do not rely on batch-completed log
events; that logger method is not wired into production processing.

When requesting support, provide versions, aggregate metrics, safe event names,
safe categories, and a synthetic reproduction. Follow
[SECURITY.md](../SECURITY.md) for sensitive reports. Never attach real
documents, database copies, environment dumps, or raw provider responses.
