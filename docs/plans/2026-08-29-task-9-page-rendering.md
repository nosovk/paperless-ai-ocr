# On-Demand PDF Page Rendering Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Render requested PDF page ranges as validated, ordered PNG batches that exist only while a callback is running.

**Architecture:** Add an immutable `Renderer` that invokes `pdftoppm` directly with a clean environment, an internal timeout, and a range limited by `-f` and `-l`. Each render uses an isolated mode-0700 batch directory and a releasable workspace reservation; outputs are validated, renamed deterministically, passed to one batch callback, and removed immediately afterward.

**Tech Stack:** Go 1.25+, standard library process/filesystem APIs, Poppler `pdftoppm`, existing `internal/pdf` workspace and safe-error infrastructure.

---

### Task 1: Add Releasable Workspace Reservations

**Files:**
- Modify: `internal/pdf/workspace.go`
- Modify: `internal/pdf/inspect_test.go`

**Step 1: Write failing tests**

Add tests proving that a reservation:

- consumes the configured byte budget while active;
- blocks an overlapping reservation that would exceed the budget;
- returns its bytes exactly once when released;
- permits a later sequential reservation;
- checks current free space before becoming active;
- is safe when release is called repeatedly.

Keep the existing public `Reserve` behavior tests only if the method remains necessary. Prefer replacing it with an unexported lease-style API because no production caller currently uses cumulative reservations.

**Step 2: Run tests to verify RED**

Run:

```bash
go test -run 'TestWorkspaceReservation' -count=1 ./internal/pdf
```

Expected: FAIL because the releasable reservation API does not exist.

**Step 3: Implement the minimal lease API**

Add an unexported reservation type and methods equivalent to:

```go
type reservation struct {
	workspace *Workspace
	bytes     int64
	released  bool
}

func (workspace *Workspace) reserve(ctx context.Context, bytes int64) (*reservation, error)
func (reservation *reservation) release()
```

Use the workspace mutex for both allocation and release. Preserve overflow, budget, cancellation, closed-workspace, and minimum-free-space checks. Release must never make `workspace.reserved` negative.

**Step 4: Run tests to verify GREEN**

Run:

```bash
go test -run 'TestWorkspaceReservation' -count=1 ./internal/pdf
```

Expected: PASS.

### Task 2: Define Renderer Construction and Input Validation

**Files:**
- Create: `internal/pdf/render.go`
- Create: `internal/pdf/render_test.go`

**Step 1: Write failing tests**

Cover:

- default executable lookup for `pdftoppm`;
- default DPI of 200;
- default timeout of five minutes;
- absolute, regular, executable binary validation;
- rejection of negative DPI or timeout;
- rejection of nil context, nil renderer, nil callback, invalid page ranges, and non-owned source files;
- redaction of executable and document paths from public errors.

Use the intended API:

```go
type RenderOptions struct {
	Executable string
	DPI        int
	Timeout    time.Duration
}

type Page struct {
	Number int
	Path   string
	Size   int64
}

func NewRenderer(options RenderOptions) (*Renderer, error)

func (renderer *Renderer) Render(
	ctx context.Context,
	workspace *Workspace,
	sourceName string,
	firstPage int,
	lastPage int,
	visit func([]Page) error,
) error
```

Page-count upper-bound validation remains the caller's responsibility.

**Step 2: Run tests to verify RED**

Run:

```bash
go test -run 'TestNewRenderer|TestRenderRejects' -count=1 ./internal/pdf
```

Expected: FAIL because Renderer does not exist.

**Step 3: Implement constructor and validation**

Resolve the default executable with `exec.LookPath`, canonicalize it with `filepath.EvalSymlinks`, and require an executable regular file. Store immutable validated configuration. Implement only enough `Render` validation for these tests; do not invoke the command yet.

**Step 4: Run tests to verify GREEN**

Run the same focused command and expect PASS.

### Task 3: Invoke pdftoppm for Only the Requested Range

**Files:**
- Modify: `internal/pdf/render.go`
- Modify: `internal/pdf/render_test.go`

**Step 1: Write failing tests**

Use a fake executable to capture arguments and environment. Prove that Renderer:

- passes `-f FIRST -l LAST -r DPI -png SOURCE PREFIX` without a shell;
- defaults to 200 DPI and honors a configured DPI;
- passes the source path as one argument even when it contains spaces;
- writes under a fresh isolated batch directory whose name does not derive from the document name;
- uses only `LANG=C`, `LC_ALL=C`, and a fixed system `PATH`;
- does not render pages outside the requested range.

The fake command should create minimally signed PNG files so the callback can run.

**Step 2: Run tests to verify RED**

Run:

```bash
go test -run 'TestRenderRequestedRange|TestRenderDPI|TestRenderUsesCleanEnvironment' -count=1 ./internal/pdf
```

Expected: FAIL because command execution is not implemented.

**Step 3: Implement minimal command execution**

Create a unique mode-0700 child directory, acquire a reservation for the workspace byte budget before execution, and defer cleanup plus release. Invoke `pdftoppm` using `exec.CommandContext`, bounded stdout/stderr, `commandWaitDelay`, and `configureProcessTermination` from inspection. Use an opaque command failure that cannot expose command arguments or stderr through formatting or unwrapping.

**Step 4: Run tests to verify GREEN**

Run the focused tests and expect PASS.

### Task 4: Validate and Order Rendered PNG Outputs

**Files:**
- Modify: `internal/pdf/render.go`
- Modify: `internal/pdf/render_test.go`

**Step 1: Write failing tests**

Cover:

