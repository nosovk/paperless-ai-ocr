# Paperless AI OCR Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Build and publish a secure Go service that replaces Paperless-ngx native OCR text with a validated multimodal transcription before triggering Paperless AI metadata processing.

**Architecture:** A webhook and polling reconciler feed a durable SQLite priority queue. One worker downloads a PDF and native OCR, uses direct PDF input or Poppler-rendered page batches with an OpenAI-compatible multimodal gateway, validates all output, atomically updates Paperless, and then dispatches the document to Paperless AI.

**Tech Stack:** Go 1.26, standard `net/http`, `modernc.org/sqlite`, Poppler (`pdfinfo`, `pdftoppm`), Prometheus client, Docker Buildx, GitHub Actions, GHCR, SPDX/CycloneDX SBOM and GitHub artifact attestations.

---

### Task 1: Bootstrap The Go Module

**Files:**
- Create: `go.mod`
- Create: `cmd/paperless-ai-ocr/main.go`
- Create: `internal/buildinfo/buildinfo.go`
- Create: `internal/buildinfo/buildinfo_test.go`
- Modify: `README.md`

**Steps:**
1. Write a failing test asserting development build metadata has non-empty version, revision, and build time placeholders.
2. Run `go test ./internal/buildinfo` and verify it fails because the package is absent.
3. Initialize module `github.com/nosovk/paperless-ai-ocr` with `go 1.26.0` and implement immutable build metadata populated by linker flags.
4. Add a minimal `main` that supports `--version` and otherwise exits with a clear not-yet-configured error.
5. Run `go test ./...` and `go run ./cmd/paperless-ai-ocr --version`.
6. Commit with `feat: bootstrap Go service`.

### Task 2: Typed Configuration And Secret Safety

**Files:**
- Create: `internal/config/config.go`
- Create: `internal/config/config_test.go`
- Create: `internal/saferr/saferr.go`
- Create: `internal/saferr/saferr_test.go`

**Steps:**
1. Write table-driven failing tests for required URLs/tokens/model, defaults, duration/size parsing, invalid schemes, and redacted formatting.
2. Run `go test ./internal/config ./internal/saferr` and verify failure.
3. Implement a typed `Config` loaded from environment with defaults for ports, polling, rendering, retries, deadlines, and concurrency fixed to one.
4. Implement categorized errors whose public strings cannot contain configured secrets or provider bodies.
5. Require `PAPERLESS_URL`, `PAPERLESS_API_TOKEN`, `AI_BASE_URL`, `AI_API_KEY`, `AI_MODEL`, `WEBHOOK_TOKEN`, `PAPERLESS_AI_WEBHOOK_URL`, and `PAPERLESS_AI_WEBHOOK_KEY`.
6. Run focused tests, `go test ./...`, and `go vet ./...`.
7. Commit with `feat: add validated configuration`.

### Task 3: SQLite Schema And Migrations

**Files:**
- Create: `internal/database/database.go`
- Create: `internal/database/migrations.go`
- Create: `internal/database/database_test.go`
- Create: `internal/database/testdata/`

**Steps:**
1. Write failing tests for first migration, repeated migration, foreign keys, WAL mode, busy timeout, schema version, and restrictive database file permissions.
2. Define `jobs`, `batches`, and `settings` tables with state checks, uniqueness constraints, timestamps, priorities, leases, and safe error fields.
3. Use embedded, ordered migrations executed transactionally.
4. Configure SQLite for one writer, WAL, foreign keys, and bounded busy timeout.
5. Run `go test ./internal/database -race`, then `go test ./... -race`.
6. Commit with `feat: add durable job database`.

### Task 4: Durable Priority Queue

**Files:**
- Create: `internal/queue/queue.go`
- Create: `internal/queue/model.go`
- Create: `internal/queue/queue_test.go`

