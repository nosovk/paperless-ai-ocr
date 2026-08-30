#!/usr/bin/env bash
set -euo pipefail

: "${IMAGE:?IMAGE is required}"
: "${VERSION:?VERSION is required}"
: "${REVISION:?REVISION is required}"
: "${GITHUB_OUTPUT:?GITHUB_OUTPUT is required}"

readonly version=${VERSION#v}
readonly work_dir=$(mktemp -d)
trap 'rm -rf "$work_dir"' EXIT

inspect() {
  if [[ -n ${INSPECT_COMMAND:-} ]]; then
    "$INSPECT_COMMAND" "$1"
    return
  fi
  docker buildx imagetools inspect --format '{{json .}}' "$1"
}

validate_image() {
  python3 - "$1" "$VERSION" "$REVISION" "$2" <<'PY'
import json
import re
import sys


path, version, revision, reference = sys.argv[1:]
try:
    document = json.loads(open(path, encoding="utf-8").read())
except (OSError, json.JSONDecodeError) as err:
    raise SystemExit(f"could not determine release state for {reference}: invalid inspection JSON: {err}")

manifest = document.get("manifest")
digest = manifest.get("digest") if isinstance(manifest, dict) else None
if not isinstance(digest, str) or re.fullmatch(r"sha256:[0-9a-f]{64}", digest) is None:
    raise SystemExit(f"could not determine release state for {reference}: expected exactly one manifest digest")

platforms = {}
for descriptor in manifest.get("manifests", []):
    platform = descriptor.get("platform", {})
    key = (platform.get("os"), platform.get("architecture"))
    if key not in {("linux", "amd64"), ("linux", "arm64")}:
        if key != ("unknown", "unknown"):
            raise SystemExit(f"existing image {reference} does not contain the required release platforms")
        continue
    if key in platforms:
        raise SystemExit(f"existing image {reference} does not contain the required release platforms")
    platforms[key] = descriptor.get("annotations", {})

if set(platforms) != {("linux", "amd64"), ("linux", "arm64")}:
    raise SystemExit(f"existing image {reference} does not contain the required release platforms")
for annotations in platforms.values():
    if annotations.get("org.opencontainers.image.version") != version:
        raise SystemExit(f"existing image {reference} does not match release version {version}")
    if annotations.get("org.opencontainers.image.revision") != revision:
        raise SystemExit(f"existing image {reference} does not match the tagged commit {revision}")

print(digest)
PY
}

states=()
digests=()
for tag in "$VERSION" "$version"; do
  reference="$IMAGE:$tag"
  output="$work_dir/${tag//\//_}"
  if inspect "$reference" >"$output" 2>&1; then
    digest=$(validate_image "$output" "$reference")
    states+=(present)
    digests+=("$digest")
    continue
  fi
  if grep -Eqi '(^|: )manifest unknown([: ]|$)' "$output" \
    || grep -Fxi "ERROR: $reference: not found" "$output" >/dev/null; then
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
