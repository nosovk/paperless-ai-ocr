#!/usr/bin/env bash
set -euo pipefail

for command in go pdfinfo pdftoppm; do
  if ! command -v "$command" >/dev/null 2>&1; then
    printf 'required command not found: %s\n' "$command" >&2
    exit 1
  fi
done

timeout 5m go test -tags=acceptance ./internal/acceptance -count=1 -v