**Steps:**
1. Write failing tests for idempotent enqueue, webhook-over-backfill priority, atomic claim, one active job, retry scheduling, terminal failure, completion, and expired lease recovery.
2. Implement queue transitions in SQL transactions and reject illegal transitions.
3. Key current work by document ID and source checksum while retaining completed history.
4. Ensure a model or prompt change alone does not enqueue completed documents.
5. Run `go test ./internal/queue -race -count=10` and the full suite.
6. Commit with `feat: implement durable priority queue`.

### Task 5: Paperless API Client

**Files:**
- Create: `internal/paperless/client.go`
- Create: `internal/paperless/types.go`
- Create: `internal/paperless/client_test.go`

**Steps:**
1. Build an `httptest.Server` and write failing tests for pagination, document detail, original download streaming, checksum retrieval, content PATCH, tag lookup/create, tag replacement preserving unrelated tags, and auth redaction.
2. Implement bounded HTTP transport, context deadlines, response-size limits, pagination loop detection, and typed status errors.
3. Expose methods needed by reconciliation and the worker, not a generic REST abstraction.
4. Verify content PATCH and tag updates are separate explicit operations with deterministic ordering.
5. Run focused and full race tests.
6. Commit with `feat: add Paperless API client`.

### Task 6: Authenticated Webhook Server

**Files:**
- Create: `internal/server/server.go`
- Create: `internal/server/webhook.go`
- Create: `internal/server/webhook_test.go`

**Steps:**
1. Write failing tests for missing/malformed/wrong bearer tokens, constant-time verifier behavior, malformed JSON, oversized body, valid document ID, duplicate delivery, and immediate HTTP 202.
2. Implement Go 1.26 `http.ServeMux` routes and strict JSON decoding.
3. Read no Paperless document data inside the handler; only enqueue a high-priority candidate.
4. Disable request-body and authorization logging.
5. Run tests with race detection and commit with `feat: add authenticated Paperless webhook`.

### Task 7: Reconciler And Initial Backfill

**Files:**
- Create: `internal/reconcile/reconcile.go`
- Create: `internal/reconcile/reconcile_test.go`

**Steps:**
1. Write failing tests for paginated archive discovery, checkpoint recovery, missed webhook recovery, backfill priority, completed-document suppression across model/prompt changes, checksum-change requeue, and temporary Paperless failure.
2. Implement a bounded polling pass that persists progress and yields between pages.
3. Queue new documents above backfill while preserving deterministic ordering within each priority.
4. Do not rely only on tags for idempotency; SQLite is authoritative and tags are operator-visible state.
5. Run repeated race tests and commit with `feat: reconcile Paperless archive`.

### Task 8: Temporary Workspace And PDF Inspection

**Files:**
- Create: `internal/pdf/workspace.go`
- Create: `internal/pdf/inspect.go`
- Create: `internal/pdf/inspect_test.go`
- Create: `testdata/pdfs/`

**Steps:**
1. Add small synthetic one-page and multi-page PDFs safe for public source control.
2. Write failing tests for page counting, malformed PDFs, command timeout, path safety, cleanup, free-space checks, and temporary byte budget.
3. Implement `pdfinfo` execution without a shell, with a clean environment, context cancellation, bounded stderr, and strict numeric parsing.
4. Create per-job mode-0700 workspaces and guarantee cleanup through normal and cancellation paths.
5. Run tests inside a development container with Poppler installed.
6. Commit with `feat: inspect PDF documents safely`.

### Task 9: On-Demand Page Rendering

**Files:**
- Create: `internal/pdf/render.go`
- Create: `internal/pdf/render_test.go`

**Steps:**
1. Write failing tests for requested page ranges, 200 DPI default, deterministic filenames, timeout, partial command failure, output ordering, byte limits, and immediate cleanup after callback completion.
2. Implement `pdftoppm` range rendering without rendering the full document in advance.
3. Verify PNG signatures and reject missing, duplicate, extra, or oversized outputs.
4. Keep command output free of document paths in public errors.
5. Run Poppler integration tests and commit with `feat: render PDF page batches`.

