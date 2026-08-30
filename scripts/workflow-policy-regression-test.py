#!/usr/bin/env python3
"""Prove workflow policy validation rejects security-sensitive mutations."""

from __future__ import annotations

import shutil
import subprocess
import sys
import tempfile
from collections.abc import Callable
from pathlib import Path


ROOT = Path(__file__).resolve().parent.parent
POLICY_INPUTS = (
    ".github/workflows/ci.yml",
    ".github/workflows/release.yml",
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


def mutate_actionlint_version(text: str) -> str:
    return text.replace("ACTIONLINT_VERSION: v1.7.12", "ACTIONLINT_VERSION: latest", 1)


def mutate_golangci_version(text: str) -> str:
    return text.replace("          version: v2.13.2", "          version: latest", 1)


def mutate_qemu_image(text: str) -> str:
    return text.replace(
        "QEMU_IMAGE: docker.io/tonistiigi/binfmt@sha256:400a4873b838d1b89194d982c45e5fb3cda4593fbfd7e08a02e76b03b21166f0",
        "QEMU_IMAGE: docker.io/tonistiigi/binfmt:latest",
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
            run_mutant("unpinned release govulncheck", ".github/workflows/release.yml", mutate_govulncheck_version, ref),
            run_mutant("unpinned release actionlint", ".github/workflows/release.yml", mutate_actionlint_version, ref),
            run_mutant("unpinned release golangci", ".github/workflows/release.yml", mutate_golangci_version, ref),
            run_mutant("unpinned QEMU image", ".github/workflows/release.yml", mutate_qemu_image, ref),
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
            run_mutant("conditional final digest", ".github/workflows/release.yml", lambda text: mutate_step_attribute(text, "Resolve final digest", "if: steps.release-state.outputs.publish == 'true'"), ref),
            run_mutant("publish output bypasses final digest", ".github/workflows/release.yml", mutate_publish_output_to_build, ref),
            run_mutant("attestation bypasses final digest", ".github/workflows/release.yml", mutate_attestation_to_build, ref),
            run_mutant("summary bypasses final digest", ".github/workflows/release.yml", mutate_summary_to_build, ref),
            run_mutant("missing deterministic created label", ".github/workflows/release.yml", mutate_remove_created_label, ref),
            run_mutant("missing deterministic created annotation", ".github/workflows/release.yml", mutate_remove_created_annotation, ref),
        )
        if failure is not None
    ]
    if failures:
        raise SystemExit("\n".join(failures))
    print("workflow policy mutation tests passed")


if __name__ == "__main__":
    main()
