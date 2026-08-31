#!/usr/bin/env python3
"""Prove workflow policy validation rejects security-sensitive mutations."""

from __future__ import annotations

import re
import shutil
import subprocess
import sys
import tempfile
from collections.abc import Callable
from pathlib import Path


ROOT = Path(__file__).resolve().parent.parent
POLICY_INPUTS = (
    ".markdown-link-check.json",
    ".markdownlint-cli2.jsonc",
    ".markdownlint.json",
    ".github/workflows/ci.yml",
    ".github/workflows/release.yml",
    "Dockerfile",
    "go.mod",
    "package.json",
    "package-lock.json",
    "scripts/release-metadata.sh",
    "scripts/release-state.sh",
)


def load_at_ref(relative_path: str, ref: str | None) -> str:
    if ref is None:
        return (ROOT / relative_path).read_text(encoding="utf-8")
    result = subprocess.run(
        ["git", "show", f"{ref}:{relative_path}"],
        cwd=ROOT,
        check=True,
        capture_output=True,
        text=True,
    )
    return result.stdout


def mutate_release_trigger(text: str) -> str:
    return text.replace("      - 'v*'", "      - 'v*'\n      - '**'", 1)


def mutate_ci_job_permission(text: str) -> str:
    return text.replace(
        "    timeout-minutes: 15\n    steps:",
        "    timeout-minutes: 15\n    permissions:\n      contents: write\n    steps:",
        1,
    )


def mutate_release_extra_job(text: str) -> str:
    return text + (
        "\n  unverified-publish:\n"
        "    runs-on: ubuntu-latest\n"
        "    permissions:\n"
        "      packages: write\n"
        "    steps: []\n"
    )


def mutate_release_workflow_permission(text: str) -> str:
    return text.replace(
        "permissions:\n  contents: read",
        "permissions:\n  contents: read\n  packages: write",
        1,
    )


def mutate_fake_tag_validation(text: str) -> str:
    mutated = text.replace(
        "      - name: Validate release tag and metadata before publication\n"
        "        id: release-metadata\n"
        "        run: bash scripts/release-metadata.sh\n",
        "      # Validate release tag and metadata before publication\n"
        "      # run: bash scripts/release-metadata.sh\n",
        1,
    )
    if mutated != text:
        return mutated
    return text.replace(
        "      - name: Log in to GitHub Container Registry\n",
        "      # Validate release tag with ^v(0|[1-9][0-9]*)\\.(0|[1-9][0-9]*)\\.(0|[1-9][0-9]*)$\n"
        "      # run: bash scripts/release-metadata.sh\n"
        "      - name: Log in to GitHub Container Registry\n",
        1,
    )


def mutate_relocate_tag_validation(text: str) -> str:
    validation = (
        "      - name: Validate release tag and metadata before publication\n"
        "        id: release-metadata\n"
        "        run: bash scripts/release-metadata.sh\n"
    )
    if validation in text:
        return text.replace(validation, "", 1).replace(
            "      - name: Summarize release\n",
            validation + "      - name: Summarize release\n",
            1,
        )
    return text.replace(
        "      - name: Summarize release\n",
        "      - name: Validate release tag too late\n"
        "        run: test '${{ github.ref_name }}' != invalid\n"
        "      - name: Summarize release\n",
        1,
    )


def mutate_bypass_tag_validation(text: str) -> str:
    return text.replace(
        "        run: bash scripts/release-metadata.sh\n",
        "        run: true # bash scripts/release-metadata.sh\n",
        1,
    )


def mutate_required_values_in_unrelated_step(text: str) -> str:
    old = "          sbom: true\n          provenance: mode=max\n"
    new = (
        "          sbom: false\n"
        "          provenance: false\n"
        "      - name: Document intended supply-chain settings\n"
        "        run: |\n"
        "          # sbom: true\n"
        "          printf '%s\\n' 'provenance: mode=max' >/dev/null\n"
    )
    return text.replace(old, new, 1)


def mutate_permissive_tag_regex(text: str) -> str:
    return text.replace(
        "readonly tag_pattern='^v(0|[1-9][0-9]*)\\.(0|[1-9][0-9]*)\\.(0|[1-9][0-9]*)$'",
        "readonly tag_pattern='^v.*$'",
        1,
    )


def mutate_wall_clock_created(text: str) -> str:
    return text.replace(
        "readonly committed_at=$(git show -s --format=%cI HEAD)\n"
        "readonly created=$(date --utc --date=\"$committed_at\" +'%Y-%m-%dT%H:%M:%SZ')",
        "readonly created=$(date --utc +'%Y-%m-%dT%H:%M:%SZ')",
        1,
    )


def mutate_govulncheck_version(text: str) -> str:
    return text.replace("GOVULNCHECK_VERSION: v1.7.0", "GOVULNCHECK_VERSION: latest", 1)


def mutate_remove_acceptance_job(text: str) -> str:
    start = text.index("\n  acceptance:\n")
    end = text.index("\n  vulnerability-scan:\n", start)
    return text[:start] + text[end:]


def mutate_bypass_acceptance(text: str) -> str:
    return text.replace(
        "      - name: Run acceptance tests\n        run: bash scripts/acceptance.sh\n",
        "      - name: Run acceptance tests\n        if: false\n        run: bash scripts/acceptance.sh\n",
        1,
    )


def mutate_markdownlint_version(text: str) -> str:
    return text.replace('"markdownlint-cli2": "0.23.2"', '"markdownlint-cli2": "latest"', 1)


def mutate_markdown_link_check_version(text: str) -> str:
    return text.replace('"markdown-link-check": "3.15.0"', '"markdown-link-check": "^3.15.0"', 1)


def mutate_remove_node_modules_markdownlint_ignore(text: str) -> str:
    return text.replace(',\n    "node_modules/**"', "", 1)


def mutate_documentation_node_version(text: str) -> str:
    return text.replace("          node-version: 24.8.0", "          node-version: latest", 1)


