#!/usr/bin/env bash
set -euo pipefail

readonly tag_pattern='^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$'

if [[ ! ${GITHUB_REF_NAME:-} =~ $tag_pattern ]]; then
  printf 'unsupported release tag: %s\n' "${GITHUB_REF_NAME:-}" >&2
  exit 1
fi

readonly head=$(git rev-parse HEAD)
if [[ ${GITHUB_SHA:-} != "$head" ]]; then
  printf 'tagged checkout mismatch: HEAD=%s GITHUB_SHA=%s\n' "$head" "${GITHUB_SHA:-}" >&2
  exit 1
fi

readonly committed_at=$(git show -s --format=%cI HEAD)
readonly created=$(date --utc --date="$committed_at" +'%Y-%m-%dT%H:%M:%SZ')

printf 'version=%s\ncreated=%s\n' "$GITHUB_REF_NAME" "$created" >> "$GITHUB_OUTPUT"
