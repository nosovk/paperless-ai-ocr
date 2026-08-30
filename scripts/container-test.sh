#!/usr/bin/env bash
set -euo pipefail

readonly image="${CONTAINER_TEST_IMAGE:-paperless-ai-ocr:container-test-$$-$RANDOM}"
readonly platform="linux/amd64"
readonly test_support_image="python:3.14.2-alpine3.23@sha256:31da4cb527055e4e3d7e9e006dffe9329f84ebea79eaca0a1f1c27ce61e40ca5"

work_dir=""
fake_container_id=""
container_id=""
builder_name=""
network_name=""
data_volume=""
image_owned=false

fail() {
  printf 'container-test: %s\n' "$*" >&2
  exit 1
}

cleanup() {
  local status=$?
  trap - EXIT INT TERM
  if [[ -n "$container_id" ]]; then
    docker rm -f "$container_id" >/dev/null 2>&1 || true
  fi
  if [[ -n "$fake_container_id" ]]; then
    docker rm -f "$fake_container_id" >/dev/null 2>&1 || true
  fi
  if [[ -n "$builder_name" ]]; then
    docker buildx rm -f "$builder_name" >/dev/null 2>&1 || true
  fi
  if [[ -n "$network_name" ]]; then
    docker network rm "$network_name" >/dev/null 2>&1 || true
  fi
  if [[ -n "$data_volume" ]]; then
    docker volume rm -f "$data_volume" >/dev/null 2>&1 || true
  fi
  if [[ -n "$work_dir" ]]; then
    rm -rf "$work_dir"
  fi
  if [[ "$image_owned" == true ]]; then
    docker image rm -f "$image" >/dev/null 2>&1 || true
  fi
  exit "$status"
}

handle_signal() {
  trap - INT TERM
  exit "$1"
}

trap cleanup EXIT
trap 'handle_signal 130' INT
trap 'handle_signal 143' TERM

if [[ -n "${CONTAINER_TEST_SELF_TEST_SIGNAL:-}" ]]; then
  kill -s "$CONTAINER_TEST_SELF_TEST_SIGNAL" "$$"
  fail "signal self-test did not terminate"
fi

command -v docker >/dev/null || fail "docker is required"
docker info >/dev/null || fail "docker daemon is unavailable"
docker buildx version >/dev/null || fail "docker buildx is required"
[[ -f Dockerfile ]] || fail "Dockerfile is missing"
if docker image inspect "$image" >/dev/null 2>&1; then
  fail "container test image already exists: $image"
fi
image_owned=true

work_dir=$(mktemp -d)

cat >"$work_dir/fake-services.py" <<'PY'
import http.server
import json
import sys


class Handler(http.server.BaseHTTPRequestHandler):
    def log_message(self, *_args):
        pass

    def do_GET(self):
        print(f"GET {self.path}", flush=True)
        if self.path == "/api/":
            self.send_response(200)
            self.end_headers()
            return
        if self.path == "/api/documents/":
            body = b'{"count":0,"next":null,"results":[]}'
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
            return
        self.send_response(404)
        self.end_headers()

    def do_POST(self):
        print(f"POST {self.path}", flush=True)
        length = int(self.headers.get("Content-Length", "0"))
        self.rfile.read(length)
        if self.path == "/v1/responses":
            body = json.dumps({"output": [{"type": "message", "content": [{"type": "output_text", "text": "OCR-PROBE-7K3M9Q2X", "refusal": ""}]}]}).encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
            return
        self.send_response(204)
        self.end_headers()


http.server.ThreadingHTTPServer(("0.0.0.0", int(sys.argv[1])), Handler).serve_forever()
PY

readonly fake_port=8081
network_name="paperless-ai-ocr-test-$RANDOM-$$"
docker network create "$network_name" >/dev/null
data_volume="paperless-ai-ocr-data-test-$RANDOM-$$"
docker volume create "$data_volume" >/dev/null
docker run --rm \
  --mount type=volume,src="$data_volume",dst=/data \
  alpine:3.23.3@sha256:25109184c71bdad752c8312a8623239686a9a2071e8825f20acb8f2198c3f659 \
  sh -ec 'chown 65532:65532 /data; chmod 0700 /data'
fake_container_id=$(docker run -d \
  --network "$network_name" \
  --network-alias fake-services \
  --read-only \
  --tmpfs /tmp:size=16m,mode=0700 \
  --mount type=bind,src="$work_dir/fake-services.py",dst=/fake-services.py,readonly \
  "$test_support_image" \
  python /fake-services.py "$fake_port")

