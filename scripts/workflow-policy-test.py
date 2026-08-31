#!/usr/bin/env python3
"""Validate the structure and security policy of controlled workflows."""

from __future__ import annotations

import json
import re
import shlex
from dataclasses import dataclass
from pathlib import Path


ROOT = Path(__file__).resolve().parent.parent
SHA = r"[0-9a-f]{40}"
ALLOWED_ACTIONS = {
    "actions/checkout": ("3d3c42e5aac5ba805825da76410c181273ba90b1", "v7.0.1"),
    "actions/setup-go": ("b7ad1dad31e06c5925ef5d2fc7ad053ef454303e", "v7.0.0"),
    "actions/setup-node": ("820762786026740c76f36085b0efc47a31fe5020", "v7.0.0"),
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
SAFE_GO_VERSION = "1.26.6"
GO_BUILDER = "golang:1.26.6-alpine3.23@sha256:e57c41c1d5864341031181b0db34b9a537bb5773eb6428e4e5bdaea0f9135406"
DOCUMENTATION_NODE_VERSION = "24.8.0"
DOCUMENTATION_DEPENDENCIES = {
    "markdown-link-check": "3.15.0",
    "markdownlint-cli2": "0.23.2",
}
MARKDOWNLINT_CONFIG = {
    "MD013": False,
    "MD024": {"siblings_only": True},
}
MARKDOWN_LINK_CHECK_CONFIG = {
    "timeout": "20s",
    "retryOn429": True,
    "retryCount": 2,
    "fallbackRetryDelay": "10s",
    "ignorePatterns": [
        {
            "pattern": "^https://github.com/nosovk/paperless-ai-ocr/security/advisories/new$",
        }
    ],
}


def require(condition: bool, message: str) -> None:
    if not condition:
        raise AssertionError(message)


def indentation(line: str) -> int:
    return len(line) - len(line.lstrip(" "))


def uncommented(lines: list[str]) -> str:
    return "\n".join(line for line in lines if not line.lstrip().startswith("#"))


def normalize_mapping_key(line: str) -> str:
    match = re.fullmatch(
        r'(\s*(?:-\s+)?)(?:"((?:\\.|[^"])*)"|\'([^\']*)\'|([A-Za-z0-9_-]+))\s*:(.*)',
        line,
    )
    if match is None:
        return line
    key = next(group for group in match.groups()[1:4] if group is not None)
    require("\\" not in key, "escape sequences in quoted workflow mapping keys are forbidden")
    return f"{match.group(1)}{key}:{match.group(5)}"


def normalize_workflow_lines(text: str) -> list[str]:
    lines = []
    block_scalar_indent: int | None = None
    for line in text.splitlines():
        current_indent = indentation(line)
        if block_scalar_indent is not None:
            if not line.strip() or current_indent > block_scalar_indent:
                lines.append(line)
                continue
            block_scalar_indent = None
        normalized = normalize_mapping_key(line)
        lines.append(normalized)
        if re.fullmatch(r"\s*(?:-\s+)?[A-Za-z0-9_-]+\s*:\s*[|>]\s*", normalized):
            block_scalar_indent = current_indent
    return lines


@dataclass(frozen=True)
class DockerInstruction:
    keyword: str
    arguments: str
    raw: str


def docker_instructions(text: str) -> list[DockerInstruction]:
    logical_lines: list[str] = []
    continued: list[str] = []
    for physical_line in text.splitlines():
        stripped = physical_line.strip()
        if not continued and (not stripped or stripped.startswith("#")):
            continue
        if stripped.endswith("\\"):
            continued.append(stripped[:-1].rstrip())
            continue
        if continued:
            continued.append(stripped)
            logical_lines.append(" ".join(continued))
            continued = []
        else:
            logical_lines.append(stripped)
    require(not continued, "Dockerfile must not end with a continued instruction")

    instructions = []
    for raw in logical_lines:
        match = re.fullmatch(r"([A-Za-z]+)\s+(.+)", raw)
        require(match is not None, f"unsupported Dockerfile instruction format: {raw}")
        instructions.append(DockerInstruction(match.group(1).upper(), match.group(2), raw))
    return instructions


def docker_from(arguments: str) -> tuple[str, str | None]:
    tokens = shlex.split(arguments)
    while tokens and tokens[0].startswith("--"):
        tokens.pop(0)
    require(len(tokens) in (1, 3), f"unsupported FROM format: {arguments}")
    if len(tokens) == 1:
        return tokens[0], None
    require(tokens[1].lower() == "as", f"unsupported FROM format: {arguments}")
    return tokens[0], tokens[2]


@dataclass(frozen=True)
class Block:
    header: str
    indent: int
    lines: list[str]

    @property
    def text(self) -> str:
        return uncommented(self.lines)

    def child(self, key: str) -> Block:
        wanted = rf" {{{self.indent + 2}}}{re.escape(key)}\s*:"
        matches = [index for index, line in enumerate(self.lines) if re.fullmatch(wanted, line)]
        require(len(matches) == 1, f"{self.header} must contain exactly one {key!r} block")
        return make_block(self.lines, matches[0], key)

    def children(self) -> dict[str, Block]:
        child_indent = self.indent + 2
        result: dict[str, Block] = {}
        for index, line in enumerate(self.lines):
            match = re.fullmatch(rf" {{{child_indent}}}([A-Za-z0-9_-]+)\s*:", line)
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
            match = re.fullmatch(r"\s*([A-Za-z0-9_-]+)\s*:\s*(.*?)\s*", line)
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
        self.lines = normalize_workflow_lines(self.path.read_text(encoding="utf-8"))

    @property
    def text(self) -> str:
        return uncommented(self.lines)

    def top(self, key: str) -> Block:
        matches = [index for index, line in enumerate(self.lines) if re.fullmatch(rf"{re.escape(key)}\s*:", line)]
        require(len(matches) == 1, f"workflow must contain exactly one top-level {key!r} block")
        return make_block(self.lines, matches[0], key)

    def jobs(self) -> dict[str, Block]:
        return self.top("jobs").children()


def step_blocks(job: Block) -> list[Block]:
    steps = job.child("steps")
    step_indent = steps.indent + 2
    starts = [index for index, line in enumerate(steps.lines) if re.match(rf"^ {{{step_indent}}}-(?:\s|$)", line)]
    require(starts, f"job {job.header!r} has no steps")
    result = []
    for position, start in enumerate(starts):
        end = starts[position + 1] if position + 1 < len(starts) else len(steps.lines)
        require(
            not re.match(rf"^ {{{step_indent}}}-\s*\{{", steps.lines[start]),
            f"job {job.header!r} must not contain flow-style steps",
        )
        match = re.fullmatch(rf" {{{step_indent}}}- name\s*: (.+)", steps.lines[start])
        name = match.group(1).strip() if match else f"unnamed step {position + 1}"
        lines = steps.lines[start:end]
        shorthand = re.fullmatch(rf"( {{{step_indent}}})-\s+(.+)", lines[0])
        if shorthand and not match:
            lines = [f"{shorthand.group(1)}-", f"{shorthand.group(1)}  {shorthand.group(2)}", *lines[1:]]
        result.append(Block(name, step_indent, lines))
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


def block_scalar_commands(step: Block, key: str) -> tuple[str, ...]:
    attribute_indent = step.indent + 2
    matches = [
        index
        for index, line in enumerate(step.lines)
        if re.fullmatch(rf" {{{attribute_indent}}}{re.escape(key)}\s*:\s*\|\s*", line)
    ]
    require(len(matches) == 1, f"{step.header} must contain exactly one block {key!r}")
    commands = []
    for line in step.lines[matches[0] + 1 :]:
        if line.strip() and indentation(line) <= attribute_indent:
            break
        if line.strip():
            commands.append(line.strip())
    require(commands, f"{step.header} block {key!r} must not be empty")
    return tuple(commands)


def assert_pinned_actions(workflow: Workflow) -> None:
    uses = re.findall(r"(?m)^\s+uses\s*:\s*([^\s#]+)\s+#\s+(v[^\s]+)\s*$", workflow.text)
    require(uses, f"{workflow.path.name} has no actions")
    use_keys = re.findall(r"(?m)^\s+uses\s*:", workflow.text)
    require(len(use_keys) == len(uses), f"every action in {workflow.path.name} must have a release comment")
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
    for name in ("MARKDOWNLINT_VERSION", "MARKDOWN_LINK_CHECK_VERSION"):
        require(name not in text, f"documentation version override is forbidden: {name}")

    block_scalar_indent: int | None = None
    for line in workflow.lines:
        if not line.strip() or line.lstrip().startswith("#"):
            continue
        current_indent = indentation(line)
        if block_scalar_indent is not None:
            if current_indent > block_scalar_indent:
                continue
            block_scalar_indent = None
        if re.fullmatch(r"\s*(?:-\s+)?[A-Za-z0-9_-]+\s*:\s*[|>]\s*", line):
            block_scalar_indent = current_indent
            continue
        flow_env = re.fullmatch(r"\s*(?:-\s+)?env\s*:\s*\{(.*)\}\s*", line)
        if flow_env is not None:
            for key in re.findall(r"(?:^|,)\s*(?:\"([^\"]+)\"|'([^']+)'|([A-Za-z0-9_-]+))\s*:", flow_env.group(1)):
                key_source = next(part for part in key if part)
                require("\\" not in key_source, "escape sequences in quoted workflow mapping keys are forbidden")
                normalized_key = key_source.upper()
                require(
                    not normalized_key.startswith("NPM_CONFIG_")
                    and normalized_key != "NODE_OPTIONS",
                    f"protected workflow environment key is forbidden: {normalized_key}",
                )
            continue
        match = re.fullmatch(r"\s*(?:-\s+)?([A-Za-z0-9_-]+)\s*:.*", line)
        if match is None:
            continue
        key = match.group(1).upper()
        require(
            not key.startswith("NPM_CONFIG_") and key != "NODE_OPTIONS",
            f"protected workflow environment key is forbidden: {key}",
        )


def load_json(relative_path: str) -> dict[str, object]:
    path = ROOT / relative_path
    require(path.is_file(), f"missing {relative_path}")
    try:
        value = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as err:
        raise AssertionError(f"invalid {relative_path}") from err
    require(isinstance(value, dict), f"{relative_path} must contain a JSON object")
    return value


def assert_documentation_packages() -> None:
    require(
        load_json(".markdownlint.json") == MARKDOWNLINT_CONFIG,
        ".markdownlint.json rules must be exact",
    )
    require(
        load_json(".markdown-link-check.json") == MARKDOWN_LINK_CHECK_CONFIG,
        ".markdown-link-check.json policy must be exact",
    )
    require(
        load_json(".markdownlint-cli2.jsonc")
        == {"ignores": ["docs/plans/**", "node_modules/**"]},
        ".markdownlint-cli2.jsonc ignores must be exact",
    )

    package = load_json("package.json")
    require(
        package
        == {
            "name": "paperless-ai-ocr",
            "private": True,
            "engines": {"node": DOCUMENTATION_NODE_VERSION},
            "devDependencies": DOCUMENTATION_DEPENDENCIES,
        },
        "package.json documentation tool policy is invalid",
    )

    lock = load_json("package-lock.json")
    require(lock.get("name") == "paperless-ai-ocr", "package-lock.json name must match package.json")
    require(lock.get("lockfileVersion") == 3, "package-lock.json must use lockfileVersion 3")
    require(lock.get("requires") is True, "package-lock.json must require dependency resolution")
    packages = lock.get("packages")
    require(isinstance(packages, dict), "package-lock.json packages must be an object")
    root = packages.get("")
    require(
        root
        == {
            "name": "paperless-ai-ocr",
            "devDependencies": DOCUMENTATION_DEPENDENCIES,
            "engines": {"node": DOCUMENTATION_NODE_VERSION},
        },
        "package-lock.json root metadata must match package.json",
    )
    require(len(packages) > 1, "package-lock.json must contain resolved dependencies")
    for package_path, metadata in packages.items():
        if package_path == "":
            continue
        require(package_path.startswith("node_modules/"), f"unexpected lockfile package path: {package_path}")
        require(isinstance(metadata, dict), f"lockfile package metadata must be an object: {package_path}")
        require(metadata.get("link") is not True, f"linked lockfile packages are not allowed: {package_path}")
        version = metadata.get("version")
        resolved = metadata.get("resolved")
        integrity = metadata.get("integrity")
        require(isinstance(version, str) and re.fullmatch(r"\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?", version) is not None, f"lockfile package version must be exact: {package_path}")
        require(isinstance(resolved, str) and re.fullmatch(r"https://registry\.npmjs\.org/.+\.tgz", resolved) is not None, f"lockfile package must use the npm registry: {package_path}")
        require(isinstance(integrity, str) and re.fullmatch(r"sha512-[A-Za-z0-9+/]+={0,2}", integrity) is not None, f"lockfile package integrity must be sha512: {package_path}")


def assert_go_toolchain(workflows: tuple[Workflow, ...]) -> None:
    go_mod = (ROOT / "go.mod").read_text(encoding="utf-8")
    versions = re.findall(r"(?m)^go (\S+)$", go_mod)
    require(versions == [SAFE_GO_VERSION], f"go.mod must require exact Go {SAFE_GO_VERSION}")

    instructions = docker_instructions((ROOT / "Dockerfile").read_text(encoding="utf-8"))
    stage_starts = [
        (index, *docker_from(instruction.arguments))
        for index, instruction in enumerate(instructions)
        if instruction.keyword == "FROM"
    ]
    require(stage_starts, "Dockerfile must contain stages")
    aliases = [alias.casefold() for _, _, alias in stage_starts if alias]
    require(len(aliases) == len(set(aliases)), "Dockerfile stage aliases must be unique")
    builders = [(image, alias) for _, image, alias in stage_starts if image.lower().startswith("golang:")]
    require(len(builders) == 1, "Dockerfile must contain exactly one Go builder stage")
    builder, builder_alias = builders[0]
    require(builder == GO_BUILDER, f"Dockerfile builder must be exactly {GO_BUILDER}")
    builder_version = re.fullmatch(r"golang:(\d+\.\d+\.\d+)-[^\s]+", builder)
    require(builder_version is not None, "Dockerfile Go builder version must be exact")
    require(builder_version.group(1) == versions[0], "Dockerfile builder Go version must match go.mod")
    require(builder_alias, "Dockerfile Go builder stage must have an alias")

    builder_start = next(index for index, image, alias in stage_starts if image == builder and alias == builder_alias)
    builder_end = next((index for index, _, _ in stage_starts if index > builder_start), len(instructions))
    builder_stage = instructions[builder_start + 1 : builder_end]
    output_path = "/out/paperless-ai-ocr"
    output_instructions = [instruction for instruction in builder_stage if output_path in instruction.raw]
    require(len(output_instructions) == 1, "Go builder stage must contain exactly one instruction mentioning the build output")
    approved_build = output_instructions[0]
    expected_build = (
        "RUN CGO_ENABLED=0 go build -trimpath "
        '-ldflags="-s -w -X github.com/nosovk/paperless-ai-ocr/internal/buildinfo.version=${VERSION} '
        "-X github.com/nosovk/paperless-ai-ocr/internal/buildinfo.revision=${REVISION} "
        '-X github.com/nosovk/paperless-ai-ocr/internal/buildinfo.buildTime=${CREATED}" '
        "-o /out/paperless-ai-ocr ./cmd/paperless-ai-ocr"
    )
    require(approved_build.raw == expected_build, "Go builder output must come from the approved static go build command")
    require(
        builder_stage[-1] == approved_build,
        "approved go build must be the final instruction in the Go builder stage",
    )

    protected_path = "/usr/local/bin/paperless-ai-ocr"
    final_stage = instructions[stage_starts[-1][0] + 1 :]
    protected = [instruction for instruction in final_stage if instruction.keyword in {"RUN", "ADD", "COPY"} and protected_path in instruction.raw]
    require(len(protected) == 1, "final image must contain exactly one mutating instruction mentioning the protected binary path")
    approved_copy = protected[0]
    require(approved_copy.keyword == "COPY", "protected binary path may only be written by the approved COPY")
    require(not approved_copy.arguments.lstrip().startswith("["), "protected binary COPY must use controlled shell form")
    copy_tokens = shlex.split(approved_copy.arguments)
    copy_from = [token.removeprefix("--from=") for token in copy_tokens if token.startswith("--from=")]
    copy_paths = [token for token in copy_tokens if not token.startswith("--")]
    require(
        copy_from == [builder_alias]
        and copy_paths == ["/out/paperless-ai-ocr", protected_path],
        "final image binary must be copied once from the approved Go builder stage",
    )
    copy_index = final_stage.index(approved_copy)
    allowed_after_copy = {"USER", "EXPOSE", "HEALTHCHECK", "ENTRYPOINT"}
    for instruction in final_stage[copy_index + 1 :]:
        require(
            instruction.keyword in allowed_after_copy,
            f"unsupported instruction after approved binary COPY: {instruction.keyword}",
        )
    entrypoints = [instruction for instruction in final_stage if instruction.keyword == "ENTRYPOINT"]
    require(
        len(entrypoints) == 1 and entrypoints[0].arguments == '["/usr/local/bin/paperless-ai-ocr"]',
        'final image must contain exactly ENTRYPOINT ["/usr/local/bin/paperless-ai-ocr"]',
    )

    setup_go_steps = []
    for workflow in workflows:
        for job in workflow.jobs().values():
            for step in step_blocks(job):
                if step.values().get("uses", "").startswith("actions/setup-go@"):
                    setup_go_steps.append(step)
    require(setup_go_steps, "controlled workflows must use actions/setup-go")
    for step in setup_go_steps:
        inputs = step.child("with").values()
        require(inputs.get("go-version-file") == "go.mod", "setup-go must use go-version-file: go.mod")
        require("go-version" not in inputs, "setup-go literal go-version is forbidden")


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
    require(
        workflow.top("env").values() == {"GOVULNCHECK_VERSION": "v1.7.0"},
        "CI tool versions must be exactly pinned",
    )
    jobs = workflow.jobs()
    require(jobs.keys() == {"test", "lint", "race", "acceptance", "vulnerability-scan", "docker-build"}, "unexpected CI job set")
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
    documentation = test_steps["Check documentation"]
    setup_node = test_steps["Set up Node.js for documentation checks"]
    require("actions/setup-node@820762786026740c76f36085b0efc47a31fe5020 # v7.0.0" in setup_node.text, "CI documentation checks must use the approved setup-node action")
    require(setup_node.child("with").values() == {"node-version": DOCUMENTATION_NODE_VERSION, "package-manager-cache": "false"}, "CI documentation Node.js version and cache policy must be exact")
    require(
        block_scalar_commands(documentation, "run")
        == (
            "npm ci --ignore-scripts --no-audit --no-fund",
            'npm exec --no -- markdownlint-cli2 "**/*.md"',
            "npm exec --no -- markdown-link-check --config .markdown-link-check.json --quiet README.md SECURITY.md CONTRIBUTING.md docs/configuration.md docs/architecture.md docs/operations.md docs/threat-model.md",
        ),
        "CI documentation commands and targets must be exact",
    )
    require("if" not in documentation.values(), "documentation checks must not be conditional")
    require("continue-on-error" not in documentation.values(), "documentation checks must fail closed")
    require("gofmt -l" in test_steps["Check formatting"].text, "CI formatting step must run gofmt")
    require("go vet ./..." in test_steps["Vet"].text, "CI vet step missing")
    require("go test ./..." in test_steps["Unit tests"].text, "CI unit test step missing")
    require("TestInspectRealPDFs|TestRenderRealPDFOnePage|TestRenderRealPDFRangeAndSequentialReuse" in test_steps["Run real Poppler tests explicitly"].text, "CI explicit Poppler tests missing")
    require("go test -race ./..." in named_steps(jobs["race"])["Run race tests"].text, "CI race tests missing")
    acceptance_steps = named_steps(jobs["acceptance"])
    require("poppler-utils" in acceptance_steps["Install Poppler"].text, "CI acceptance job must install Poppler")
    assert_required_step(
        acceptance_steps["Run acceptance tests"],
        "bash scripts/acceptance.sh",
        "CI acceptance tests must use the controlled script",
    )
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
    require(
        "DOCKER_METADATA_ANNOTATIONS_LEVELS: manifest,manifest-descriptor,index" in metadata,
        "multi-arch annotations must cover manifests, platform descriptors, and index",
    )
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
    final_state_name = "Verify final release state"
    final_state = publish_steps[final_state_name]
    assert_required_step(final_state, "bash scripts/release-state.sh", "publish must re-inspect the final registry state")
    require(final_state.values().get("id") == "final-state", "final registry state step must expose outputs")
    require(
        final_state.child("env").values()
        == {
            "IMAGE": "ghcr.io/${{ github.repository }}",
            "VERSION": "${{ steps.release-metadata.outputs.version }}",
            "REVISION": "${{ github.sha }}",
        },
        "final registry state inputs are invalid",
    )
    final_digest = publish_steps["Resolve final digest"]
    require("if" not in final_digest.values(), "final digest resolution must always run after release state succeeds")
    require(final_digest.values().get("id") == "final-digest", "final digest step must expose its verified digest")
    for value in (
        "PUBLISH: ${{ steps.release-state.outputs.publish }}",
        "EXISTING_DIGEST: ${{ steps.release-state.outputs.digest }}",
        "BUILT_DIGEST: ${{ steps.build.outputs.digest }}",
        "REGISTRY_DIGEST: ${{ steps.final-state.outputs.digest }}",
        "FINAL_PUBLISH: ${{ steps.final-state.outputs.publish }}",
        "digest=$REGISTRY_DIGEST",
        "printf 'digest=%s\\n' \"$digest\" >> \"$GITHUB_OUTPUT\"",
        "^sha256:[0-9a-f]{64}$",
    ):
        require(value in final_digest.text, f"final digest resolution missing: {value}")
    require(names.index("Build and push image") < names.index(final_state_name) < names.index("Resolve final digest") < names.index("Attest image provenance"), "final registry state and digest must be verified after the optional build and before attestation")
    require(publish.child("outputs").values() == {"digest": "${{ steps.final-digest.outputs.digest }}"}, "publish output must use the verified final digest")
    attest_step = publish_steps["Attest image provenance"]
    require(
        "        if: steps.release-state.outputs.publish == 'true'" in attest_step.lines,
        "provenance attestation must only describe an image built by this workflow run",
    )
    attest = attest_step.text
    for value in ("subject-name: ghcr.io/${{ github.repository }}", "subject-digest: ${{ steps.final-digest.outputs.digest }}", "push-to-registry: true", "create-storage-record: false"):
        require(value in attest, f"attestation missing: {value}")
    summary = publish_steps["Summarize release"].text
    require("DIGEST: ${{ steps.final-digest.outputs.digest }}" in summary, "release summary must use the verified final digest")
    require("PUBLISH: ${{ steps.release-state.outputs.publish }}" in summary, "release summary must distinguish publication from recovery")
    require("GITHUB_STEP_SUMMARY" in summary and "Package visibility is not controlled by this workflow" in summary, "release summary is incomplete")


def main() -> None:
    ci = Workflow("ci.yml")
    release = Workflow("release.yml")
    for workflow in (ci, release):
        assert_pinned_actions(workflow)
        assert_no_unsafe_inputs(workflow)
    assert_go_toolchain((ci, release))
    assert_documentation_packages()
    assert_release_metadata_script()
    assert_ci(ci)
    assert_release(release)
    print("workflow policy validation passed")


if __name__ == "__main__":
    main()
