# Changelog

All notable changes to this project are documented in this file.

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