fake_deadline=$((SECONDS + 30))
while ((SECONDS < fake_deadline)); do
  if docker run --rm \
    --network "$network_name" \
    -e NO_PROXY=fake-services \
    -e no_proxy=fake-services \
    "$test_support_image" \
    python -c "import urllib.request; urllib.request.urlopen('http://fake-services:$fake_port/api/', timeout=2).read()" \
    >/dev/null 2>&1; then
    break
  fi
  if [[ $(docker inspect --format '{{.State.Running}}' "$fake_container_id") != true ]]; then
    docker logs "$fake_container_id" >&2
    fail "fake dependency server exited before becoming ready"
  fi
  sleep 1
done
if ! docker run --rm \
  --network "$network_name" \
  -e NO_PROXY=fake-services \
  -e no_proxy=fake-services \
  "$test_support_image" \
  python -c "import urllib.request; urllib.request.urlopen('http://fake-services:$fake_port/api/', timeout=2).read()" \
  >/dev/null 2>&1; then
  docker logs "$fake_container_id" >&2
  fail "fake dependency server did not become HTTP-ready within 30 seconds"
fi

docker buildx build \
  --platform "$platform" \
  --load \
  --tag "$image" \
  --build-arg VERSION=container-test \
  --build-arg REVISION=container-test \
  --build-arg CREATED=1970-01-01T00:00:00Z \
  .

config_json=$(docker image inspect "$image")
python3 - "$config_json" <<'PY'
import json
import sys

image = json.loads(sys.argv[1])[0]
config = image["Config"]
if not config.get("User") or config["User"] == "0" or config["User"].startswith("0:"):
    raise SystemExit("image does not configure a non-root user")
health = config.get("Healthcheck", {}).get("Test", [])
if not health or health[0] != "CMD":
    raise SystemExit("healthcheck is not exec form")
entrypoint = config.get("Entrypoint") or []
if entrypoint != ["/usr/local/bin/paperless-ai-ocr"]:
    raise SystemExit(f"unexpected entrypoint: {entrypoint!r}")
for value in config.get("Env") or []:
    name = value.partition("=")[0]
    if name in {
        "PAPERLESS_URL", "PAPERLESS_API_TOKEN", "AI_BASE_URL", "AI_API_KEY",
        "AI_MODEL", "WEBHOOK_TOKEN", "PAPERLESS_AI_WEBHOOK_URL",
        "PAPERLESS_AI_WEBHOOK_KEY",
    }:
        raise SystemExit(f"runtime configuration embedded in image: {name}")
labels = config.get("Labels") or {}
for name in (
    "org.opencontainers.image.source",
    "org.opencontainers.image.title",
    "org.opencontainers.image.description",
    "org.opencontainers.image.licenses",
    "org.opencontainers.image.version",
    "org.opencontainers.image.revision",
    "org.opencontainers.image.created",
):
    if not labels.get(name):
        raise SystemExit(f"missing OCI label: {name}")
PY

docker history --no-trunc "$image" >"$work_dir/history.txt"
if grep -Eiq '(PAPERLESS_API_TOKEN|AI_API_KEY|WEBHOOK_TOKEN|PAPERLESS_AI_WEBHOOK_KEY)=' "$work_dir/history.txt"; then
  fail "image history contains known runtime credential configuration"
fi

uid=$(docker run --rm --entrypoint /usr/bin/id "$image" -u)
gid=$(docker run --rm --entrypoint /usr/bin/id "$image" -g)
[[ "$uid" == 65532 && "$gid" == 65532 ]] || fail "container identity is $uid:$gid, want 65532:65532"

docker run --rm --entrypoint /bin/sh "$image" -ec \
  'command -v pdfinfo >/dev/null; command -v pdftoppm >/dev/null; test -s /etc/ssl/certs/ca-certificates.crt'

version_output=$(docker run --rm --read-only --tmpfs /tmp:size=16m,mode=0700 --entrypoint /usr/local/bin/paperless-ai-ocr "$image" --version)
[[ "$version_output" == *"version=container-test revision=container-test build_time=1970-01-01T00:00:00Z"* ]] || fail "build metadata was not linked into the binary"

