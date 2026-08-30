#!/usr/bin/env python3
"""Validate CI/release workflow policy without executing privileged actions."""

from __future__ import annotations

import re
from pathlib import Path


ROOT = Path(__file__).resolve().parent.parent
SHA = r"[0-9a-f]{40}"
ALLOWED_ACTIONS = {
    "actions/checkout": ("3d3c42e5aac5ba805825da76410c181273ba90b1", "v7.0.1"),
    "actions/setup-go": ("b7ad1dad31e06c5925ef5d2fc7ad053ef454303e", "v7.0.0"),
    "docker/setup-buildx-action": ("37fe631027851001ddb9b187196cc803df7f5f0e", "v4.3.0"),
    "docker/login-action": ("dbcb813823bdd20940b903addbd779551569679f", "v4.6.0"),
    "docker/metadata-action": ("dc802804100637a589fabce1cb79ff13a1411302", "v6.2.0"),
    "docker/build-push-action": ("53b7df96c91f9c12dcc8a07bcb9ccacbed38856a", "v7.3.0"),
    "actions/attest": ("1e69f48acb82d1966a394da916b4c1698aa569d6", "v4.2.2"),
    "golangci/golangci-lint-action": ("ba0d7d2ec06a0ea1cb5fa41b2e4a3ab91d21278a", "v9.3.0"),
    "golang/govulncheck-action": ("032d45514ae346b1db93c04b0c90b841c370344f", "v1.1.0"),
}


def require(condition: bool, message: str) -> None:
    if not condition:
        raise AssertionError(message)


def load(name: str) -> str:
    path = ROOT / ".github" / "workflows" / name
    require(path.is_file(), f"missing workflow: {path.relative_to(ROOT)}")
    return path.read_text(encoding="utf-8")


def top_level_block(text: str, key: str) -> str:
    match = re.search(rf"(?m)^{re.escape(key)}:\s*$", text)
    require(match is not None, f"missing top-level {key!r} block")
    lines = text[match.end() :].splitlines(keepends=True)
    block: list[str] = []
    for line in lines:
        if line.strip() and not line.startswith((" ", "\t", "#")):
            break
        block.append(line)
    return "".join(block)


def assert_pinned_actions(text: str) -> None:
    uses = re.findall(r"(?m)^\s*-?\s*uses:\s*([^\s#]+)\s+#\s+(v[^\s]+)\s*$", text)
    require(uses, "workflow has no actions")
    require(text.count("uses:") == len(uses), "every action must have a trailing release-version comment")
    for value, version in uses:
        match = re.fullmatch(rf"([^@]+)@({SHA})", value)
        require(match is not None, f"action is not pinned to a full SHA: {value}")
        action, revision = match.groups()
        require(action in ALLOWED_ACTIONS, f"unexpected action: {action}")
        expected_revision, expected_version = ALLOWED_ACTIONS[action]
        require(revision == expected_revision, f"unverified SHA for {action}")
        require(version == expected_version, f"incorrect release comment for {action}")


def assert_no_unsafe_inputs(text: str) -> None:
    require("pull_request_target" not in text, "pull_request_target is forbidden")
    require("secrets." not in text, "custom repository secrets are forbidden")
    require("permissions: write-all" not in text, "write-all permissions are forbidden")
    credential_names = (
        "PAPERLESS_API_TOKEN",
        "AI_API_KEY",
        "WEBHOOK_TOKEN",
        "PAPERLESS_AI_WEBHOOK_KEY",
    )
    for name in credential_names:
        require(name not in text, f"credential-looking workflow input is forbidden: {name}")


def assert_ci(text: str) -> None:
    trigger = top_level_block(text, "on")
    permissions = top_level_block(text, "permissions")
    require("push:" in trigger and "pull_request:" in trigger, "CI must run for pushes and pull requests")
    require(re.search(r"(?m)^\s+branches:\s+\['\*\*'\]\s*$", trigger) is not None, "CI push validation must cover all branches without matching tags")
    require(re.search(r"(?m)^\s+contents:\s+read\s*$", permissions) is not None, "CI contents permission must be read")
    require("write" not in permissions, "CI must not have write permissions")
    require("cancel-in-progress: true" in text, "CI must cancel superseded branch/PR runs")
    require(text.count("timeout-minutes:") == 5, "every CI job must have a timeout")
    require(text.count("poppler-utils util-linux") == 2, "normal and race tests must install Poppler and setsid")

    commands = (
        "gofmt -l",
        "python3 scripts/workflow-policy-test.py",
        "go vet ./...",
        "go test ./...",
        "go test -race ./...",
        "TestInspectRealPDFs|TestRenderRealPDFOnePage|TestRenderRealPDFRangeAndSequentialReuse",
    )
    for command in commands:
        require(command in text, f"CI missing required command/input: {command}")
    require("golangci/golangci-lint-action" in text, "CI missing golangci-lint")
    require("golang/govulncheck-action" in text, "CI missing govulncheck")
    require("docker/build-push-action" in text, "CI missing Docker build")
    require(re.search(r"(?m)^\s+push:\s+false\s*$", text) is not None, "CI Docker build must not push")
    require("linux/amd64" in text, "CI Docker build must cover linux/amd64")


def assert_release(text: str) -> None:
    trigger = top_level_block(text, "on")
    permissions = top_level_block(text, "permissions")
    require(re.search(r"(?m)^\s+tags:\s*$", trigger) is not None, "release must use a tag filter")
    require(re.search(r"(?m)^\s+- ['\"]v\*['\"]\s*$", trigger) is not None, "release tag filter must be v*")
    for permission in ("contents: read", "packages: write", "id-token: write", "attestations: write"):
        require(permission in permissions, f"release missing permission: {permission}")
    require("artifact-metadata:" not in permissions, "artifact-metadata permission is unnecessary")
    require("cancel-in-progress: false" in text, "release publishing must not be cancelled in progress")
    require(text.count("timeout-minutes:") == 1, "the release job must have a timeout")

    required = (
        "registry: ghcr.io",
        "username: ${{ github.actor }}",
        "password: ${{ github.token }}",
        "ghcr.io/${{ github.repository }}",
        "type=raw,value=${{ github.ref_name }}",
        "type=semver,pattern={{version}}",
        "type=semver,pattern={{major}}.{{minor}}",
        "platforms: linux/amd64,linux/arm64",
        "push: true",
        "sbom: true",
        "provenance: mode=max",
        "VERSION=${{ github.ref_name }}",
        "REVISION=${{ github.sha }}",
        "subject-name: ghcr.io/${{ github.repository }}",
        "subject-digest: ${{ steps.build.outputs.digest }}",
        "push-to-registry: true",
        "create-storage-record: false",
        "steps.build.outputs.digest",
        "GITHUB_STEP_SUMMARY",
    )
    for value in required:
        require(value in text, f"release missing required policy value: {value}")
    require(re.search(r"(?m)^\s+CREATED=\$\{\{ steps\.build-metadata\.outputs\.created \}\}\s*$", text) is not None, "release CREATED build arg must use generated RFC3339 output")
    require("latest=false" in text, "release must explicitly disable the latest tag")
    require("value=latest" not in text.lower(), "release must not publish latest")


def main() -> None:
    ci = load("ci.yml")
    release = load("release.yml")
    for workflow in (ci, release):
        assert_pinned_actions(workflow)
        assert_no_unsafe_inputs(workflow)
    assert_ci(ci)
    assert_release(release)
    print("workflow policy validation passed")


if __name__ == "__main__":
    main()