def mutate_package_node_version(text: str) -> str:
    return text.replace('"node": "24.8.0"', '"node": ">=24"', 1)


def mutate_lockfile_version(text: str) -> str:
    return text.replace('"lockfileVersion": 3', '"lockfileVersion": 2', 1)


def mutate_remove_lock_integrity(text: str) -> str:
    return re.sub(r'^\s+"integrity": "sha512-[^"]+",?\n', "", text, count=1, flags=re.MULTILINE)


def mutate_corrupt_lock_integrity(text: str) -> str:
    return text.replace('"integrity": "sha512-', '"integrity": "sha256-', 1)


def mutate_remove_npm_ci(text: str) -> str:
    return text.replace("          npm ci --ignore-scripts --no-audit --no-fund\n", "", 1)


def mutate_replace_npm_ci(text: str) -> str:
    return text.replace("npm ci --ignore-scripts --no-audit --no-fund", "npm install", 1)


def mutate_exec_without_no(text: str) -> str:
    return text.replace("npm exec --no -- markdownlint-cli2", "npm exec -- markdownlint-cli2", 1)


def mutate_exec_to_npx(text: str) -> str:
    return text.replace("npm exec --no -- markdownlint-cli2", "npx markdownlint-cli2", 1)


def mutate_reintroduce_documentation_version_env(text: str) -> str:
    return text.replace(
        "      - name: Check documentation\n",
        "      - name: Check documentation\n"
        "        env:\n"
        "          MARKDOWNLINT_VERSION: latest\n",
        1,
    )


def mutate_workflow_npm_registry(text: str) -> str:
    return text.replace(
        "env:\n  GOVULNCHECK_VERSION: v1.7.0\n",
        "env:\n  GOVULNCHECK_VERSION: v1.7.0\n  NPM_CONFIG_REGISTRY: https://example.invalid/\n",
        1,
    )


def mutate_workflow_escaped_npm_registry(text: str) -> str:
    return text.replace(
        "env:\n  GOVULNCHECK_VERSION: v1.7.0\n",
        'env:\n  GOVULNCHECK_VERSION: v1.7.0\n  "NPM_CONFIG_\\x52EGISTRY": https://example.invalid/\n',
        1,
    )


def mutate_workflow_flow_escaped_npm_registry(text: str) -> str:
    return text.replace(
        "env:\n  GOVULNCHECK_VERSION: v1.7.0\n",
        'env: {GOVULNCHECK_VERSION: v1.7.0, "NPM_CONFIG_\\x52EGISTRY": https://example.invalid/}\n',
        1,
    )


def mutate_job_npm_registry(text: str) -> str:
    return text.replace(
        "    timeout-minutes: 15\n    steps:\n",
        "    timeout-minutes: 15\n    env:\n      NPM_CONFIG_REGISTRY: https://example.invalid/\n    steps:\n",
        1,
    )


def mutate_job_escaped_npm_registry(text: str) -> str:
    return text.replace(
        "    timeout-minutes: 15\n    steps:\n",
        '    timeout-minutes: 15\n    env:\n      "NPM_CONFIG_\\x52EGISTRY": https://example.invalid/\n    steps:\n',
        1,
    )


def mutate_job_flow_escaped_npm_registry(text: str) -> str:
    return text.replace(
        "    timeout-minutes: 15\n    steps:\n",
        "    timeout-minutes: 15\n"
        '    env: {"NPM_CONFIG_\\x52EGISTRY": https://example.invalid/}\n'
        "    steps:\n",
        1,
    )


def mutate_documentation_step_npm_registry(text: str) -> str:
    return text.replace(
        "      - name: Check documentation\n",
        "      - name: Check documentation\n"
        "        env:\n"
        "          NPM_CONFIG_REGISTRY: https://example.invalid/\n",
        1,
    )


def mutate_documentation_step_escaped_npm_registry(text: str) -> str:
    return text.replace(
        "      - name: Check documentation\n",
        "      - name: Check documentation\n"
        "        env:\n"
        '          "NPM_CONFIG_\\x52EGISTRY": https://example.invalid/\n',
        1,
    )


def mutate_documentation_step_flow_npm_registry(text: str) -> str:
    return text.replace(
        "      - name: Check documentation\n",
        "      - name: Check documentation\n"
        "        env: {NPM_CONFIG_REGISTRY: https://example.invalid/}\n",
        1,
    )


def mutate_documentation_step_flow_escaped_npm_registry(text: str) -> str:
    return text.replace(
        "      - name: Check documentation\n",
        "      - name: Check documentation\n"
        '        env: {"NPM_CONFIG_\\x52EGISTRY": https://example.invalid/}\n',
        1,
    )


def mutate_documentation_step_npm_userconfig(text: str) -> str:
    return text.replace(
        "      - name: Check documentation\n",
        "      - name: Check documentation\n"
        "        env:\n"
        "          'NPM_CONFIG_USERCONFIG' : /tmp/unsafe-npmrc\n",
        1,
    )


def mutate_documentation_step_npm_replace_registry_host(text: str) -> str:
    return text.replace(
        "      - name: Check documentation\n",
        "      - name: Check documentation\n"
        "        env:\n"
        "          NPM_CONFIG_REPLACE_REGISTRY_HOST: always\n",
        1,
    )


def mutate_documentation_step_npm_prefix(text: str) -> str:
    return text.replace(
        "      - name: Check documentation\n",
        "      - name: Check documentation\n"
        "        env:\n"
        "          NPM_CONFIG_PREFIX: /tmp/unsafe-prefix\n",
        1,
    )


def mutate_documentation_step_node_options(text: str) -> str:
    return text.replace(
        "      - name: Check documentation\n",
        "      - name: Check documentation\n"
        "        env:\n"
        "          NODE_OPTIONS: --require=/tmp/unsafe.cjs\n",
        1,
    )


def mutate_npm_key_in_comment(text: str) -> str:
    return text.replace(
        "      - name: Check documentation\n",
        '      # "NPM_CONFIG_\\x52EGISTRY": https://example.invalid/\n'
        "      - name: Check documentation\n",
        1,
    )


