#!/usr/bin/env python3
"""Validate the structure and security policy of controlled workflows."""

from __future__ import annotations

import re
from dataclasses import dataclass
from pathlib import Path


ROOT = Path(__file__).resolve().parent.parent
SHA = r"[0-9a-f]{40}"
ALLOWED_ACTIONS = {
    "actions/checkout": ("3d3c42e5aac5ba805825da76410c181273ba90b1", "v7.0.1"),
    "actions/setup-go": ("b7ad1dad31e06c5925ef5d2fc7ad053ef454303e", "v7.0.0"),
    "docker/setup-qemu-action": ("96fe6ef7f33517b61c61be40b68a1882f3264fb8", "v4.2.0"),
    "docker/setup-buildx-action": ("37fe631027851001ddb9b187196cc803df7f5f0e", "v4.3.0"),
    "docker/login-action": ("dbcb813823bdd20940b903addbd779551569679f", "v4.6.0"),
    "docker/metadata-action": ("dc802804100637a589fabce1cb79ff13a1411302", "v6.2.0"),
    "docker/build-push-action": ("53b7df96c91f9c12dcc8a07bcb9ccacbed38856a", "v7.3.0"),
    "actions/attest": ("1e69f48acb82d1966a394da916b4c1698aa569d6", "v4.2.2"),
    "golangci/golangci-lint-action": ("ba0d7d2ec06a0ea1cb5fa41b2e4a3ab91d21278a", "v9.3.0"),
}
EXPECTED_RELEASE_PERMISSIONS = {
    "contents": "read",
    "packages": "write",
    "id-token": "write",
    "attestations": "write",
}
RELEASE_TAG_PATTERN = "readonly tag_pattern='^v(0|[1-9][0-9]*)\\.(0|[1-9][0-9]*)\\.(0|[1-9][0-9]*)$'"
QEMU_IMAGE = "docker.io/tonistiigi/binfmt@sha256:400a4873b838d1b89194d982c45e5fb3cda4593fbfd7e08a02e76b03b21166f0"


def require(condition: bool, message: str) -> None:
    if not condition:
        raise AssertionError(message)


def indentation(line: str) -> int:
    return len(line) - len(line.lstrip(" "))


def uncommented(lines: list[str]) -> str:
    return "\n".join(line for line in lines if not line.lstrip().startswith("#"))


@dataclass(frozen=True)
class Block:
    header: str
    indent: int
    lines: list[str]

    @property
    def text(self) -> str:
        return uncommented(self.lines)

    def child(self, key: str) -> Block:
        wanted = f"{' ' * (self.indent + 2)}{key}:"
        matches = [index for index, line in enumerate(self.lines) if line == wanted]
        require(len(matches) == 1, f"{self.header} must contain exactly one {key!r} block")
        return make_block(self.lines, matches[0], key)

    def children(self) -> dict[str, Block]:
        child_indent = self.indent + 2
        result: dict[str, Block] = {}
        for index, line in enumerate(self.lines):
            match = re.fullmatch(rf" {{{child_indent}}}([A-Za-z0-9_-]+):", line)
            if match:
                key = match.group(1)
                require(key not in result, f"duplicate {key!r} in {self.header}")
                result[key] = make_block(self.lines, index, key)
        return result

    def values(self) -> dict[str, str]:
        value_indent = self.indent + 2
        result: dict[str, str] = {}
        for line in self.lines[1:]:
            if indentation(line) != value_indent or line.lstrip().startswith(("-", "#")):
                continue
            match = re.fullmatch(r"\s*([A-Za-z0-9_-]+):\s*(.*?)\s*", line)
            if match and match.group(2):
                result[match.group(1)] = match.group(2).strip("'\"")
        return result


def make_block(lines: list[str], start: int, header: str) -> Block:
    indent = indentation(lines[start])
    end = len(lines)
    for index in range(start + 1, len(lines)):
        line = lines[index]
        if line.strip() and indentation(line) <= indent:
            end = index
            break
    return Block(header, indent, lines[start:end])


