#!/usr/bin/env bash
set -euo pipefail

: "${IMAGE:?IMAGE is required}"
: "${VERSION:?VERSION is required}"

readonly version=${VERSION#v}
readonly major_minor=${version%.*}
readonly work_dir=$(mktemp -d)
trap 'rm -rf "$work_dir"' EXIT

inspect() {
  if [[ -n ${INSPECT_COMMAND:-} ]]; then
    "$INSPECT_COMMAND" "$1"
    return
  fi
  docker buildx imagetools inspect "$1"
}

for tag in "$VERSION" "$version" "$major_minor"; do
  reference="$IMAGE:$tag"
  output="$work_dir/inspect-output"
  if inspect "$reference" >"$output" 2>&1; then
    digest=$(grep -Em1 '^(Digest: +)?sha256:[0-9a-f]{64}$' "$output" || true)
    printf 'immutable release tag %s already exists%s; refusing to overwrite it. If a prior publication was partial, recover the attestation manually for the existing digest.\n' \
      "$reference" "${digest:+ ($digest)}" >&2
    exit 1
  fi
  if ! grep -Eqi '(manifest unknown|not found)' "$output"; then
    printf 'could not verify immutable tag %s; refusing to publish:\n' "$reference" >&2
    cat "$output" >&2
    exit 1
  fi
done