def mutate_npm_key_in_command(text: str) -> str:
    return text.replace(
        "          npm ci --ignore-scripts --no-audit --no-fund\n",
        "          printf '%s\\n' '\"NPM_CONFIG_\\x52EGISTRY\": documentation' >/dev/null\n"
        "          npm ci --ignore-scripts --no-audit --no-fund\n",
        1,
    )


def mutate_ordinary_quoted_env_key(text: str) -> str:
    return text.replace(
        "env:\n  GOVULNCHECK_VERSION: v1.7.0\n",
        'env:\n  "GOVULNCHECK_VERSION": v1.7.0\n',
        1,
    )


def mutate_disable_markdownlint_rule(text: str) -> str:
    return text.replace('"MD024": {', '"MD033": false,\n  "MD024": {', 1)


def mutate_change_markdownlint_rule(text: str) -> str:
    return text.replace('"siblings_only": true', '"siblings_only": false', 1)


def mutate_broad_link_ignore(text: str) -> str:
    return text.replace(
        "^https://github.com/nosovk/paperless-ai-ocr/security/advisories/new$",
        "^https://github.com/.*$",
        1,
    )


def mutate_remove_link_check(text: str) -> str:
    return text.replace('"ignorePatterns": [', '"ignorePatterns": [],\n  "removedIgnorePatterns": [', 1)


def mutate_ignore_link_status(text: str) -> str:
    return text.replace(
        '"retryCount": 2,',
        '"retryCount": 2,\n  "aliveStatusCodes": [200, 403],',
        1,
    )


def mutate_add_legacy_documentation_target(text: str) -> str:
    return text.replace(
        " docs/threat-model.md\n",
        " docs/threat-model.md docs/plans/2026-08-29-paperless-ai-ocr-design.md\n",
        1,
    )


def mutate_remove_normative_documentation_target(text: str) -> str:
    return text.replace(" docs/operations.md docs/threat-model.md\n", " docs/threat-model.md\n", 1)


def mutate_duplicate_documentation_target(text: str) -> str:
    return text.replace(" docs/threat-model.md\n", " docs/threat-model.md README.md\n", 1)


def mutate_actionlint_version(text: str) -> str:
    return text.replace("ACTIONLINT_VERSION: v1.7.12", "ACTIONLINT_VERSION: latest", 1)


def mutate_ci_actionlint_version(text: str) -> str:
    return text.replace("actionlint/cmd/actionlint@v1.7.12", "actionlint/cmd/actionlint@latest", 1)


def mutate_bypass_ci_actionlint(text: str) -> str:
    return text.replace(
        "          actionlint .github/workflows/ci.yml .github/workflows/release.yml\n",
        "          actionlint .github/workflows/ci.yml .github/workflows/release.yml || true\n",
        1,
    )


def mutate_golangci_version(text: str) -> str:
    return text.replace("          version: v2.13.2", "          version: latest", 1)


def mutate_qemu_image(text: str) -> str:
    return text.replace(
        "QEMU_IMAGE: docker.io/tonistiigi/binfmt@sha256:400a4873b838d1b89194d982c45e5fb3cda4593fbfd7e08a02e76b03b21166f0",
        "QEMU_IMAGE: docker.io/tonistiigi/binfmt:latest",
        1,
    )


def mutate_go_mod_downgrade(text: str) -> str:
    return text.replace("go 1.26.6", "go 1.26.0", 1)


def mutate_builder_downgrade(text: str) -> str:
    return text.replace(
        "golang:1.26.6-alpine3.23@sha256:e57c41c1d5864341031181b0db34b9a537bb5773eb6428e4e5bdaea0f9135406",
        "golang:1.26.0-alpine3.23@sha256:d4c4845f5d60c6a974c6000ce58ae079328d03ab7f721a0734277e69905473e5",
        1,
    )


def mutate_builder_version_mismatch(text: str) -> str:
    return text.replace("golang:1.26.6-alpine3.23@", "golang:1.26.0-alpine3.23@", 1)


def mutate_remove_builder_digest(text: str) -> str:
    return text.replace(
        "@sha256:e57c41c1d5864341031181b0db34b9a537bb5773eb6428e4e5bdaea0f9135406",
        "",
        1,
    )


def mutate_wrong_builder_digest(text: str) -> str:
    return text.replace(
        "sha256:e57c41c1d5864341031181b0db34b9a537bb5773eb6428e4e5bdaea0f9135406",
        "sha256:057c41c1d5864341031181b0db34b9a537bb5773eb6428e4e5bdaea0f9135406",
        1,
    )


def mutate_select_additional_unsafe_builder(text: str) -> str:
    unsafe_builder = (
        "from golang:1.26.0-alpine3.23@sha256:d4c4845f5d60c6a974c6000ce58ae079328d03ab7f721a0734277e69905473e5 AS unsafe\n\n"
    )
    return text.replace("FROM alpine:", unsafe_builder + "FROM alpine:", 1).replace(
        "COPY --from=build ",
        "COPY --from=unsafe ",
        1,
    )


def mutate_add_platform_unsafe_builder(text: str) -> str:
    unsafe_builder = (
        "FROM --platform=linux/amd64 golang:1.26.0-alpine3.23@sha256:d4c4845f5d60c6a974c6000ce58ae079328d03ab7f721a0734277e69905473e5 AS unsafe\n\n"
    )
    return text.replace("FROM alpine:", unsafe_builder + "FROM alpine:", 1)


def mutate_overwrite_runtime_binary(text: str) -> str:
    return text.replace(
        "COPY --from=build --chown=65532:65532 /out/paperless-ai-ocr /usr/local/bin/paperless-ai-ocr\n",
        "COPY --from=build --chown=65532:65532 /out/paperless-ai-ocr /usr/local/bin/paperless-ai-ocr\n"
        "COPY go.mod /usr/local/bin/paperless-ai-ocr\n",
        1,
    )


