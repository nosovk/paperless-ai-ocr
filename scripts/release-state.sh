#!/usr/bin/env bash
set -euo pipefail

: "${IMAGE:?IMAGE is required}"
: "${VERSION:?VERSION is required}"
: "${GITHUB_OUTPUT:?GITHUB_OUTPUT is required}"

readonly version=${VERSION#v}
readonly work_dir=$(mktemp -d)
trap 'rm -rf "$work_dir"' EXIT

inspect() {
  if [[ -n ${INSPECT_COMMAND:-} ]]; then
    "$INSPECT_COMMAND" "$1"
    return
  fi
  docker buildx imagetools inspect "$1"
}

states=()
digests=()
for tag in "$VERSION" "$version"; do
  reference="$IMAGE:$tag"
  output="$work_dir/${tag//\//_}"
  if inspect "$reference" >"$output" 2>&1; then
    mapfile -t found < <(grep -E '^Digest:[[:space:]]+sha256:[0-9a-f]{64}$' "$output" | sed -E 's/^Digest:[[:space:]]+//' || true)
    if [[ ${#found[@]} -ne 1 ]]; then
      printf 'could not determine release state for %s: expected exactly one manifest digest\n' "$reference" >&2
      exit 1
    fi
    states+=(present)
    digests+=("${found[0]}")
    continue
  fi
  if grep -Eqi '(manifest unknown|not found)' "$output"; then
    states+=(missing)
    digests+=("")
    continue
  fi
  printf 'could not determine release state for %s:\n' "$reference" >&2
  cat "$output" >&2
  exit 1
done

if [[ ${states[0]} == missing && ${states[1]} == missing ]]; then
  printf 'publish=true\ndigest=\n' >>"$GITHUB_OUTPUT"
  exit 0
fi

if [[ ${states[0]} != "${states[1]}" ]]; then
  printf 'only one immutable release tag exists; refusing automatic recovery\n' >&2
  exit 1
fi

if [[ ${digests[0]} != "${digests[1]}" ]]; then
  printf 'immutable release tags resolve to different digests; refusing automatic recovery\n' >&2
  exit 1
fi

printf 'publish=false\ndigest=%s\n' "${digests[0]}" >>"$GITHUB_OUTPUT"
