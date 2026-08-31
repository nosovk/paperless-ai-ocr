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
  python3 - "$1" "$version" "$REVISION" "$2" <<'PY'
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
root_size = manifest.get("size")
if manifest.get("schemaVersion") != 2 or manifest.get("mediaType") not in {
    "application/vnd.oci.image.index.v1+json",
    "application/vnd.docker.distribution.manifest.list.v2+json",
} or not isinstance(root_size, int) or isinstance(root_size, bool) or not 0 < root_size <= 2**63 - 1:
    raise SystemExit(f"existing image {reference} has an invalid image index")

descriptors = manifest.get("manifests")
if not isinstance(descriptors, list):
    raise SystemExit(f"existing image {reference} has an invalid image index")

platforms = {}
attestation_references = set()
descriptor_digests = set()
manifest_media_types = {
    "application/vnd.oci.image.manifest.v1+json",
    "application/vnd.docker.distribution.manifest.v2+json",
}
for descriptor in descriptors:
    if not isinstance(descriptor, dict):
        raise SystemExit(f"existing image {reference} has an invalid release descriptor")
    platform = descriptor.get("platform", {})
    if not isinstance(platform, dict):
        raise SystemExit(f"existing image {reference} has an invalid release descriptor")
    key = (platform.get("os"), platform.get("architecture"))
    descriptor_digest = descriptor.get("digest")
    media_type = descriptor.get("mediaType")
    size = descriptor.get("size")
    if media_type not in manifest_media_types or not isinstance(descriptor_digest, str) or re.fullmatch(
        r"sha256:[0-9a-f]{64}", descriptor_digest
    ) is None or not isinstance(size, int) or isinstance(size, bool) or not 0 < size <= 2**63 - 1:
        if key == ("unknown", "unknown"):
            raise SystemExit(f"existing image {reference} has an invalid attestation descriptor")
        raise SystemExit(f"existing image {reference} has an invalid release descriptor")
    if descriptor_digest in descriptor_digests:
        raise SystemExit(f"existing image {reference} has a duplicate descriptor digest")
    descriptor_digests.add(descriptor_digest)
    annotations = descriptor.get("annotations")
    if not isinstance(annotations, dict):
        if key == ("unknown", "unknown"):
            raise SystemExit(f"existing image {reference} has an invalid attestation descriptor")
        raise SystemExit(f"existing image {reference} has an invalid release descriptor")
    if key == ("unknown", "unknown"):
        attestation_reference = annotations.get("vnd.docker.reference.digest")
        if (
            media_type != "application/vnd.oci.image.manifest.v1+json"
            or
            annotations.get("vnd.docker.reference.type") != "attestation-manifest"
            or not isinstance(attestation_reference, str)
            or re.fullmatch(r"sha256:[0-9a-f]{64}", attestation_reference) is None
            or attestation_reference in attestation_references
        ):
            raise SystemExit(f"existing image {reference} has an invalid attestation descriptor")
        attestation_references.add(attestation_reference)
        continue
    if key not in {("linux", "amd64"), ("linux", "arm64")}:
        raise SystemExit(f"existing image {reference} does not contain the required release platforms")
    if key in platforms:
        raise SystemExit(f"existing image {reference} does not contain the required release platforms")
    platforms[key] = (descriptor_digest, annotations)

if set(platforms) != {("linux", "amd64"), ("linux", "arm64")}:
    raise SystemExit(f"existing image {reference} does not contain the required release platforms")
platform_digests = {descriptor_digest for descriptor_digest, _ in platforms.values()}
if attestation_references != platform_digests:
    raise SystemExit(f"existing image {reference} has an invalid attestation descriptor")
for _, annotations in platforms.values():
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
  if grep -Exi 'manifest unknown([: ].*)?' "$output" >/dev/null \
    && [[ $(wc -l <"$output") -eq 1 ]] \
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