def mutate_add_final_runtime_stage(text: str) -> str:
    return text + (
        "\nFROM alpine:3.23.3@sha256:25109184c71bdad752c8312a8623239686a9a2071e8825f20acb8f2198c3f659 AS unsafe-runtime\n"
        "COPY go.mod /usr/local/bin/paperless-ai-ocr\n"
        "ENTRYPOINT [\"/usr/local/bin/paperless-ai-ocr\"]\n"
    )


def mutate_relative_copy_overwrite_runtime_binary(text: str) -> str:
    return text.replace(
        "COPY --from=build --chown=65532:65532 /out/paperless-ai-ocr /usr/local/bin/paperless-ai-ocr\n",
        "COPY --from=build --chown=65532:65532 /out/paperless-ai-ocr /usr/local/bin/paperless-ai-ocr\n"
        "WORKDIR /usr/local/bin\n"
        "COPY go.mod paperless-ai-ocr\n",
        1,
    )


def mutate_glob_remove_runtime_binary(text: str) -> str:
    return text.replace(
        "COPY --from=build --chown=65532:65532 /out/paperless-ai-ocr /usr/local/bin/paperless-ai-ocr\n",
        "COPY --from=build --chown=65532:65532 /out/paperless-ai-ocr /usr/local/bin/paperless-ai-ocr\n"
        "RUN rm -f /usr/local/bin/*\n",
        1,
    )


def mutate_run_overwrite_runtime_binary(text: str) -> str:
    return text.replace(
        "COPY --from=build --chown=65532:65532 /out/paperless-ai-ocr /usr/local/bin/paperless-ai-ocr\n",
        "COPY --from=build --chown=65532:65532 /out/paperless-ai-ocr /usr/local/bin/paperless-ai-ocr\n"
        "RUN printf unsafe > /usr/local/bin/paperless-ai-ocr\n",
        1,
    )


def mutate_json_copy_overwrite_runtime_binary(text: str) -> str:
    return text.replace(
        "COPY --from=build --chown=65532:65532 /out/paperless-ai-ocr /usr/local/bin/paperless-ai-ocr\n",
        "COPY --from=build --chown=65532:65532 /out/paperless-ai-ocr /usr/local/bin/paperless-ai-ocr\n"
        'COPY ["go.mod", "/usr/local/bin/paperless-ai-ocr"]\n',
        1,
    )


def mutate_add_continued_unsafe_builder(text: str) -> str:
    unsafe_builder = (
        "FROM \\\n"
        "    golang:1.26.0-alpine3.23@sha256:d4c4845f5d60c6a974c6000ce58ae079328d03ab7f721a0734277e69905473e5 AS unsafe\n\n"
    )
    return text.replace("FROM alpine:", unsafe_builder + "FROM alpine:", 1)


def mutate_overwrite_build_output(text: str) -> str:
    return text.replace(
        "    ./cmd/paperless-ai-ocr\n\nFROM alpine:",
        "    ./cmd/paperless-ai-ocr\n\n"
        "RUN printf %s unsafe > /out/paperless-ai-ocr\n\n"
        "FROM alpine:",
        1,
    )


def mutate_replace_go_build(text: str) -> str:
    start = text.index("RUN CGO_ENABLED=0 go build \\\n")
    end = text.index("\n\nFROM alpine:", start)
    return text[:start] + "RUN mkdir -p /out && printf %s unsafe > /out/paperless-ai-ocr" + text[end:]


def mutate_copy_overwrite_build_output(text: str) -> str:
    return text.replace(
        "    ./cmd/paperless-ai-ocr\n\nFROM alpine:",
        "    ./cmd/paperless-ai-ocr\n\n"
        "COPY go.mod /out/paperless-ai-ocr\n\n"
        "FROM alpine:",
        1,
    )


def mutate_entrypoint_to_healthcheck(text: str) -> str:
    return text.replace(
        'ENTRYPOINT ["/usr/local/bin/paperless-ai-ocr"]',
        'ENTRYPOINT ["/usr/local/bin/healthcheck"]',
        1,
    )


def mutate_reuse_builder_alias(text: str) -> str:
    fake_builder = (
        "FROM alpine:3.23.3@sha256:25109184c71bdad752c8312a8623239686a9a2071e8825f20acb8f2198c3f659 AS build\n"
        "RUN mkdir -p /out && printf %s unsafe > /out/paperless-ai-ocr\n\n"
    )
    return text.replace("FROM alpine:", fake_builder + "FROM alpine:", 1)


def mutate_setup_go_away_from_go_mod(text: str) -> str:
    return text.replace("          go-version-file: go.mod", "          go-version: '1.26.6'", 1)


def mutate_add_unnamed_setup_go(text: str) -> str:
    return text.replace(
        "    steps:\n",
        "    steps:\n"
        "      -\n"
        "        uses: actions/setup-go@b7ad1dad31e06c5925ef5d2fc7ad053ef454303e # v7.0.0\n"
        "        with:\n"
        "          go-version: '1.26.0'\n",
        1,
    )


def mutate_whitespace_hidden_setup_go(text: str) -> str:
    return text.replace(
        "        uses: actions/setup-go@b7ad1dad31e06c5925ef5d2fc7ad053ef454303e # v7.0.0\n"
        "        with:\n"
        "          go-version-file: go.mod\n",
        "        uses : actions/setup-go@b7ad1dad31e06c5925ef5d2fc7ad053ef454303e # v7.0.0\n"
        "        with:\n"
        "          go-version: '1.26.0'\n",
        1,
    )


def mutate_add_shorthand_whitespace_setup_go(text: str) -> str:
    return text.replace(
        "    steps:\n",
        "    steps:\n"
        "      - uses : actions/setup-go@b7ad1dad31e06c5925ef5d2fc7ad053ef454303e # v7.0.0\n"
        "        with:\n"
        "          go-version: '1.26.0'\n",
        1,
    )