class Workflow:
    def __init__(self, name: str) -> None:
        self.path = ROOT / ".github" / "workflows" / name
        require(self.path.is_file(), f"missing workflow: {self.path.relative_to(ROOT)}")
        self.lines = self.path.read_text(encoding="utf-8").splitlines()

    @property
    def text(self) -> str:
        return uncommented(self.lines)

    def top(self, key: str) -> Block:
        matches = [index for index, line in enumerate(self.lines) if line == f"{key}:"]
        require(len(matches) == 1, f"workflow must contain exactly one top-level {key!r} block")
        return make_block(self.lines, matches[0], key)

    def jobs(self) -> dict[str, Block]:
        return self.top("jobs").children()


def step_blocks(job: Block) -> list[Block]:
    steps = job.child("steps")
    step_indent = steps.indent + 2
    starts = [index for index, line in enumerate(steps.lines) if re.fullmatch(rf" {{{step_indent}}}- name: .+", line)]
    require(starts, f"job {job.header!r} has no named steps")
    result = []
    for position, start in enumerate(starts):
        end = starts[position + 1] if position + 1 < len(starts) else len(steps.lines)
        name = steps.lines[start].split(":", 1)[1].strip()
        result.append(Block(name, step_indent, steps.lines[start:end]))
    return result


def named_steps(job: Block) -> dict[str, Block]:
    steps = step_blocks(job)
    result = {step.header: step for step in steps}
    require(len(result) == len(steps), f"job {job.header!r} has duplicate step names")
    return result


def permissions(block: Block) -> dict[str, str]:
    return block.child("permissions").values()


def assert_run_command(step: Block, command: str, message: str) -> None:
    values = step.values()
    require(values.get("run") == command, message)


def assert_required_step(step: Block, command: str, message: str) -> None:
    assert_run_command(step, command, message)
    values = step.values()
    require("if" not in values, f"{step.header} must not be conditional")
    require("continue-on-error" not in values, f"{step.header} must fail closed")


def assert_pinned_actions(workflow: Workflow) -> None:
    uses = re.findall(r"(?m)^\s+uses:\s*([^\s#]+)\s+#\s+(v[^\s]+)\s*$", workflow.text)
    require(uses, f"{workflow.path.name} has no actions")
    require(workflow.text.count("uses:") == len(uses), f"every action in {workflow.path.name} must have a release comment")
    for value, version in uses:
        match = re.fullmatch(rf"([^@]+)@({SHA})", value)
        require(match is not None, f"action is not pinned to a full SHA: {value}")
        action, revision = match.groups()
        require(action in ALLOWED_ACTIONS, f"unexpected action: {action}")
        expected_revision, expected_version = ALLOWED_ACTIONS[action]
        require(revision == expected_revision, f"unverified SHA for {action}")
        require(version == expected_version, f"incorrect release comment for {action}")


def assert_no_unsafe_inputs(workflow: Workflow) -> None:
    text = workflow.text
    require("pull_request_target" not in text, "pull_request_target is forbidden")
    require("secrets." not in text, "custom repository secrets are forbidden")
    require("permissions: write-all" not in text, "write-all permissions are forbidden")
    for name in ("PAPERLESS_API_TOKEN", "AI_API_KEY", "WEBHOOK_TOKEN", "PAPERLESS_AI_WEBHOOK_KEY"):
        require(name not in text, f"credential-looking workflow input is forbidden: {name}")


def assert_release_metadata_script() -> None:
    path = ROOT / "scripts" / "release-metadata.sh"
    require(path.is_file(), "missing release metadata script")
    lines = path.read_text(encoding="utf-8").splitlines()
    require(lines.count(RELEASE_TAG_PATTERN) == 1, "release tag regex must be exact stable semver")
    require(lines.count('readonly head=$(git rev-parse HEAD)') == 1, "release metadata must resolve the checked-out HEAD")
    require(lines.count('if [[ ${GITHUB_SHA:-} != "$head" ]]; then') == 1, "release metadata must bind HEAD to GITHUB_SHA")
    require(lines.count('  exit 1') >= 2, "release metadata validation must fail on a checkout mismatch")
    require(
        lines.count('readonly committed_at=$(git show -s --format=%cI HEAD)') == 1,
        "release CREATED must come from the tagged commit timestamp",
    )
    require(
        lines.count('readonly created=$(date --utc --date="$committed_at" +\'%Y-%m-%dT%H:%M:%SZ\')') == 1,
        "release CREATED must be normalized to RFC3339 UTC",
    )


