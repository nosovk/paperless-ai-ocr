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
readonly digest_c=sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc
readonly digest_d=sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd
readonly digest_e=sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee
readonly revision=0123456789abcdef0123456789abcdef01234567

image_json() {
  local digest=$1
  local version=${2:-1.2.3}
  local image_revision=${3:-$revision}
  local platforms=${4:-both}
  local amd64=''
  local arm64=''
  local attestations=''
  if [[ $platforms == both || $platforms == amd64 ]]; then
    amd64=$(printf '{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"%s","size":1234,"annotations":{"org.opencontainers.image.version":"%s","org.opencontainers.image.revision":"%s"},"platform":{"os":"linux","architecture":"amd64"}}' "$digest_a" "$version" "$image_revision")
  fi
  if [[ $platforms == both || $platforms == arm64 ]]; then
    arm64=$(printf '{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"%s","size":1234,"annotations":{"org.opencontainers.image.version":"%s","org.opencontainers.image.revision":"%s"},"platform":{"os":"linux","architecture":"arm64"}}' "$digest_b" "$version" "$image_revision")
  fi
  if [[ $platforms == both ]]; then
    attestations=$(printf ',{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"%s","size":1234,"annotations":{"vnd.docker.reference.type":"attestation-manifest","vnd.docker.reference.digest":"%s"},"platform":{"os":"unknown","architecture":"unknown"}},{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"%s","size":1234,"annotations":{"vnd.docker.reference.type":"attestation-manifest","vnd.docker.reference.digest":"%s"},"platform":{"os":"unknown","architecture":"unknown"}}' "$digest_d" "$digest_a" "$digest_e" "$digest_b")
  fi
  printf '{"manifest":{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","digest":"%s","size":4321,"manifests":[%s%s%s%s]}}' \
    "$digest" "$amd64" "${amd64:+${arm64:+,}}" "$arm64" "$attestations"
}

assert_rejected() {
  local json=$1
  local expected=$2
  local description=$3
  make_inspector "printf '%s\\n' '$json'"
  if run_check >"$work_dir/stdout" 2>"$work_dir/stderr"; then
    fail "accepted $description"
  fi
  grep -F "$expected" "$work_dir/stderr" >/dev/null || fail "$description error is not actionable"
}

make_inspector "printf 'manifest unknown: not found\\n' >&2; exit 1"
run_check
assert_exact_tags_inspected
assert_output 'publish=true'
assert_output 'digest='

readonly valid_image=$(image_json "$digest_c")

make_inspector "printf '%s\\n' '$valid_image'"
run_check
assert_exact_tags_inspected
assert_output 'publish=false'
assert_output "digest=$digest_c"

make_inspector "if [[ \$1 == *:v1.2.3 ]]; then printf '%s\\n' '$valid_image'; exit 0; fi; printf 'manifest unknown\\n' >&2; exit 1"
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

make_inspector "printf 'ERROR: unauthorized: manifest unknown: authentication required\\n' >&2; exit 1"
if run_check >"$work_dir/stdout" 2>"$work_dir/stderr"; then
  fail "treated a mixed authorization error as a missing tag"
fi
grep -F 'could not determine release state' "$work_dir/stderr" >/dev/null || fail "mixed inspection failure is not actionable"

make_inspector "printf 'manifest unknown: not found\\nunauthorized: authentication required\\n' >&2; exit 1"
if run_check >"$work_dir/stdout" 2>"$work_dir/stderr"; then
  fail "treated ambiguous multiline output as a missing tag"
fi
grep -F 'could not determine release state' "$work_dir/stderr" >/dev/null || fail "multiline inspection failure is not actionable"

make_inspector "printf 'manifest unknown: authentication required\\n' >&2; exit 1"
if run_check >"$work_dir/stdout" 2>"$work_dir/stderr"; then
  fail "treated an ambiguous manifest-unknown error as a missing tag"
fi
grep -F 'could not determine release state' "$work_dir/stderr" >/dev/null || fail "ambiguous manifest-unknown failure is not actionable"

make_inspector "printf 'ERROR: ghcr.io/example/project:v1.2.3: not found\\nunauthorized: authentication required\\n' >&2; exit 1"
if run_check >"$work_dir/stdout" 2>"$work_dir/stderr"; then
  fail "treated a multiline exact-reference error as a missing tag"
fi
grep -F 'could not determine release state' "$work_dir/stderr" >/dev/null || fail "multiline exact-reference failure is not actionable"

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