- Poppler filename padding variants;
- ordered callback pages for an unordered directory listing;
- deterministic callback names `page-%06d.png` based on absolute page number;
- missing expected output;
- duplicate page numbers represented with different padding;
- unknown page numbers;
- non-PNG extra entries;
- symlinks, directories, and non-regular files;
- empty or truncated signatures;
- invalid PNG signatures;
- per-file and aggregate sizes exceeding the workspace budget.

Only the exact eight-byte PNG signature is required:

```go
var pngSignature = [8]byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}
```

**Step 2: Run tests to verify RED**

Run:

```bash
go test -run 'TestRender(Output|PNG|Byte|Ordering|Filename)' -count=1 ./internal/pdf
```

Expected: FAIL because output validation is incomplete.

**Step 3: Implement minimal validation**

Read only the isolated batch directory. Parse output names as `render-<positive ASCII integer>.png`, independent of zero-padding width. Reject every unrecognized entry. Use `Lstat`, require regular files, sum sizes with overflow protection, read exactly the signature, and reject any file or aggregate exceeding the budget. Rename accepted files to deterministic names, sort by absolute page number, and call the callback once with the complete batch.

**Step 4: Run tests to verify GREEN**

Run the focused tests and expect PASS.

### Task 5: Guarantee Immediate Cleanup and Correct Error Semantics

**Files:**
- Modify: `internal/pdf/render.go`
- Modify: `internal/pdf/render_test.go`

**Step 1: Write failing tests**

Prove that:

- files exist and are readable during the callback;
- all callback paths are gone immediately after successful callback return;
- cleanup occurs after callback errors while preserving `errors.Is` for the callback cause;
- cleanup occurs during panic unwinding;
- partial output is removed after command failure;
- caller cancellation preserves `errors.Is(context.Canceled)` or the caller deadline;
- internal timeout is reported safely without masquerading as the caller's deadline;
- the process group is terminated promptly;
- an inherited pipe held by a detached descendant cannot delay return beyond `WaitDelay`;
- document paths, output paths, stderr, and file-content canaries never appear in the public error chain.

**Step 2: Run tests to verify RED**

Run:

```bash
go test -run 'TestRender(Cleanup|Callback|Partial|Caller|Internal|Redact)' -count=1 ./internal/pdf
```

Expected: FAIL for missing cleanup or error behavior.

**Step 3: Implement minimal lifecycle and errors**

Install cleanup defers immediately after creating the batch directory and reservation. If callback returns an error, wrap it in a rendering-category error while preserving its cause. If cleanup alone fails, return a safe cleanup error. During panic unwinding, cleanup must run and the panic must continue unchanged.

**Step 4: Run tests to verify GREEN**

Run the focused tests and expect PASS.

### Task 6: Add Real Poppler Integration Coverage

**Files:**
- Modify: `internal/pdf/render_test.go`

**Step 1: Write integration tests**

Using `/usr/bin/pdftoppm` and existing public fixtures, prove that:

- `one-page.pdf` renders page 1 as one valid PNG;
- `multi-page.pdf` renders only pages 2 through 3, in order;
- deterministic callback filenames are independent of Poppler's native naming;
- images disappear immediately after callback completion;
- the same workspace can render multiple sequential batches because reservations are released.

Skip only when the explicit Poppler executable is unavailable; otherwise failures are real test failures.

**Step 2: Run integration tests**

Run:

```bash
go test -run 'TestRenderRealPDF' -count=1 ./internal/pdf
```

Expected: PASS with Poppler installed.

### Task 7: Refactor and Verify

**Files:**
- Modify: `internal/pdf/render.go`
- Modify: `internal/pdf/render_test.go`
- Modify: `internal/pdf/workspace.go`
- Modify: `internal/pdf/inspect_test.go`

**Step 1: Refactor while green**

Remove duplication shared with `inspect.go` only where it improves clarity. Keep command execution, output parsing, and cleanup straightforward; do not introduce a generic command framework or stronger PNG decoding beyond the specification.

**Step 2: Run focused stress verification**

```bash
go test -count=50 -race ./internal/pdf
```

Expected: PASS.

**Step 3: Run repository verification**

```bash
go test -count=1 -race ./...
go vet ./...
git diff --check
```

Expected: all commands exit successfully.

**Step 4: Run cross-compilation checks**

Compile the affected package tests for both deployment architectures without executing them:

```bash
GOOS=linux GOARCH=amd64 go test -c -o /tmp/paperless-ai-ocr-pdf-amd64.test ./internal/pdf
GOOS=linux GOARCH=arm64 go test -c -o /tmp/paperless-ai-ocr-pdf-arm64.test ./internal/pdf
```

Expected: both commands exit successfully.

### Task 8: Independent Reviews and Final Commit

**Files:**
- Review all Task 9 changes against `docs/plans/2026-08-29-paperless-ai-ocr-implementation.md:136-147`
- Review all Task 9 changes against this plan

**Step 1: Run spec compliance review**

Require an independent reviewer to return `SPEC_COMPLIANT`. Fix every missing requirement or unjustified extra and repeat review until compliant.

**Step 2: Run code quality review**

After spec compliance, require an independent reviewer to return `APPROVED`. Review race safety, reservation lifecycle, path redaction, process termination, filename parsing, cleanup on every exit path, and test quality. Fix findings and repeat review until approved.

**Step 3: Re-run all final verification commands**

Do not rely on earlier or subagent-reported output. Run every command from Task 7 again and inspect the complete results.

**Step 4: Commit without amending**

```bash
git add docs/plans/2026-08-29-task-9-page-rendering.md internal/pdf/workspace.go internal/pdf/inspect_test.go internal/pdf/render.go internal/pdf/render_test.go
git commit -m "feat: render PDF page batches"
```

**Step 5: Push the approved commit**

```bash
git push origin feature/initial-implementation
```
