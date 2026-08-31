# Security Policy

## Reporting A Vulnerability

Report vulnerabilities privately through
[GitHub Private Vulnerability Reporting](https://github.com/nosovk/paperless-ai-ocr/security/advisories/new).
Do not open a public issue first.

Include the affected version or revision, impact, prerequisites, a minimal
synthetic reproduction, and suggested mitigations if known. Maintainers will use
the private advisory to coordinate validation, remediation, and disclosure.

## Sensitive Material Is Prohibited

Do not include any of the following in a vulnerability report, public issue,
pull request, chat, or email:

- real PDFs or document images;
- real OCR text or AI transcriptions;
- real filenames or document titles;
- SQLite databases, WAL files, or backups;
- environment dumps or container inspection output containing environment;
- API tokens, webhook keys, bearer credentials, cookies, or authorization
  headers;
- raw provider responses or raw HTTP request and response bodies;
- logs containing credentials, document content, OCR, transcriptions, provider
  responses, or other sensitive data.

Use synthetic documents, synthetic identifiers, redacted safe categories, and
minimal code that does not contain production data. If exposure has already
occurred, rotate affected credentials and follow the relevant provider's
incident process before sharing a sanitized report.

## Supported Versions

Until versioned releases exist, security fixes apply to the current default
development line. After releases begin, supported versions will be listed here.

## Scope Notes

The service processes untrusted PDFs and untrusted AI responses and stores
validated transcriptions in SQLite. Review the deployment assumptions and
residual risks in the [Threat model](docs/threat-model.md).