### Task 10: AI Gateway Capability Probe

**Files:**
- Create: `internal/aigate/client.go`
- Create: `internal/aigate/capability.go`
- Create: `internal/aigate/capability_test.go`
- Create: `testdata/probe/`

**Steps:**
1. Write failing fake-server tests for direct PDF support, image-only support, auth failure, unsupported multimodal input, malformed response, timeout, and provider-body redaction.
2. Define a narrow transport interface with `DirectPDF` and `PageImages` capabilities.
3. Probe using a tiny synthetic document containing no user data and cache the selected capability for process lifetime.
4. Fail readiness if no multimodal transport works; never downgrade to text-only processing.
5. Run focused tests and commit with `feat: probe multimodal gateway capabilities`.

### Task 11: Structured Transcription Client

**Files:**
- Create: `internal/aigate/transcribe.go`
- Create: `internal/aigate/transcribe_test.go`
- Create: `internal/ocr/prompt.go`
- Create: `internal/ocr/prompt_test.go`

**Steps:**
1. Write failing tests that inspect generated requests for PDF/image content, OCR draft placement, page range, model, structured response schema, and absence of secrets in errors.
2. Add a versioned faithful-transcription prompt that treats native OCR as untrusted evidence and forbids summarization, translation, normalization, and inference.
3. Implement direct-PDF and page-image request builders behind the same interface.
4. Bound request and response sizes, honor `Retry-After`, and categorize retryable statuses.
5. Run tests and commit with `feat: add multimodal transcription client`.

### Task 12: Transcription Validation

**Files:**
- Create: `internal/ocr/validate.go`
- Create: `internal/ocr/validate_test.go`

**Steps:**
1. Write table-driven failing tests for valid batches, invalid JSON, missing/duplicate/out-of-range pages, empty text, refusals, unknown fields, excessive output, and suspicious non-transcription prose.
2. Strictly decode ordered page records and reject trailing JSON data.
3. Keep validation conservative and deterministic; do not use another model as validator.
4. Join pages with stable page separators suitable for Paperless full-text search.
5. Run fuzz tests for response parsing plus the full suite.
6. Commit with `feat: validate AI transcriptions`.

### Task 13: Resumable Document Worker

**Files:**
- Create: `internal/worker/worker.go`
- Create: `internal/worker/worker_test.go`

**Steps:**
1. Write failing tests for direct PDF, image fallback, five-page batches, long documents, saved-batch restart, one active request, leases, retries, deadline, cancellation, and terminal failure.
2. Download the PDF to the protected workspace while hashing the stream and capture native OCR without persisting it separately.
3. Reuse validated batch records only when checksum, model, prompt version, page range, and render settings match.
4. Delete rendered images after each committed batch and remove temporary files on every exit path.
5. Preserve native OCR and mark the job failed after retry exhaustion.
6. Run `go test ./internal/worker -race -count=10` and commit with `feat: process documents sequentially`.

### Task 14: Atomic Finalization And Downstream Dispatch

**Files:**
- Create: `internal/finalize/finalize.go`
- Create: `internal/finalize/finalize_test.go`
- Create: `internal/paperlessai/client.go`
- Create: `internal/paperlessai/client_test.go`

**Steps:**
1. Write failing tests proving ordering: refetch checksum, PATCH content, add complete tag, remove fail tag, then invoke Paperless AI.
2. Write failure tests proving checksum mismatch discards output, content PATCH failure changes no tags, and downstream failure is retried without repeating Gemini OCR.
3. Implement the Paperless AI webhook client with dedicated auth and response limits.
4. Store a finalization checkpoint so restart resumes after the last confirmed side effect.
5. Ensure terminal OCR failure applies `ai-ocr-failed` without changing content or dispatching metadata processing.
6. Run race tests and commit with `feat: finalize OCR and dispatch metadata processing`.