def mutate_quoted_setup_go_key(text: str) -> str:
    return text.replace(
        "        uses: actions/setup-go@b7ad1dad31e06c5925ef5d2fc7ad053ef454303e # v7.0.0\n"
        "        with:\n"
        "          go-version-file: go.mod\n",
        '        "uses" : actions/setup-go@b7ad1dad31e06c5925ef5d2fc7ad053ef454303e # v7.0.0\n'
        "        with:\n"
        "          go-version: '1.26.0'\n",
        1,
    )


def mutate_add_flow_setup_go(text: str) -> str:
    return text.replace(
        "          go-version-file: go.mod\n",
        "          go-version-file: go.mod\n"
        "      - {uses: actions/setup-go@b7ad1dad31e06c5925ef5d2fc7ad053ef454303e, with: {go-version: 1.26.0}}\n",
        1,
    )


def mutate_remove_head_check(text: str) -> str:
    return text.replace(
        'readonly head=$(git rev-parse HEAD)\n'
        'if [[ ${GITHUB_SHA:-} != "$head" ]]; then\n'
        "  printf 'tagged checkout mismatch: HEAD=%s GITHUB_SHA=%s\\n' \"$head\" \"${GITHUB_SHA:-}\" >&2\n"
        "  exit 1\n"
        "fi\n\n",
        "",
        1,
    )


def mutate_bypass_head_check(text: str) -> str:
    return text.replace(
        'if [[ ${GITHUB_SHA:-} != "$head" ]]; then',
        'if false && [[ ${GITHUB_SHA:-} != "$head" ]]; then',
        1,
    )


def release_state_step() -> str:
    return (
        "      - name: Determine release state\n"
        "        id: release-state\n"
        "        env:\n"
        "          IMAGE: ghcr.io/${{ github.repository }}\n"
        "          VERSION: ${{ steps.release-metadata.outputs.version }}\n"
        "          REVISION: ${{ github.sha }}\n"
        "        run: bash scripts/release-state.sh\n"
    )


def mutate_remove_release_state(text: str) -> str:
    return text.replace(release_state_step(), "", 1)


def mutate_relocate_release_state(text: str) -> str:
    step = release_state_step()
    return text.replace(step, "", 1).replace("      - name: Attest image provenance\n", step + "      - name: Attest image provenance\n", 1)


def mutate_comment_release_state(text: str) -> str:
    return text.replace(
        release_state_step(),
        "      # Determine release state\n"
        "      # run: bash scripts/release-state.sh\n",
        1,
    )


def mutate_release_concurrency(text: str) -> str:
    return text.replace("  group: release-container-image", "  group: release-${{ github.ref }}", 1)


def mutate_add_major_minor_tag(text: str) -> str:
    return text.replace(
        "            type=semver,pattern={{version}},value=${{ steps.release-metadata.outputs.version }}\n",
        "            type=semver,pattern={{version}},value=${{ steps.release-metadata.outputs.version }}\n"
        "            type=semver,pattern={{major}}.{{minor}},value=${{ steps.release-metadata.outputs.version }}\n",
        1,
    )


def mutate_unconditional_build(text: str) -> str:
    return text.replace(
        "        if: steps.release-state.outputs.publish == 'true'\n",
        "",
        1,
    )


def final_state_step() -> str:
    return (
        "      - name: Verify final release state\n"
        "        id: final-state\n"
        "        env:\n"
        "          IMAGE: ghcr.io/${{ github.repository }}\n"
        "          VERSION: ${{ steps.release-metadata.outputs.version }}\n"
        "          REVISION: ${{ github.sha }}\n"
        "        run: bash scripts/release-state.sh\n"
    )


def mutate_remove_final_state(text: str) -> str:
    return text.replace(final_state_step(), "", 1)


def mutate_publish_output_to_build(text: str) -> str:
    return text.replace(
        "      digest: ${{ steps.final-digest.outputs.digest }}",
        "      digest: ${{ steps.build.outputs.digest }}",
        1,
    )


def mutate_attestation_to_build(text: str) -> str:
    return text.replace(
        "          subject-digest: ${{ steps.final-digest.outputs.digest }}",
        "          subject-digest: ${{ steps.build.outputs.digest }}",
        1,
    )


def mutate_unconditional_attestation(text: str) -> str:
    return text.replace(
        "      - name: Attest image provenance\n"
        "        id: attest\n"
        "        if: steps.release-state.outputs.publish == 'true'\n",
        "      - name: Attest image provenance\n"
        "        id: attest\n",
        1,
    )


def mutate_remove_manifest_descriptor_annotations(text: str) -> str:
    return text.replace(
        "DOCKER_METADATA_ANNOTATIONS_LEVELS: manifest,manifest-descriptor,index",
        "DOCKER_METADATA_ANNOTATIONS_LEVELS: manifest,index",
        1,
    )


def mutate_summary_to_build(text: str) -> str:
    return text.replace(
        "          DIGEST: ${{ steps.final-digest.outputs.digest }}",
        "          DIGEST: ${{ steps.build.outputs.digest }}",
        1,
    )


def mutate_step_attribute(text: str, step_name: str, attribute: str) -> str:
    return text.replace(
        f"      - name: {step_name}\n",
        f"      - name: {step_name}\n        {attribute}\n",
        1,
    )


def mutate_remove_created_label(text: str) -> str:
    return text.replace(
        "          labels: |\n"
        "            org.opencontainers.image.created=${{ steps.release-metadata.outputs.created }}\n",
        "",
        1,
    )


def mutate_remove_created_annotation(text: str) -> str:
    return text.replace(
        "          annotations: |\n"
        "            org.opencontainers.image.created=${{ steps.release-metadata.outputs.created }}\n",
        "",
        1,
    )


