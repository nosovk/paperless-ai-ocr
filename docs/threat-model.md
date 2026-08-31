# Threat Model

## Protected Assets

- Original PDFs and Poppler-rendered page images.
- Paperless native OCR and AI-generated transcriptions.
- Paperless, AI gateway, inbound webhook, and Paperless AI credentials.
- SQLite queue, checkpoints, diagnostics, and retained batch transcriptions.
- Integrity of Paperless content, tags, and downstream metadata dispatch.
- Published container images and CI release metadata.

## Trust Boundaries

### Inbound Webhook

Paperless crosses the network boundary with a bearer token and a document ID.
The handler limits the body to 4 KiB, requires one JSON content type and one
Authorization header, and accepts only a single positive `document_id` field.
The endpoint should remain on a private network.

### Paperless

The service trusts the configured Paperless API for document metadata, original
PDF bytes, tags, and content updates. A checksum is verified before processing
and before finalization, but a compromised Paperless instance controls source
data and API responses.

### AI Gateway

PDFs or rendered images and bounded native OCR cross this boundary. Provider
responses are untrusted and schema-validated, but schema-valid hallucination
remains possible. Provider retention, training, access, and jurisdiction are
operator decisions.

### Paperless AI

After successful replacement, the service sends a Paperless document URL and an
API key. Only HTTP `202` confirms success. Ambiguous outcomes are not retried
automatically to reduce duplicate effects.

### Poppler

Poppler parses attacker-controlled or malformed PDFs in a native process.
Timeouts, page limits, per-page validation, post-process temporary-output
accounting, and container hardening reduce exposure but do not remove native
memory-safety or resource-use risks. The workspace byte budget reserves and
validates accepted output after the Poppler process writes it; it is not an
active filesystem or process quota and cannot prevent peak writes. Bound `/tmp`
or its backing filesystem separately to impose a storage ceiling during render.

### Data And Temporary Storage

`/app/data` holds SQLite in WAL mode. `/tmp` holds per-job PDFs and images until
workspace cleanup. Both paths contain sensitive material and must be isolated
from other workloads. Backups must be consistent, access-controlled, and
encrypted.

### Metrics And Logs

Metrics use fixed labels and omit document IDs. Logs use allowlisted fields but
include document IDs for job events. Access to both surfaces should be limited;
document IDs can still reveal processing activity and may be sensitive in an
operator's environment.

### Container Runtime

The example deployment uses an unprivileged UID/GID, a read-only root
filesystem, dropped capabilities, `no-new-privileges`, private networking, and
bounded mode-0700 temporary storage. The operator remains responsible for host,
network, volume, secret, and egress controls.

### CI And GHCR

GitHub Actions dependencies and container base images are pinned. Release
workflows constrain permissions, build multi-architecture images, and publish
provenance. Repository administration, GitHub credentials, runner integrity,
and GHCR access remain trusted infrastructure.

## Primary Threats And Controls

| Threat | Controls and limits |
| --- | --- |
| Unauthorized enqueue | Dedicated bearer token, strict request shape, private ingress. |
| Credential disclosure | Environment-only injection, redirect rejection, safe error and log allowlists. |
| Malicious provider output | Response size bounds, exact schema and page validation, atomic finalization. |
| Stale overwrite | Paperless checksum checks before OCR and finalization. |
| Partial transcription | Durable batch checkpoints; content replacement only after complete validation. |
| Duplicate downstream effects | Durable finalization and dispatch reservation; ambiguous dispatch is not repeated. |
| PDF resource exhaustion | 10,000-page limit, render timeout, post-render 8 MiB page validation and byte accounting, plus a separately bounded temporary filesystem. |
| Data remanence | Temporary workspace cleanup; operator-enforced SQLite and backup retention. |
| Supply-chain replacement | SHA-pinned actions, digest-pinned base images, policy mutation tests, release attestations. |

## Residual Risks

- Schema-valid AI output can hallucinate, omit, reorder, or alter text.
- Poppler has native parser vulnerabilities and can consume significant CPU,
  memory, or temporary storage before output is inspected and accounted.
- Operators control HTTP endpoints. There is no destination IP denylist, so a
  malicious configuration can cause server-side requests to internal services.
- SQLite retains validated transcription indefinitely unless the operator
  applies an external retention process.
- Document IDs appear in production job logs.
- A compromised Paperless, AI gateway, Paperless AI service, container host, or
  CI administrator can bypass controls within its trust domain.

The original PDF remains in Paperless as the audit source. Successful AI OCR
overwrites native OCR in `document.content`; this service does not separately
retain native OCR. Operators who require it after success must export or back it
up before processing. A terminal OCR failure before successful content update
leaves native OCR unchanged. AI transcription is derived data and must not be
the sole basis for high-impact decisions.