assert_rejected "${valid_image/\"schemaVersion\":2/\"schemaVersion\":1}" 'invalid image index' 'an invalid OCI schema version'
assert_rejected "${valid_image/application\/vnd.oci.image.index.v1+json/application\/vnd.oci.image.manifest.v1+json}" 'invalid image index' 'a non-index root media type'
assert_rejected "${valid_image/application\/vnd.oci.image.manifest.v1+json/application\/vnd.oci.image.layer.v1.tar+gzip}" 'invalid release descriptor' 'a non-image runnable descriptor'
assert_rejected "${valid_image/$digest_a/sha256:ABC}" 'invalid release descriptor' 'a malformed runnable descriptor digest'
assert_rejected "${valid_image/\"size\":4321,/}" 'invalid image index' 'an image index without size'
assert_rejected "${valid_image/\"size\":4321/\"size\":0}" 'invalid image index' 'an image index with zero size'
assert_rejected "${valid_image/\"size\":4321/\"size\":9223372036854775808}" 'invalid image index' 'an image index with overflowing size'
assert_rejected "${valid_image/\"size\":1234,/}" 'invalid release descriptor' 'a runnable descriptor without size'
assert_rejected "${valid_image/\"size\":1234/\"size\":0}" 'invalid release descriptor' 'a runnable descriptor with zero size'
assert_rejected "${valid_image/\"size\":1234/\"size\":\"1234\"}" 'invalid release descriptor' 'a runnable descriptor with string size'
assert_rejected "${valid_image/\"size\":1234/\"size\":-1}" 'invalid release descriptor' 'a runnable descriptor with negative size'
assert_rejected "${valid_image/\"size\":1234/\"size\":true}" 'invalid release descriptor' 'a runnable descriptor with boolean size'
assert_rejected "${valid_image/\"size\":1234/\"size\":9223372036854775808}" 'invalid release descriptor' 'a runnable descriptor with overflowing size'
assert_rejected "${valid_image/\"vnd.docker.reference.type\":\"attestation-manifest\"/\"vnd.docker.reference.type\":\"other\"}" 'invalid attestation descriptor' 'an unknown descriptor without the BuildKit attestation type'
readonly docker_attestation=$(python3 -c 'import json,sys; value=json.loads(sys.argv[1]); next(item for item in value["manifest"]["manifests"] if item["digest"] == sys.argv[2])["mediaType"]="application/vnd.docker.distribution.manifest.v2+json"; print(json.dumps(value, separators=(",", ":")))' "$valid_image" "$digest_d")
assert_rejected "$docker_attestation" 'invalid attestation descriptor' 'a Docker media type for an attestation descriptor'
assert_rejected "${valid_image/\"vnd.docker.reference.digest\":\"$digest_a\"/\"vnd.docker.reference.digest\":\"$digest_d\"}" 'invalid attestation descriptor' 'an attestation descriptor not bound to a runnable manifest'
assert_rejected "${valid_image/\"vnd.docker.reference.digest\":\"$digest_b\"/\"vnd.docker.reference.digest\":\"$digest_a\"}" 'invalid attestation descriptor' 'duplicate attestation references'
assert_rejected "${valid_image/$digest_e/$digest_d}" 'duplicate descriptor digest' 'duplicate attestation descriptor digests'
assert_rejected "${valid_image/\"architecture\":\"arm64\"/\"architecture\":\"amd64\"}" 'does not contain the required release platforms' 'duplicate amd64 release descriptors'
assert_rejected "${valid_image/\"architecture\":\"arm64\"/\"architecture\":\"arm64\",\"variant\":\"v8\"}" 'invalid release descriptor' 'an arm64 descriptor with a platform variant'
assert_rejected "${valid_image/\"architecture\":\"amd64\"/\"architecture\":\"amd64\",\"os.version\":\"1\"}" 'invalid release descriptor' 'an amd64 descriptor with an OS version'
readonly no_attestations=$(python3 -c 'import json,sys; value=json.loads(sys.argv[1]); value["manifest"]["manifests"]=[item for item in value["manifest"]["manifests"] if item["platform"]["os"] != "unknown"]; print(json.dumps(value, separators=(",", ":")))' "$valid_image")
assert_rejected "$no_attestations" 'invalid attestation descriptor' 'an index without BuildKit attestation descriptors'
readonly one_attestation=$(python3 -c 'import json,sys; value=json.loads(sys.argv[1]); value["manifest"]["manifests"]=[item for item in value["manifest"]["manifests"] if item["digest"] != sys.argv[2]]; print(json.dumps(value, separators=(",", ":")))' "$valid_image" "$digest_e")
assert_rejected "$one_attestation" 'invalid attestation descriptor' 'incomplete attestation descriptor coverage'

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