def run_mutant(
    name: str,
    relative_path: str,
    mutate: Callable[[str], str],
    ref: str | None,
    expected_error: str | None = None,
) -> str | None:
    with tempfile.TemporaryDirectory(prefix="workflow-policy-") as temporary:
        root = Path(temporary)
        (root / ".github" / "workflows").mkdir(parents=True)
        (root / "scripts").mkdir()
        for policy_input in POLICY_INPUTS:
            destination = root / policy_input
            destination.parent.mkdir(parents=True, exist_ok=True)
            destination.write_text(
                load_at_ref(policy_input, ref),
                encoding="utf-8",
            )
        if ref is None:
            shutil.copy2(ROOT / "scripts" / "workflow-policy-test.py", root / "scripts")
        else:
            (root / "scripts" / "workflow-policy-test.py").write_text(
                load_at_ref("scripts/workflow-policy-test.py", ref),
                encoding="utf-8",
            )
        path = root / relative_path
        original = path.read_text(encoding="utf-8")
        mutated = mutate(original)
        if mutated == original:
            return f"{name}: mutation did not change {relative_path}"
        path.write_text(mutated, encoding="utf-8")
        result = subprocess.run(
            ["python3", str(root / "scripts" / "workflow-policy-test.py")],
            cwd=root,
            check=False,
            capture_output=True,
            text=True,
        )
        if result.returncode == 0:
            return f"{name}: validator accepted forbidden mutation"
        if expected_error is not None and expected_error not in result.stderr:
            return f"{name}: validator rejected mutation for the wrong reason: {result.stderr.strip()}"
    return None


def run_allowed_unsafe_input_mutant(
    name: str,
    relative_path: str,
    mutate: Callable[[str], str],
    ref: str | None,
) -> str | None:
    with tempfile.TemporaryDirectory(prefix="workflow-policy-") as temporary:
        root = Path(temporary)
        (root / ".github" / "workflows").mkdir(parents=True)
        (root / "scripts").mkdir()
        for policy_input in POLICY_INPUTS:
            destination = root / policy_input
            destination.parent.mkdir(parents=True, exist_ok=True)
            destination.write_text(load_at_ref(policy_input, ref), encoding="utf-8")
        policy = load_at_ref("scripts/workflow-policy-test.py", ref) if ref else (ROOT / "scripts" / "workflow-policy-test.py").read_text(encoding="utf-8")
        (root / "scripts" / "workflow-policy-test.py").write_text(policy, encoding="utf-8")
        path = root / relative_path
        original = path.read_text(encoding="utf-8")
        mutated = mutate(original)
        if mutated == original:
            return f"{name}: mutation did not change {relative_path}"
        path.write_text(mutated, encoding="utf-8")
        result = subprocess.run(
            [
                "python3",
                "-c",
                "import importlib.util, sys; "
                "spec = importlib.util.spec_from_file_location('policy', 'scripts/workflow-policy-test.py'); "
                "policy = importlib.util.module_from_spec(spec); "
                "sys.modules[spec.name] = policy; "
                "spec.loader.exec_module(policy); "
                "policy.assert_no_unsafe_inputs(policy.Workflow('ci.yml'))",
            ],
            cwd=root,
            check=False,
            capture_output=True,
            text=True,
        )
        if result.returncode != 0:
            return f"{name}: validator rejected safe non-key occurrence: {result.stderr.strip()}"
    return None


