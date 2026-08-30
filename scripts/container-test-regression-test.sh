#!/usr/bin/env bash
set -euo pipefail

readonly script=scripts/container-test.sh

failures=0

check() {
  local expected=$1
  local message=$2
  if ! grep -Fq "$expected" "$script"; then
    printf 'container-test-regression-test: %s\n' "$message" >&2
    failures=$((failures + 1))
  fi
}

check "trap 'handle_signal 130' INT" "SIGINT does not force status 130"
check "trap 'handle_signal 143' TERM" "SIGTERM does not force status 143"
check 'container test image already exists' "pre-existing image tags are not rejected"
check 'image_owned=true' "image cleanup does not track ownership"

for signal_test in INT:130 TERM:143; do
  signal_name=${signal_test%%:*}
  expected_status=${signal_test##*:}
  set +e
  CONTAINER_TEST_SELF_TEST_SIGNAL=$signal_name "$script" >/dev/null 2>&1
  actual_status=$?
  set -e
  if [[ "$actual_status" != "$expected_status" ]]; then
    printf 'container-test-regression-test: %s self-test exited %s, want %s\n' \
      "$signal_name" "$actual_status" "$expected_status" >&2
    failures=$((failures + 1))
  fi
done

((failures == 0)) || exit 1

printf 'container-test-regression-test: all checks passed\n'