def assert_ci(workflow: Workflow) -> None:
    trigger = workflow.top("on")
    require(trigger.children().keys() == {"push", "pull_request"}, "CI triggers must be exactly push and pull_request")
    require("branches: ['**']" in trigger.child("push").text, "CI push trigger must cover branches without tags")
    require(workflow.top("permissions").values() == {"contents": "read"}, "CI workflow permissions must be contents: read")
    require(workflow.top("env").values() == {"GOVULNCHECK_VERSION": "v1.7.0"}, "CI govulncheck version must be v1.7.0")
    jobs = workflow.jobs()
    require(jobs.keys() == {"test", "lint", "race", "vulnerability-scan", "docker-build"}, "unexpected CI job set")
    for job in jobs.values():
        if "permissions:" in job.text:
            require(permissions(job) == {"contents": "read"}, f"CI job {job.header!r} may only read contents")
        require("timeout-minutes:" in job.text, f"CI job {job.header!r} must have a timeout")

    test_steps = named_steps(jobs["test"])
    policy = test_steps["Validate workflow policy"].text
    require("python3 scripts/workflow-policy-test.py" in policy, "CI must run workflow policy validation")
    require("python3 scripts/workflow-policy-regression-test.py" in policy, "CI must run policy mutation tests")
    require("bash scripts/release-metadata-test.sh" in policy, "CI must test release metadata validation")
    require("bash scripts/release-state-test.sh" in policy, "CI must test recoverable release state")
    require("gofmt -l" in test_steps["Check formatting"].text, "CI formatting step must run gofmt")
    require("go vet ./..." in test_steps["Vet"].text, "CI vet step missing")
    require("go test ./..." in test_steps["Unit tests"].text, "CI unit test step missing")
    require("TestInspectRealPDFs|TestRenderRealPDFOnePage|TestRenderRealPDFRangeAndSequentialReuse" in test_steps["Run real Poppler tests explicitly"].text, "CI explicit Poppler tests missing")
    require("go test -race ./..." in named_steps(jobs["race"])["Run race tests"].text, "CI race tests missing")
    vulnerability_steps = named_steps(jobs["vulnerability-scan"])
    require("go install golang.org/x/vuln/cmd/govulncheck@${GOVULNCHECK_VERSION}" in vulnerability_steps["Install pinned govulncheck"].text, "CI govulncheck install must be pinned")
    require("govulncheck ./..." in vulnerability_steps["Run govulncheck"].text, "CI vulnerability scan missing")
    docker = named_steps(jobs["docker-build"])["Build linux/amd64 image"].text
    require("platforms: linux/amd64" in docker and "push: false" in docker, "CI Docker build policy is invalid")


def assert_qemu_before_buildx(job: Block) -> None:
    steps = step_blocks(job)
    names = [step.header for step in steps]
    require(names.index("Set up QEMU") < names.index("Set up Docker Buildx"), f"QEMU must precede Buildx in {job.header}")
    qemu = named_steps(job)["Set up QEMU"].text
    require("docker/setup-qemu-action@96fe6ef7f33517b61c61be40b68a1882f3264fb8 # v4.2.0" in qemu, "QEMU action pin is invalid")
    require("platforms: arm64" in qemu, "QEMU must register arm64")
    require("image: ${{ env.QEMU_IMAGE }}" in qemu, "QEMU image must be digest-pinned")