def main() -> None:
    ref = sys.argv[1] if len(sys.argv) == 2 else None
    if len(sys.argv) > 2:
        raise SystemExit(f"usage: {Path(sys.argv[0]).name} [git-ref]")
    failures = [
        failure
        for failure in (
            run_mutant("broad release trigger", ".github/workflows/release.yml", mutate_release_trigger, ref),
            run_mutant("CI job write permission", ".github/workflows/ci.yml", mutate_ci_job_permission, ref),
            run_mutant("unverified release write job", ".github/workflows/release.yml", mutate_release_extra_job, ref),
            run_mutant("workflow-level write escalation", ".github/workflows/release.yml", mutate_release_workflow_permission, ref),
            run_mutant("required values in unrelated step", ".github/workflows/release.yml", mutate_required_values_in_unrelated_step, ref),
            run_mutant("tag validation only in comment", ".github/workflows/release.yml", mutate_fake_tag_validation, ref),
            run_mutant("tag validation command bypassed", ".github/workflows/release.yml", mutate_bypass_tag_validation, ref),
            run_mutant("tag validation after registry login", ".github/workflows/release.yml", mutate_relocate_tag_validation, ref),
            run_mutant("permissive release tag regex", "scripts/release-metadata.sh", mutate_permissive_tag_regex, ref),
            run_mutant("wall-clock release timestamp", "scripts/release-metadata.sh", mutate_wall_clock_created, ref),
            run_mutant("unpinned CI govulncheck", ".github/workflows/ci.yml", mutate_govulncheck_version, ref),
            run_mutant("unpinned CI actionlint", ".github/workflows/ci.yml", mutate_ci_actionlint_version, ref),
            run_mutant("disabled CI actionlint install", ".github/workflows/ci.yml", lambda text: mutate_step_attribute(text, "Install pinned actionlint", "if: false"), ref),
            run_mutant("allowed CI actionlint install failure", ".github/workflows/ci.yml", lambda text: mutate_step_attribute(text, "Install pinned actionlint", "continue-on-error: true"), ref),
            run_mutant("masked CI actionlint install shell", ".github/workflows/ci.yml", lambda text: mutate_step_attribute(text, "Install pinned actionlint", "shell: bash {0} || true"), ref),
            run_mutant("bypassed CI actionlint", ".github/workflows/ci.yml", mutate_bypass_ci_actionlint, ref),
            run_mutant("disabled CI policy validation", ".github/workflows/ci.yml", lambda text: mutate_step_attribute(text, "Validate workflow policy", "if: false"), ref),
            run_mutant("allowed CI policy validation failure", ".github/workflows/ci.yml", lambda text: mutate_step_attribute(text, "Validate workflow policy", "continue-on-error: true"), ref),
            run_mutant("masked CI policy validation shell", ".github/workflows/ci.yml", lambda text: mutate_step_attribute(text, "Validate workflow policy", "shell: bash {0} || true"), ref),
            run_mutant("missing CI acceptance job", ".github/workflows/ci.yml", mutate_remove_acceptance_job, ref),
            run_mutant("bypassed CI acceptance", ".github/workflows/ci.yml", mutate_bypass_acceptance, ref),
            run_mutant("unpinned markdownlint dependency", "package.json", mutate_markdownlint_version, ref),
            run_mutant("ranged markdown link checker dependency", "package.json", mutate_markdown_link_check_version, ref),
            run_mutant("missing node_modules markdownlint ignore", ".markdownlint-cli2.jsonc", mutate_remove_node_modules_markdownlint_ignore, ref),
            run_mutant("unpinned CI documentation Node.js", ".github/workflows/ci.yml", mutate_documentation_node_version, ref),
            run_mutant("ranged package Node.js", "package.json", mutate_package_node_version, ref),
            run_mutant("old npm lockfile version", "package-lock.json", mutate_lockfile_version, ref),
            run_mutant("missing npm integrity", "package-lock.json", mutate_remove_lock_integrity, ref),
            run_mutant("corrupt npm integrity", "package-lock.json", mutate_corrupt_lock_integrity, ref),
            run_mutant("missing npm ci", ".github/workflows/ci.yml", mutate_remove_npm_ci, ref),
            run_mutant("npm install replaces npm ci", ".github/workflows/ci.yml", mutate_replace_npm_ci, ref),
            run_mutant("npm exec permits install", ".github/workflows/ci.yml", mutate_exec_without_no, ref),
            run_mutant("npx replaces npm exec", ".github/workflows/ci.yml", mutate_exec_to_npx, ref),
            run_mutant("documentation version step env", ".github/workflows/ci.yml", mutate_reintroduce_documentation_version_env, ref),
            run_mutant("workflow npm registry override", ".github/workflows/ci.yml", mutate_workflow_npm_registry, ref),
            run_mutant("workflow escaped npm registry override", ".github/workflows/ci.yml", mutate_workflow_escaped_npm_registry, ref),
            run_mutant("workflow flow-style escaped npm registry override", ".github/workflows/ci.yml", mutate_workflow_flow_escaped_npm_registry, ref, "escape sequences in quoted workflow mapping keys are forbidden"),
            run_mutant("job npm registry override", ".github/workflows/ci.yml", mutate_job_npm_registry, ref),
            run_mutant("job escaped npm registry override", ".github/workflows/ci.yml", mutate_job_escaped_npm_registry, ref),
            run_mutant("job flow-style escaped npm registry override", ".github/workflows/ci.yml", mutate_job_flow_escaped_npm_registry, ref, "escape sequences in quoted workflow mapping keys are forbidden"),
            run_mutant("documentation step npm registry override", ".github/workflows/ci.yml", mutate_documentation_step_npm_registry, ref),
            run_mutant("documentation step escaped npm registry override", ".github/workflows/ci.yml", mutate_documentation_step_escaped_npm_registry, ref),
            run_mutant("documentation step flow-style npm registry override", ".github/workflows/ci.yml", mutate_documentation_step_flow_npm_registry, ref),
            run_mutant("documentation step flow-style escaped npm registry override", ".github/workflows/ci.yml", mutate_documentation_step_flow_escaped_npm_registry, ref, "escape sequences in quoted workflow mapping keys are forbidden"),
            run_mutant("documentation step npm userconfig override", ".github/workflows/ci.yml", mutate_documentation_step_npm_userconfig, ref),
            run_mutant("documentation step npm registry-host replacement", ".github/workflows/ci.yml", mutate_documentation_step_npm_replace_registry_host, ref),
            run_mutant("documentation step npm prefix override", ".github/workflows/ci.yml", mutate_documentation_step_npm_prefix, ref),
            run_mutant("documentation step Node options override", ".github/workflows/ci.yml", mutate_documentation_step_node_options, ref),
            run_mutant("additional disabled markdownlint rule", ".markdownlint.json", mutate_disable_markdownlint_rule, ref),
            run_mutant("broadened markdownlint rule", ".markdownlint.json", mutate_change_markdownlint_rule, ref),
            run_mutant("broad documentation link ignore", ".markdown-link-check.json", mutate_broad_link_ignore, ref),
            run_mutant("removed documentation link check", ".markdown-link-check.json", mutate_remove_link_check, ref),
            run_mutant("ignored documentation link status", ".markdown-link-check.json", mutate_ignore_link_status, ref),
            run_mutant("legacy plan documentation target", ".github/workflows/ci.yml", mutate_add_legacy_documentation_target, ref),
            run_mutant("missing normative documentation target", ".github/workflows/ci.yml", mutate_remove_normative_documentation_target, ref),
            run_mutant("duplicated documentation target", ".github/workflows/ci.yml", mutate_duplicate_documentation_target, ref),
            run_mutant("unpinned release govulncheck", ".github/workflows/release.yml", mutate_govulncheck_version, ref),
            run_mutant("unpinned release actionlint", ".github/workflows/release.yml", mutate_actionlint_version, ref),
            run_mutant("unpinned release golangci", ".github/workflows/release.yml", mutate_golangci_version, ref),
            run_mutant("unpinned QEMU image", ".github/workflows/release.yml", mutate_qemu_image, ref),
            run_mutant("go.mod downgrade", "go.mod", mutate_go_mod_downgrade, ref),
            run_mutant("Docker builder downgrade", "Dockerfile", mutate_builder_downgrade, ref),
            run_mutant("Docker builder version mismatch", "Dockerfile", mutate_builder_version_mismatch, ref),
            run_mutant("Docker builder digest removed", "Dockerfile", mutate_remove_builder_digest, ref),
            run_mutant("Docker builder wrong digest", "Dockerfile", mutate_wrong_builder_digest, ref),
            run_mutant("additional unsafe Docker builder selected", "Dockerfile", mutate_select_additional_unsafe_builder, ref),
            run_mutant("platform-qualified unsafe Docker builder", "Dockerfile", mutate_add_platform_unsafe_builder, ref),
            run_mutant("later runtime binary overwrite", "Dockerfile", mutate_overwrite_runtime_binary, ref),
            run_mutant("later final runtime stage", "Dockerfile", mutate_add_final_runtime_stage, ref),
            run_mutant("relative runtime binary overwrite", "Dockerfile", mutate_relative_copy_overwrite_runtime_binary, ref),
            run_mutant("glob removes runtime binary", "Dockerfile", mutate_glob_remove_runtime_binary, ref),
            run_mutant("RUN overwrites runtime binary", "Dockerfile", mutate_run_overwrite_runtime_binary, ref),
            run_mutant("JSON COPY overwrites runtime binary", "Dockerfile", mutate_json_copy_overwrite_runtime_binary, ref),
            run_mutant("continued unsafe Docker builder", "Dockerfile", mutate_add_continued_unsafe_builder, ref),
            run_mutant("RUN overwrites Go build output", "Dockerfile", mutate_overwrite_build_output, ref),
            run_mutant("fake Go build output", "Dockerfile", mutate_replace_go_build, ref),
            run_mutant("COPY overwrites Go build output", "Dockerfile", mutate_copy_overwrite_build_output, ref),
            run_mutant("alternate runtime entrypoint", "Dockerfile", mutate_entrypoint_to_healthcheck, ref),
            run_mutant("reused Docker builder alias", "Dockerfile", mutate_reuse_builder_alias, ref),
            run_mutant("CI setup-go literal version", ".github/workflows/ci.yml", mutate_setup_go_away_from_go_mod, ref),
            run_mutant("release setup-go literal version", ".github/workflows/release.yml", mutate_setup_go_away_from_go_mod, ref),
            run_mutant("unnamed CI setup-go literal version", ".github/workflows/ci.yml", mutate_add_unnamed_setup_go, ref),
            run_mutant("whitespace-hidden CI setup-go", ".github/workflows/ci.yml", mutate_whitespace_hidden_setup_go, ref),
            run_mutant("shorthand whitespace-hidden CI setup-go", ".github/workflows/ci.yml", mutate_add_shorthand_whitespace_setup_go, ref),
            run_mutant("quoted-key CI setup-go", ".github/workflows/ci.yml", mutate_quoted_setup_go_key, ref),
            run_mutant("flow-style CI setup-go", ".github/workflows/ci.yml", mutate_add_flow_setup_go, ref),
            run_mutant("missing checkout binding", "scripts/release-metadata.sh", mutate_remove_head_check, ref),
            run_mutant("bypassed checkout binding", "scripts/release-metadata.sh", mutate_bypass_head_check, ref),
            run_mutant("per-tag release concurrency", ".github/workflows/release.yml", mutate_release_concurrency, ref),
            run_mutant("mutable major.minor release tag", ".github/workflows/release.yml", mutate_add_major_minor_tag, ref),
            run_mutant("missing release state check", ".github/workflows/release.yml", mutate_remove_release_state, ref),
            run_mutant("release state check after push", ".github/workflows/release.yml", mutate_relocate_release_state, ref),
            run_mutant("release state check only in comment", ".github/workflows/release.yml", mutate_comment_release_state, ref),
            run_mutant("publish validation disabled", ".github/workflows/release.yml", lambda text: mutate_step_attribute(text, "Validate release tag and metadata before publication", "if: false"), ref),
            run_mutant("publish validation allowed to fail", ".github/workflows/release.yml", lambda text: mutate_step_attribute(text, "Validate release tag and metadata before publication", "continue-on-error: true"), ref),
            run_mutant("release state check disabled", ".github/workflows/release.yml", lambda text: mutate_step_attribute(text, "Determine release state", "if: false"), ref),
            run_mutant("release state check allowed to fail", ".github/workflows/release.yml", lambda text: mutate_step_attribute(text, "Determine release state", "continue-on-error: true"), ref),
            run_mutant("unconditional release build", ".github/workflows/release.yml", mutate_unconditional_build, ref),
            run_mutant("missing final registry verification", ".github/workflows/release.yml", mutate_remove_final_state, ref),
            run_mutant("conditional final digest", ".github/workflows/release.yml", lambda text: mutate_step_attribute(text, "Resolve final digest", "if: steps.release-state.outputs.publish == 'true'"), ref),
            run_mutant("publish output bypasses final digest", ".github/workflows/release.yml", mutate_publish_output_to_build, ref),
            run_mutant("attestation bypasses final digest", ".github/workflows/release.yml", mutate_attestation_to_build, ref),
            run_mutant("recovery creates unsupported attestation", ".github/workflows/release.yml", mutate_unconditional_attestation, ref),
            run_mutant("summary bypasses final digest", ".github/workflows/release.yml", mutate_summary_to_build, ref),
            run_mutant("missing platform descriptor annotations", ".github/workflows/release.yml", mutate_remove_manifest_descriptor_annotations, ref),
            run_mutant("missing deterministic created label", ".github/workflows/release.yml", mutate_remove_created_label, ref),
            run_mutant("missing deterministic created annotation", ".github/workflows/release.yml", mutate_remove_created_annotation, ref),
        )
        if failure is not None
    ]
    failures.extend(
        failure
        for failure in (
            run_allowed_unsafe_input_mutant("npm key in workflow comment", ".github/workflows/ci.yml", mutate_npm_key_in_comment, ref),
            run_allowed_unsafe_input_mutant("npm key in command text", ".github/workflows/ci.yml", mutate_npm_key_in_command, ref),
            run_allowed_unsafe_input_mutant("ordinary quoted environment key", ".github/workflows/ci.yml", mutate_ordinary_quoted_env_key, ref),
        )
        if failure is not None
    )
    if failures:
        raise SystemExit("\n".join(failures))
    print("workflow policy mutation tests passed")


if __name__ == "__main__":
    main()
