# Contributing

## Prerequisites

- Go 1.26.6, matching `go.mod`.
- Poppler command-line utilities for PDF tests.
- Docker with Buildx for container work.
- Python 3 for workflow policy and mutation tests.
- Node.js 24.8.0 for reproducible documentation checks matching CI.

Do not commit credentials, real documents, OCR, transcriptions, database files,
provider responses, or sensitive logs. Use only synthetic fixtures.

## Development Checks

Run formatting, static analysis, unit tests, race tests, and vulnerability
analysis:

```sh
gofmt -w .
go vet ./...
go test ./...
go test -race ./...
go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.13.2 run
go run golang.org/x/vuln/cmd/govulncheck@v1.7.0 ./...
```

Run workflow and policy checks:

```sh
go run github.com/rhysd/actionlint/cmd/actionlint@v1.7.12 \
  .github/workflows/ci.yml .github/workflows/release.yml
python3 scripts/workflow-policy-test.py
python3 scripts/workflow-policy-regression-test.py
bash scripts/release-metadata-test.sh
bash scripts/release-state-test.sh
```

Run the same documentation checks as CI:

```sh
npm ci --ignore-scripts --no-audit --no-fund
npm exec --no -- markdownlint-cli2 "**/*.md"
npm exec --no -- markdown-link-check --config .markdown-link-check.json \
  --quiet README.md SECURITY.md CONTRIBUTING.md docs/configuration.md \
  docs/architecture.md docs/operations.md docs/threat-model.md
```

The markdownlint policy disables only line length because prose, links, and
tables contain intentionally long lines, and permits duplicate headings only in
different sections because several reference pages repeat local subsection
names. The link checker ignores only the authenticated GitHub security-advisory
creation URL.

Run `git diff --check` before requesting review. Container tests are required
when Dockerfile, image content, entrypoint, healthcheck, or container behavior
changes.

## Tests And Changes

- Use test-driven development for behavior and workflow-policy changes. Record
  the expected failing test before implementation.
- Keep changes minimal and avoid unrelated refactoring.
- Add or update mutation tests when a fail-closed workflow rule is introduced.
- Keep runtime claims in documentation tied to current code and tests.
- Use ASCII unless an existing file or test specifically requires other text.

## Security-Sensitive Changes

Changes involving authentication, secrets, outbound URLs, PDF parsing, model
responses, logging, diagnostics, SQLite state, finalization, release workflows,
or container privileges require explicit adversarial review.

Do not weaken request bounds, response validation, checksum checks, lease
fencing, dispatch reservation, safe logging, action SHA pins, image digest pins,
or least-privilege workflow permissions without a documented security reason and
regression tests.

Potential vulnerabilities must follow [SECURITY.md](SECURITY.md), not a public
issue or pull request.