### Task 15: Health, Readiness, Metrics, And Lifecycle

**Files:**
- Create: `internal/server/health.go`
- Create: `internal/observability/metrics.go`
- Create: `internal/app/app.go`
- Create: `internal/app/app_test.go`
- Modify: `cmd/paperless-ai-ocr/main.go`

**Steps:**
1. Write failing tests for liveness, dependency-aware readiness, metric names, graceful shutdown, lease release, and startup recovery.
2. Wire database, clients, capability probe, reconciler, server, and one worker with cancellation causes.
3. Keep `/health` independent of external systems and make `/ready` fail until migrations and capability probing complete.
4. Expose aggregate metrics only; labels must not contain document IDs, filenames, URLs, or error messages.
5. Run full race tests and commit with `feat: wire service lifecycle and observability`.

### Task 16: Logging And Security Regression Tests

**Files:**
- Create: `internal/securelog/logger.go`
- Create: `internal/securelog/logger_test.go`
- Create: `internal/security/security_test.go`

**Steps:**
1. Write tests that seed recognizable tokens, OCR text, provider bodies, filenames, and image data, then assert none appear in captured logs or public errors.
2. Implement structured allow-list logging rather than regex-only redaction.
3. Verify webhook auth runs before document lookup and HTTP clients never include auth headers in errors.
4. Add fuzzing for malicious webhook bodies and provider responses.
5. Run `go test ./... -race`, fuzz smoke tests, and `go vet ./...`.
6. Commit with `test: enforce secret-safe diagnostics`.

### Task 17: Container Image

**Files:**
- Create: `Dockerfile`
- Create: `.dockerignore`
- Create: `compose.example.yaml`
- Create: `scripts/container-test.sh`
- Modify: `README.md`

**Steps:**
1. Write a container test that checks non-root UID, Poppler availability, read-only root compatibility, writable data/tmp mounts, healthcheck, graceful termination, and ARM64 image metadata.
2. Build the Go binary in a pinned Go 1.26 builder and copy it into a pinned minimal runtime containing Poppler and CA certificates.
3. Add OCI labels, an exec-form healthcheck, unprivileged user, and no embedded configuration.
4. Document required capabilities and provide hardened Compose settings: `cap_drop: [ALL]`, `no-new-privileges`, read-only root, private network, and bounded tmpfs.
5. Run local amd64 container tests and cross-build linux/arm64.
6. Commit with `build: add hardened multi-arch image`.

### Task 18: Continuous Integration And GHCR Publishing

**Files:**
- Create: `.github/workflows/ci.yml`
- Create: `.github/workflows/release.yml`
- Create: `.golangci.yml`
- Create: `REUSE.toml` or equivalent license metadata if required by selected scanners

**Steps:**
1. Add CI for formatting, vet, lint, unit/race tests, Poppler integration tests, vulnerability scanning, and Docker build.
2. Add release workflow triggered by `v*` tags that builds linux/amd64 and linux/arm64 with Buildx.
3. Publish public GHCR tags and immutable digest, generate SBOM/provenance, and create GitHub artifact attestations using least-privilege workflow permissions.
4. Do not expose real Paperless or AI credentials to CI; all network integrations use fakes.
5. Verify workflows with action linting and commit with `ci: publish verified multi-arch images`.

### Task 19: Public Documentation And Threat Model

**Files:**
- Modify: `README.md`
- Create: `docs/configuration.md`
- Create: `docs/architecture.md`
- Create: `docs/operations.md`
- Create: `docs/threat-model.md`
- Create: `SECURITY.md`
- Create: `CONTRIBUTING.md`

