#!/usr/bin/env bash
set -euo pipefail

readonly root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
readonly script="$root/scripts/release-metadata.sh"

fail() {
  printf 'release-metadata-test: %s\n' "$*" >&2
  exit 1
}

run_metadata() {
  local tag=$1
  local output=$2
  GITHUB_REF_NAME="$tag" GITHUB_SHA=$(git -C "$root" rev-parse HEAD) GITHUB_OUTPUT="$output" bash "$script"
}

for tag in v0.1.0 v1.2.3 v10.20.30; do
  output=$(mktemp)
  trap 'rm -f "$output"' RETURN
  run_metadata "$tag" "$output"
  grep -Fx "version=$tag" "$output" >/dev/null || fail "missing version output for $tag"
  grep -Eq '^created=[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$' "$output" || fail "invalid RFC3339 output for $tag"
  rm -f "$output"
  trap - RETURN
done

for tag in v1 v1.2 v1.2.3-rc.1 v01.2.3 v1.02.3 v1.2.03 latest; do
  output=$(mktemp)
  if run_metadata "$tag" "$output" >/dev/null 2>&1; then
    rm -f "$output"
    fail "accepted invalid tag: $tag"
  fi
  rm -f "$output"
done

mismatch_output=$(mktemp)
if GITHUB_REF_NAME=v1.2.3 GITHUB_SHA=0000000000000000000000000000000000000000 \
  GITHUB_OUTPUT="$mismatch_output" bash "$script" >"$mismatch_output.stdout" 2>"$mismatch_output.stderr"; then
  fail "accepted a checkout that does not match GITHUB_SHA"
fi
grep -F 'tagged checkout mismatch' "$mismatch_output.stderr" >/dev/null || fail "checkout mismatch error is missing"
rm -f "$mismatch_output" "$mismatch_output.stdout" "$mismatch_output.stderr"

first=$(mktemp)
second=$(mktemp)
trap 'rm -f "$first" "$second"' EXIT
run_metadata v0.1.0 "$first"
sleep 1
run_metadata v0.1.0 "$second"
cmp -s "$first" "$second" || fail "metadata output changed between identical runs"

expected=$(git -C "$root" show -s --format=%cI HEAD | xargs -I{} date --utc --date={} +'%Y-%m-%dT%H:%M:%SZ')
grep -Fx "created=$expected" "$first" >/dev/null || fail "CREATED does not match commit timestamp"

printf 'release metadata tests passed\n'