container_id=$(docker run -d \
  --network "$network_name" \
  --read-only \
  --cap-drop ALL \
  --security-opt no-new-privileges:true \
  --tmpfs /tmp:size=64m,mode=0700,uid="$uid",gid="$uid" \
  --mount type=volume,src="$data_volume",dst=/app/data \
  -e NO_PROXY=fake-services \
  -e no_proxy=fake-services \
  -e PAPERLESS_URL="http://fake-services:$fake_port/" \
  -e PAPERLESS_API_TOKEN=test-paperless-token \
  -e AI_BASE_URL="http://fake-services:$fake_port/v1/" \
  -e AI_API_KEY=test-ai-key \
  -e AI_MODEL=test-model \
  -e WEBHOOK_TOKEN=test-webhook-token \
  -e PAPERLESS_AI_WEBHOOK_URL="http://fake-services:$fake_port/paperless-ai" \
  -e PAPERLESS_AI_WEBHOOK_KEY=test-paperless-ai-key \
  -e HTTP_PORT=18080 \
  "$image")

health_deadline=$((SECONDS + 90))
while ((SECONDS < health_deadline)); do
  status=$(docker inspect --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}missing{{end}}' "$container_id")
  case "$status" in
    healthy) break ;;
    unhealthy)
      docker logs "$container_id" >&2
      printf '%s\n' 'fake dependency requests:' >&2
      docker logs "$fake_container_id" >&2
      fail "container became unhealthy"
      ;;
  esac
  if [[ $(docker inspect --format '{{.State.Running}}' "$container_id") != true ]]; then
    docker logs "$container_id" >&2
    printf '%s\n' 'fake dependency requests:' >&2
    docker logs "$fake_container_id" >&2
    fail "container exited before becoming healthy"
  fi
  sleep 1
done
[[ $(docker inspect --format '{{.State.Health.Status}}' "$container_id") == healthy ]] || {
  docker logs "$container_id" >&2
  fail "healthcheck did not become healthy within 90 seconds"
}

docker exec "$container_id" /bin/sh -ec '
  test "$(id -u):$(id -g)" = 65532:65532
  test "$(stat -c %a:%u:%g /app/data)" = 700:65532:65532
  test "$(stat -c %a:%u:%g /tmp)" = 700:65532:65532
  touch /app/data/container-test
  touch /tmp/container-test
'

docker stop --time 15 "$container_id" >/dev/null
exit_code=$(docker inspect --format '{{.State.ExitCode}}' "$container_id")
oom_killed=$(docker inspect --format '{{.State.OOMKilled}}' "$container_id")
runtime_error=$(docker inspect --format '{{.State.Error}}' "$container_id")
[[ "$exit_code" == 0 && "$oom_killed" == false && -z "$runtime_error" ]] || {
  docker logs "$container_id" >&2
  fail "SIGTERM did not produce a clean exit (status $exit_code, OOM=$oom_killed, runtime error=$runtime_error)"
}
docker rm "$container_id" >/dev/null
container_id=""

builder_name="paperless-ai-ocr-test-$RANDOM-$$"
docker buildx create --name "$builder_name" --driver docker-container >/dev/null
docker buildx inspect "$builder_name" --bootstrap >/dev/null
docker buildx build \
  --builder "$builder_name" \
  --platform linux/arm64 \
  --output "type=oci,dest=$work_dir/arm64.tar" \
  --build-arg VERSION=container-test \
  --build-arg REVISION=container-test \
  --build-arg CREATED=1970-01-01T00:00:00Z \
  .

mkdir "$work_dir/arm64"
tar -xf "$work_dir/arm64.tar" -C "$work_dir/arm64"
python3 - "$work_dir/arm64" <<'PY'
import json
import pathlib
import sys

root = pathlib.Path(sys.argv[1])
index = json.loads((root / "index.json").read_text())
descriptor = next(
    (
        item for item in index["manifests"]
        if item.get("mediaType") in {
            "application/vnd.oci.image.manifest.v1+json",
            "application/vnd.docker.distribution.manifest.v2+json",
        }
        and item.get("platform", {}).get("os") == "linux"
        and item.get("platform", {}).get("architecture") == "arm64"
    ),
    None,
)
if descriptor is None:
    raise SystemExit("OCI index contains no runnable linux/arm64 image manifest")
manifest = json.loads((root / "blobs" / "sha256" / descriptor["digest"].split(":", 1)[1]).read_text())
config_desc = manifest["config"]
config = json.loads((root / "blobs" / "sha256" / config_desc["digest"].split(":", 1)[1]).read_text())
if config.get("os") != "linux" or config.get("architecture") != "arm64":
    raise SystemExit(f"unexpected ARM64 metadata: {config.get('os')}/{config.get('architecture')}")
PY

printf 'container-test: all checks passed\n'
