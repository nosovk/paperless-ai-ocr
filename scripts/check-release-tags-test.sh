#!/usr/bin/env bash
set -euo pipefail

readonly root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
readonly script="$root/scripts/check-release-tags.sh"
readonly work_dir=$(mktemp -d)
trap 'rm -rf "$work_dir"' EXIT

fail() {
  printf 'check-release-tags-test: %s\n' "$*" >&2
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
  INSPECT_LOG="$work_dir/inspect.log" INSPECT_COMMAND="$work_dir/inspect" \
    IMAGE=ghcr.io/example/project VERSION=v1.2.3 bash "$script"
}

make_inspector "printf 'manifest unknown: not found\\n' >&2; exit 1"
run_check
cat >"$work_dir/expected" <<'EOF'
ghcr.io/example/project:v1.2.3
ghcr.io/example/project:1.2.3
ghcr.io/example/project:1.2
EOF
cmp -s "$work_dir/expected" "$work_dir/inspect.log" || fail "did not inspect all immutable release tags"

: >"$work_dir/inspect.log"
make_inspector 'if [[ $1 == *:1.2.3 ]]; then printf '\''Digest: sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n'\''; exit 0; fi; printf '\''manifest unknown\n'\'' >&2; exit 1'
if run_check >"$work_dir/stdout" 2>"$work_dir/stderr"; then
  fail "accepted an existing immutable tag"
fi
grep -F 'ghcr.io/example/project:1.2.3 already exists' "$work_dir/stderr" >/dev/null || fail "existing tag error is not actionable"
grep -F 'sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa' "$work_dir/stderr" >/dev/null || fail "existing digest is not reported"

: >"$work_dir/inspect.log"
make_inspector "printf 'unauthorized: authentication required\\n' >&2; exit 1"
if run_check >"$work_dir/stdout" 2>"$work_dir/stderr"; then
  fail "treated an inspection error as a missing tag"
fi
grep -F 'could not verify immutable tag' "$work_dir/stderr" >/dev/null || fail "inspection failure is not actionable"

printf 'release tag immutability tests passed\n'
