#!/usr/bin/env bash
set -euo pipefail

readonly root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
readonly script="$root/scripts/release-state.sh"
readonly work_dir=$(mktemp -d)
trap 'rm -rf "$work_dir"' EXIT

fail() {
  printf 'release-state-test: %s\n' "$*" >&2
  exit 1
}

make_inspector() {
  local body=$1
  cat >"$work_dir/inspect" <<EOF
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "\$1" >> "\$INSPECT_LOG"
$body
EOF
  chmod +x "$work_dir/inspect"
}

run_check() {
  : >"$work_dir/github-output"
  : >"$work_dir/inspect.log"
  GITHUB_OUTPUT="$work_dir/github-output" \
    INSPECT_LOG="$work_dir/inspect.log" \
    INSPECT_COMMAND="$work_dir/inspect" \
    IMAGE=ghcr.io/example/project \
    VERSION=v1.2.3 \
    REVISION=0123456789abcdef0123456789abcdef01234567 \
    bash "$script"
}

assert_output() {
  local expected=$1
  grep -Fx "$expected" "$work_dir/github-output" >/dev/null || fail "missing output: $expected"
}

assert_exact_tags_inspected() {
  cat >"$work_dir/expected" <<'EOF'
ghcr.io/example/project:v1.2.3
ghcr.io/example/project:1.2.3
EOF
  cmp -s "$work_dir/expected" "$work_dir/inspect.log" || fail "did not inspect exactly both immutable release tags"
}

readonly digest_a=sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
readonly digest_b=sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
readonly revision=0123456789abcdef0123456789abcdef01234567

image_json() {
  local digest=$1
  local version=${2:-1.2.3}
  local image_revision=${3:-$revision}
  local platforms=${4:-both}
  local amd64=''
  local arm64=''
  if [[ $platforms == both || $platforms == amd64 ]]; then
    amd64=$(printf '{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"%s","annotations":{"org.opencontainers.image.version":"%s","org.opencontainers.image.revision":"%s"},"platform":{"os":"linux","architecture":"amd64"}}' "$digest_a" "$version" "$image_revision")
  fi
  if [[ $platforms == both || $platforms == arm64 ]]; then
    arm64=$(printf '{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"%s","annotations":{"org.opencontainers.image.version":"%s","org.opencontainers.image.revision":"%s"},"platform":{"os":"linux","architecture":"arm64"}}' "$digest_b" "$version" "$image_revision")
  fi
  printf '{"manifest":{"digest":"%s","manifests":[%s%s%s]}}' \
    "$digest" "$amd64" "${amd64:+${arm64:+,}}" "$arm64"
}

make_inspector "printf 'manifest unknown: not found\\n' >&2; exit 1"
run_check
assert_exact_tags_inspected
assert_output 'publish=true'
assert_output 'digest='

make_inspector "printf '%s\\n' '$(image_json "$digest_a")'"
run_check
assert_exact_tags_inspected
assert_output 'publish=false'
assert_output "digest=$digest_a"

make_inspector "if [[ \$1 == *:v1.2.3 ]]; then printf '%s\\n' '$(image_json "$digest_a")'; exit 0; fi; printf 'manifest unknown\\n' >&2; exit 1"
if run_check >"$work_dir/stdout" 2>"$work_dir/stderr"; then
  fail "accepted a partial release publication"
fi
grep -F 'only one immutable release tag exists' "$work_dir/stderr" >/dev/null || fail "partial publication error is not actionable"

make_inspector "if [[ \$1 == *:v1.2.3 ]]; then printf '%s\\n' '$(image_json "$digest_a")'; else printf '%s\\n' '$(image_json "$digest_b")'; fi"
if run_check >"$work_dir/stdout" 2>"$work_dir/stderr"; then
  fail "accepted immutable tags with different digests"
fi
grep -F 'immutable release tags resolve to different digests' "$work_dir/stderr" >/dev/null || fail "digest mismatch error is not actionable"

make_inspector "printf 'unauthorized: authentication required\\n' >&2; exit 1"
if run_check >"$work_dir/stdout" 2>"$work_dir/stderr"; then
  fail "treated an inspection error as a missing tag"
fi
grep -F 'could not determine release state' "$work_dir/stderr" >/dev/null || fail "inspection failure is not actionable"

make_inspector "printf 'error: authorization helper not found\\n' >&2; exit 1"
if run_check >"$work_dir/stdout" 2>"$work_dir/stderr"; then
  fail "treated an unrelated not-found error as a missing tag"
fi
grep -F 'could not determine release state' "$work_dir/stderr" >/dev/null || fail "not-found inspection failure is not actionable"

make_inspector "printf '{\"manifest\":{}}\\n'"
if run_check >"$work_dir/stdout" 2>"$work_dir/stderr"; then
  fail "accepted an existing tag without an unambiguous digest"
fi
grep -F 'could not determine release state' "$work_dir/stderr" >/dev/null || fail "ambiguous inspection error is not actionable"

make_inspector "printf '%s\\n' '$(image_json "$digest_a" 1.2.3 deadbeefdeadbeefdeadbeefdeadbeefdeadbeef)'"
if run_check >"$work_dir/stdout" 2>"$work_dir/stderr"; then
  fail "accepted an existing image built from a different revision"
fi
grep -F 'does not match the tagged commit' "$work_dir/stderr" >/dev/null || fail "revision mismatch error is not actionable"

make_inspector "printf '%s\\n' '$(image_json "$digest_a" 9.9.9)'"
if run_check >"$work_dir/stdout" 2>"$work_dir/stderr"; then
  fail "accepted an existing image with a different version"
fi
grep -F 'does not match release version' "$work_dir/stderr" >/dev/null || fail "version mismatch error is not actionable"

make_inspector "printf '%s\\n' '$(image_json "$digest_a" 1.2.3 "$revision" amd64)'"
if run_check >"$work_dir/stdout" 2>"$work_dir/stderr"; then
  fail "accepted an existing image without both release platforms"
fi
grep -F 'does not contain the required release platforms' "$work_dir/stderr" >/dev/null || fail "platform mismatch error is not actionable"

printf 'release state tests passed\n'
