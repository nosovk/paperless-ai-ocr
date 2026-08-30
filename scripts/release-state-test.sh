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

make_inspector "printf 'manifest unknown: not found\\n' >&2; exit 1"
run_check
assert_exact_tags_inspected
assert_output 'publish=true'
assert_output 'digest='

make_inspector "printf 'Digest: $digest_a\\n'"
run_check
assert_exact_tags_inspected
assert_output 'publish=false'
assert_output "digest=$digest_a"

make_inspector "if [[ \$1 == *:v1.2.3 ]]; then printf 'Digest: $digest_a\\n'; exit 0; fi; printf 'manifest unknown\\n' >&2; exit 1"
if run_check >"$work_dir/stdout" 2>"$work_dir/stderr"; then
  fail "accepted a partial release publication"
fi
grep -F 'only one immutable release tag exists' "$work_dir/stderr" >/dev/null || fail "partial publication error is not actionable"

make_inspector "if [[ \$1 == *:v1.2.3 ]]; then printf 'Digest: $digest_a\\n'; else printf 'Digest: $digest_b\\n'; fi"
if run_check >"$work_dir/stdout" 2>"$work_dir/stderr"; then
  fail "accepted immutable tags with different digests"
fi
grep -F 'immutable release tags resolve to different digests' "$work_dir/stderr" >/dev/null || fail "digest mismatch error is not actionable"

make_inspector "printf 'unauthorized: authentication required\\n' >&2; exit 1"
if run_check >"$work_dir/stdout" 2>"$work_dir/stderr"; then
  fail "treated an inspection error as a missing tag"
fi
grep -F 'could not determine release state' "$work_dir/stderr" >/dev/null || fail "inspection failure is not actionable"

make_inspector "printf 'Name: ghcr.io/example/project:v1.2.3\\n'"
if run_check >"$work_dir/stdout" 2>"$work_dir/stderr"; then
  fail "accepted an existing tag without an unambiguous digest"
fi
grep -F 'could not determine release state' "$work_dir/stderr" >/dev/null || fail "ambiguous inspection error is not actionable"

printf 'release state tests passed\n'