def assert_release(workflow: Workflow) -> None:
    trigger = workflow.top("on")
    require(trigger.children().keys() == {"push"}, "release trigger must contain only push")
    tag_lines = [line.strip() for line in trigger.child("push").child("tags").lines[1:] if line.strip() and not line.lstrip().startswith("#")]
    require(tag_lines == ["- 'v*'"], "release tag trigger must be exactly v*")
    require(workflow.top("permissions").values() == {"contents": "read"}, "release workflow permissions must be contents: read")
    require(
        workflow.top("concurrency").values()
        == {"group": "release-container-image", "cancel-in-progress": "false"},
        "release publication must use one non-cancelling global concurrency group",
    )
    require(
        workflow.top("env").values()
        == {
            "ACTIONLINT_VERSION": "v1.7.12",
            "GOVULNCHECK_VERSION": "v1.7.0",
            "QEMU_IMAGE": QEMU_IMAGE,
        },
        "release tool versions and QEMU image must be exactly pinned",
    )
    jobs = workflow.jobs()
    require(jobs.keys() == {"verify", "publish"}, "release must contain exactly verify and publish jobs")
    verify = jobs["verify"]
    publish = jobs["publish"]
    require(permissions(verify) == {"contents": "read"}, "verify job must only read contents")
    require(permissions(publish) == EXPECTED_RELEASE_PERMISSIONS, "publish job permissions are not least privilege")
    require(publish.values().get("needs") == "verify", "publish job must need verify")
    require("timeout-minutes:" in verify.text and "timeout-minutes:" in publish.text, "release jobs must have timeouts")

    verify_steps = named_steps(verify)
    assert_required_step(
        verify_steps["Validate release tag and metadata"],
        "bash scripts/release-metadata.sh",
        "verify must validate the release tag",
    )
    policy = verify_steps["Validate workflow policy"].text
    for command in ("actionlint .github/workflows/ci.yml .github/workflows/release.yml", "python3 scripts/workflow-policy-test.py", "python3 scripts/workflow-policy-regression-test.py", "bash scripts/release-metadata-test.sh", "bash scripts/release-state-test.sh"):
        require(command in policy, f"verify policy step missing: {command}")
    required_steps = {
        "Check formatting": "gofmt -l",
        "Vet": "go vet ./...",
        "Unit tests": "go test ./...",
        "Race tests": "go test -race ./...",
        "Run real Poppler tests explicitly": "TestInspectRealPDFs|TestRenderRealPDFOnePage|TestRenderRealPDFRangeAndSequentialReuse",
        "Vulnerability scan": "govulncheck ./...",
    }
    for name, command in required_steps.items():
        require(command in verify_steps[name].text, f"verify step {name!r} is incomplete")
    lint = verify_steps["Run golangci-lint"]
    require("golangci/golangci-lint-action" in lint.text, "verify lint step missing")
    require(lint.child("with").values() == {"version": "v2.13.2"}, "verify golangci-lint version must be v2.13.2")
    install = verify_steps["Install pinned verification tools"].text
    require("actionlint/cmd/actionlint@${ACTIONLINT_VERSION}" in install, "actionlint install must be pinned")
    require("govulncheck@${GOVULNCHECK_VERSION}" in install, "govulncheck install must be pinned")
    verify_build = verify_steps["Cross-build release image"].text
    require("platforms: linux/amd64,linux/arm64" in verify_build and "push: false" in verify_build, "verify must cross-build without pushing")
    assert_qemu_before_buildx(verify)

    publish_steps = named_steps(publish)
    names = [step.header for step in step_blocks(publish)]
    validation_name = "Validate release tag and metadata before publication"
    assert_required_step(
        publish_steps[validation_name],
        "bash scripts/release-metadata.sh",
        "publish must execute tag validation",
    )
    require(names.index(validation_name) < names.index("Log in to GitHub Container Registry"), "tag validation must precede registry login")
    assert_qemu_before_buildx(publish)
    metadata = publish_steps["Generate image metadata"].text
    require("DOCKER_METADATA_ANNOTATIONS_LEVELS: manifest,index" in metadata, "multi-arch annotations must cover manifest and index")
    require("flavor: latest=false" in metadata and "value=latest" not in metadata.lower(), "release must not publish latest")
    for tag in ("type=raw,value=${{ steps.release-metadata.outputs.version }}", "type=semver,pattern={{version}},value=${{ steps.release-metadata.outputs.version }}"):
        require(tag in metadata, f"release metadata missing tag rule: {tag}")
    require("pattern={{major}}.{{minor}}" not in metadata, "release must not publish a mutable major.minor alias")
    created = "org.opencontainers.image.created=${{ steps.release-metadata.outputs.created }}"
    require(metadata.count(created) == 2, "deterministic created metadata must override both labels and annotations")
    require(f"          labels: |\n            {created}" in metadata, "created label override is misplaced")
    require(f"          annotations: |\n            {created}" in metadata, "created annotation override is misplaced")
    state_name = "Determine release state"
    state = publish_steps[state_name]
    assert_required_step(state, "bash scripts/release-state.sh", "publish must determine recoverable release state")
    require(state.values().get("id") == "release-state", "release state step must expose outputs")
    require(
        state.child("env").values()
        == {
            "IMAGE": "ghcr.io/${{ github.repository }}",
            "VERSION": "${{ steps.release-metadata.outputs.version }}",
            "REVISION": "${{ github.sha }}",
        },
        "release state inputs are invalid",
    )
    require(names.index("Generate image metadata") < names.index(state_name) < names.index("Build and push image"), "release state must be determined after metadata and before push")
    build_step = publish_steps["Build and push image"]
    require("        if: steps.release-state.outputs.publish == 'true'" in build_step.lines, "build and push must run only for an unpublished release")
    build = build_step.text
    for value in ("platforms: linux/amd64,linux/arm64", "push: true", "VERSION=${{ steps.release-metadata.outputs.version }}", "REVISION=${{ github.sha }}", "CREATED=${{ steps.release-metadata.outputs.created }}", "sbom: true", "provenance: mode=max"):
        require(value in build, f"publish build missing: {value}")
    final_digest = publish_steps["Resolve final digest"]
    require("if" not in final_digest.values(), "final digest resolution must always run after release state succeeds")
    require(final_digest.values().get("id") == "final-digest", "final digest step must expose its verified digest")
    for value in (
        "PUBLISH: ${{ steps.release-state.outputs.publish }}",
        "EXISTING_DIGEST: ${{ steps.release-state.outputs.digest }}",
        "BUILT_DIGEST: ${{ steps.build.outputs.digest }}",
        "printf 'digest=%s\\n' \"$digest\" >> \"$GITHUB_OUTPUT\"",
        "^sha256:[0-9a-f]{64}$",
    ):
        require(value in final_digest.text, f"final digest resolution missing: {value}")
    require(names.index("Build and push image") < names.index("Resolve final digest") < names.index("Attest image provenance"), "final digest must be resolved after the optional build and before attestation")
    require(publish.child("outputs").values() == {"digest": "${{ steps.final-digest.outputs.digest }}"}, "publish output must use the verified final digest")
    attest = publish_steps["Attest image provenance"].text
    for value in ("subject-name: ghcr.io/${{ github.repository }}", "subject-digest: ${{ steps.final-digest.outputs.digest }}", "push-to-registry: true", "create-storage-record: false"):
        require(value in attest, f"attestation missing: {value}")
    summary = publish_steps["Summarize release"].text
    require("DIGEST: ${{ steps.final-digest.outputs.digest }}" in summary, "release summary must use the verified final digest")
    require("GITHUB_STEP_SUMMARY" in summary and "Package visibility is not controlled by this workflow" in summary, "release summary is incomplete")


def main() -> None:
    ci = Workflow("ci.yml")
    release = Workflow("release.yml")
    for workflow in (ci, release):
        assert_pinned_actions(workflow)
        assert_no_unsafe_inputs(workflow)
    assert_release_metadata_script()
    assert_ci(ci)
    assert_release(release)
    print("workflow policy validation passed")


if __name__ == "__main__":
    main()
