# Changelog

All notable changes to this project are documented in this file.

## [0.1.5] - 2026-09-01

### Fixed

- Fall back from direct PDF input to page-image checkpoints after provider
  request timeouts, while preserving the direct fast path for successful PDFs.

## [0.1.4] - 2026-09-01

### Fixed

- Read the source checksum from the unique root entry in Paperless 3.1 document
  versions when the legacy top-level checksum field is absent.

## [0.1.3] - 2026-09-01

### Fixed

- Add horizontal padding to the synthetic capability-probe fixtures so the
  complete visual nonce is available to multimodal models.

## [0.1.2] - 2026-09-01

### Fixed

- Use the bounded authenticated documents endpoint for the Paperless startup
  probe, avoiding the Paperless 3.1 API root redirect.

## [0.1.1] - 2026-09-01

### Added

- Optional `PAPERLESS_AI_WEBHOOK_TIMEOUT` configuration for long-running
  downstream Paperless AI requests, retaining the existing 30-second default.

## [0.1.0] - 2026-08-31

First stable release.

### Added

- Durable single-worker SQLite queue with webhook priority, archive backfill,
  lease recovery, and resumable page-batch checkpoints.
- Paperless-ngx webhook ingestion, periodic reconciliation, source checksum
  validation, content replacement, completion and failure tags, and downstream
  Paperless AI dispatch.
- Direct PDF and Poppler-rendered image transports for vision-capable AI
  gateways, with startup capability detection and complete page-oriented
  response validation.
- Prometheus-compatible operational metrics, structured allowlisted logging,
  health and dependency-aware readiness endpoints.
- Hardened non-root container image for `linux/amd64` and `linux/arm64`, with a
  read-only root filesystem baseline and writable SQLite data volume.
- Tag-triggered release publication with immutable version tags, verified
  multi-architecture image state, SBOMs, BuildKit provenance, and GitHub
  provenance attestations.
- End-to-end acceptance coverage using the real application composition,
  SQLite, HTTP clients, and Poppler with fake external services.

### Security

- Credentials are accepted only through validated environment variables and
  are excluded from allowlisted logs and metrics.
- Model output is treated as untrusted data and must pass strict structural,
  page-order, size, and completeness validation before Paperless content is
  changed.
- Container builds use pinned toolchains, actions, and base-image digests. Local
  container regression tests check the runtime binary's embedded version,
  revision, and build time.

### Operational Notes

- Successful AI OCR replaces Paperless native OCR in `document.content`. The
  original PDF remains the audit source; export native OCR before processing if
  it must be retained separately.
- LLM transcription can omit, invent, or alter text despite passing structural
  validation. It must not be treated as authoritative evidence.
- Only Paperless AI HTTP `503` responses are retried automatically after the
  durable retry delay. This is an operator-selected assumption and can duplicate
  metadata processing if Paperless AI applied the request before returning
  `503`.
- The service intentionally supports one active document and one active model
  request. Do not run multiple replicas against the same operational queue.
- Production deployments should pin the published GHCR manifest digest rather
  than either stable tag.
