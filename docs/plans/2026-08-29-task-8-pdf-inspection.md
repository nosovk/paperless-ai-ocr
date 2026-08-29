# Temporary Workspace And PDF Inspection Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Add protected per-job temporary workspaces and safely determine PDF page counts with Poppler `pdfinfo`.

**Architecture:** The `internal/pdf` package owns a mode-0700 directory created below `os.TempDir()`, validates every child filename, tracks reserved temporary bytes, checks filesystem availability, and removes the complete directory on close. A configurable inspector resolves an absolute `pdfinfo` executable once, executes it directly without a shell under a clean environment and bounded timeout, captures bounded stderr for internal diagnosis without exposing it publicly, and strictly parses one positive `Pages` value.

**Tech Stack:** Go standard library, `golang.org/x/sys/unix` filesystem statistics, Poppler `pdfinfo`, table-driven tests, synthetic public PDF fixtures.

---

### Task 1: Protected Per-Job Workspace

**Files:**
- Create: `internal/pdf/workspace.go`
- Create: `internal/pdf/inspect_test.go`

**Step 1: Write failing workspace tests**

Add tests that require:

- default creation below `os.TempDir()`;
- an optional test-only/root option while production defaults remain unchanged;
- mode `0700` even under a permissive umask;
- unique directories for repeated job IDs;
- rejection of blank names, absolute names, separators, `.` and `..`;
- positive byte budget and free-space reserve validation;
- cumulative reservations cannot exceed the configured budget;
- free-space failures occur before external work starts;
- `Close` recursively removes files and directories;
- repeated `Close` is safe;
- canceled setup leaves no workspace behind.

Use a narrow filesystem-space function injection in unexported options so tests do not depend on host disk fullness.

**Step 2: Verify RED**

Run:

```bash
go test -run '^TestWorkspace' -count=1 ./internal/pdf
```

Expected: FAIL because `internal/pdf` workspace APIs do not exist.

**Step 3: Implement the minimal workspace**

Implement a `Workspace` with:

```go
type WorkspaceOptions struct {
    TemporaryByteBudget int64
    MinimumFreeBytes    int64
}

func NewWorkspace(ctx context.Context, jobID int64, options WorkspaceOptions) (*Workspace, error)
func (workspace *Workspace) Path(name string) (string, error)
func (workspace *Workspace) Reserve(ctx context.Context, bytes int64) error
func (workspace *Workspace) Close() error
```

Requirements:

- validate positive job ID and budget, nonnegative free-space reserve, and active context;
- use `os.MkdirTemp(os.TempDir(), "paperless-ai-ocr-<job>-*")` then enforce `0700`;
- never include document-derived content in directory or public error text;
- use lexical filename validation, not prefix-only path checks;
- prevent reservation integer overflow;
- query available bytes with `unix.Statfs` and reject when requested bytes plus `MinimumFreeBytes` cannot fit;
- remove a partially-created workspace on every constructor error;
- map failures to `saferr.CategoryRendering`.

The implementation may use unexported constructor/root/statfs hooks for deterministic tests, but must not add application configuration for the root directory.

**Step 4: Verify GREEN**

Run:

```bash
go test -run '^TestWorkspace' -count=10 -race ./internal/pdf
```

Expected: PASS with no race reports.

### Task 2: Safe PDF Inspection

**Files:**
- Create: `internal/pdf/inspect.go`
- Modify: `internal/pdf/inspect_test.go`
- Create: `testdata/pdfs/one-page.pdf`
- Create: `testdata/pdfs/multi-page.pdf`
- Create: `testdata/pdfs/malformed.pdf`

**Step 1: Add synthetic fixtures**

Create small source-control-safe PDFs containing no user data. Verify with real Poppler that the valid fixtures contain exactly one and three pages. Keep a deliberately malformed non-PDF fixture for rejection tests.

**Step 2: Write failing inspector tests**

Cover:

- one-page and multi-page count through real `pdfinfo`;
- malformed PDF rejection;
- canceled context before execution;
- command timeout and process termination;
- absolute executable requirement and executable lookup failure;
- direct argv handling for a safe workspace filename containing spaces;
- clean child environment verified by a fake executable;
- bounded stderr from a noisy fake executable;
- strict rejection of missing, duplicate, zero, negative, signed, decimal, overflowing, and trailing-junk `Pages` values;
- rejection of paths not produced by the workspace;
- public errors omit workspace paths, filenames, PDF content, and captured stderr.

**Step 3: Verify RED**

Run:

```bash
go test -run '^TestInspect' -count=1 ./internal/pdf
```

Expected: FAIL because inspector behavior is missing.

**Step 4: Implement direct `pdfinfo` execution**

Implement:

```go
type InspectOptions struct {
    Executable string
    Timeout    time.Duration
}

type Info struct {
    Pages int
}

func NewInspector(options InspectOptions) (*Inspector, error)
func (inspector *Inspector) Inspect(ctx context.Context, workspace *Workspace, name string) (Info, error)
```

Requirements:

- default executable is resolved once with `exec.LookPath("pdfinfo")` and stored as an absolute path;
- custom executable must resolve to an absolute regular executable file;
- positive timeout is mandatory after defaults are applied;
- call `exec.CommandContext` directly with argv, never a shell;
- set `Cmd.Env` to an explicit minimal environment that does not inherit credentials;
- set a bounded stderr writer and discard stdout beyond the bounded metadata limit;
- distinguish caller cancellation/deadline from inspector timeout while preserving `errors.Is` for context errors;
- parse exactly one `Pages:` line with ASCII decimal digits only, positive and within `int` range;
- return only sanitized `saferr.CategoryRendering` messages.

**Step 5: Verify GREEN**

Run:

```bash
go test -run '^(TestWorkspace|TestInspect)' -count=10 -race ./internal/pdf
```

Expected: PASS with real Poppler integration and fake-command edge cases.

### Task 3: Task 8 Verification And Review

**Files:**
- Review: `internal/pdf/workspace.go`
- Review: `internal/pdf/inspect.go`
- Review: `internal/pdf/inspect_test.go`
- Review: `testdata/pdfs/`

**Step 1: Run focused stress tests**

```bash
go test -count=50 -race ./internal/pdf
```

Expected: PASS.

**Step 2: Run repository verification**

```bash
go test -count=1 -race ./...
go vet ./...
git diff --check
```

Expected: all commands succeed.

**Step 3: Review against Task 8 specification**

Confirm page counting, malformed input, timeout, path safety, cleanup, free-space checks, temporary byte budget, no-shell execution, clean environment, bounded stderr, strict parsing, mode `0700`, and cancellation cleanup.

**Step 4: Commit**

```bash
git add docs/plans/2026-08-29-task-8-pdf-inspection.md internal/pdf testdata/pdfs
git commit -m "feat: inspect PDF documents safely"
```