**Steps:**
1. Document data flow, privacy disclosure, supported model transports, all configuration, retries, database retention, backfill, tags, and failure semantics.
2. Clearly state that LLM transcription can hallucinate and original PDFs/native OCR remain the audit sources.
3. Document backup, restore, upgrade, prompt/model changes, manual retry, metrics, and safe diagnostics.
4. Add a security policy and prohibit issue reports containing documents, OCR text, logs with credentials, or provider responses.
5. Run markdown/link checks and commit with `docs: document secure operation`.

### Task 20: End-To-End Acceptance Harness

**Files:**
- Create: `internal/acceptance/acceptance_test.go`
- Create: `testdata/acceptance/`
- Create: `scripts/acceptance.sh`

**Steps:**
1. Build fake Paperless, fake AI Gate, and fake Paperless AI endpoints around a temporary SQLite database and synthetic PDFs.
2. Test webhook success, missed-webhook reconciliation, full backfill, priority preemption, direct-PDF and image fallback, restart during a long document, checksum race, rate limit, terminal failure, and downstream retry.
3. Assert successful content replacement is searchable in the fake Paperless index and failed OCR preserves native content.
4. Assert completed documents are not reprocessed after model/prompt-only changes.
5. Run `go test ./... -race`, acceptance tests, container tests, vulnerability scan, and `git diff --check`.
6. Commit with `test: add end-to-end acceptance coverage`.

### Task 21: Publish v0.1.0

**Files:**
- Create: `CHANGELOG.md`
- Modify: `README.md`

**Steps:**
1. Run the complete verification suite from a clean checkout.
2. Review the final image as linux/amd64 and linux/arm64 and record the manifest digest.
3. Confirm GHCR package visibility is public and anonymous pull succeeds.
4. Tag `v0.1.0`, push the tag, and verify CI, SBOM, provenance, and attestations.
5. Document the immutable image digest for homelab integration.
6. Commit release notes before tagging with `chore: prepare v0.1.0`.

### Task 22: Integrate With Homelab

**Files (homelab repository):**
- Modify: `stacks/paperless/compose.yaml`
- Modify: `stacks/paperless/defaults.env`
- Modify: `stacks/paperless/secrets.sops.env`
- Modify: `docs/runbooks/paperless.md`
- Modify: `tests/test-paperless-config.sh`
- Modify: relevant validation and Doco-CD tests

**Steps:**
1. Start a separate homelab feature worktree and load the repository-specific integration skill/workflow.
2. Write failing configuration tests for the pinned public image digest, private networks, hardened runtime, persistent data path, required variables, and scanner disablement.
3. Add SOPS variables for AI key, Paperless token, inbound webhook token, and downstream Paperless AI webhook key without committing plaintext.
4. Add `/srv/paperless/ai-ocr` with mode 0700 and include it in encrypted backup handling.
5. Configure the Paperless `Consumption Finished` workflow with bearer authentication to the internal service.
6. Set `PAPERLESS_AI_DISABLE_AUTOMATIC_PROCESSING=yes` and verify only OCR-success dispatch triggers metadata processing.
7. Deploy with concurrency one, verify new-document priority, then allow full archive backfill.
8. Verify a disposable success case, terminal failure preserving native OCR, restart resumption, search indexing, and no repeat metadata processing.
9. Run all homelab validation, commit in reviewable increments, obtain independent review, merge, and push only with explicit approval.

### Task 23: Runtime Burn-In And v1.0.0 Gate

**Files:**
- Modify: `CHANGELOG.md`
- Modify: `docs/operations.md`

**Steps:**
1. Observe the full archive backfill and capture aggregate success/failure, latency, retry, and cost metrics without document identifiers or content.
2. Test restore from the encrypted SQLite/data backup and resume an interrupted long document.
3. Confirm Paperless search uses corrected content and original PDFs remain retrievable.
4. Review failed documents manually for validation false positives and prompt weaknesses.
5. Fix release-blocking defects through the same TDD and review process.
6. Publish `v1.0.0` only after backfill and restore acceptance criteria pass.
