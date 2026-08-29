package pdf

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/nosovk/paperless-ai-ocr/internal/saferr"
)

const testByteBudget = int64(1 << 30)

func TestWorkspaceDefaultRoot(t *testing.T) {
	workspace := newTestWorkspace(t, context.Background(), 1, WorkspaceOptions{
		TemporaryByteBudget: testByteBudget,
	})

	relative, err := filepath.Rel(os.TempDir(), workspace.dir)
	if err != nil {
		t.Fatalf("filepath.Rel(): %v", err)
	}
	if relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(os.PathSeparator)) {
		t.Fatalf("workspace directory %q is not below os.TempDir()", workspace.dir)
	}
}

func TestWorkspaceInjectableRootAndMode(t *testing.T) {
	root := t.TempDir()
	umaskMu.Lock()
	oldUmask := syscall.Umask(0)
	t.Cleanup(func() {
		syscall.Umask(oldUmask)
		umaskMu.Unlock()
	})

	workspace, err := newWorkspace(context.Background(), 2, WorkspaceOptions{
		TemporaryByteBudget: testByteBudget,
	}, workspaceHooks{root: root, availableBytes: fixedAvailableBytes(math.MaxInt64)})
	if err != nil {
		t.Fatalf("newWorkspace() error = %v", err)
	}
	t.Cleanup(func() { workspace.Close() })

	if filepath.Dir(workspace.dir) != root {
		t.Errorf("workspace root = %q, want %q", filepath.Dir(workspace.dir), root)
	}
	info, err := os.Stat(workspace.dir)
	if err != nil {
		t.Fatalf("os.Stat(): %v", err)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Errorf("workspace mode = %#o, want 0700", got)
	}
}

func TestWorkspaceUniqueForSameJob(t *testing.T) {
	first := newTestWorkspace(t, context.Background(), 3, WorkspaceOptions{TemporaryByteBudget: testByteBudget})
	second := newTestWorkspace(t, context.Background(), 3, WorkspaceOptions{TemporaryByteBudget: testByteBudget})
	if first.dir == second.dir {
		t.Fatalf("workspace directories are equal: %q", first.dir)
	}
}

func TestWorkspacePathSafety(t *testing.T) {
	workspace := newTestWorkspace(t, context.Background(), 4, WorkspaceOptions{TemporaryByteBudget: testByteBudget})

	for _, name := range []string{"", " ", ".", "..", "/absolute.pdf", `dir/file.pdf`, `dir\\file.pdf`} {
		t.Run(strconv.Quote(name), func(t *testing.T) {
			if _, err := workspace.Path(name); err == nil {
				t.Fatal("Path() error = nil")
			} else {
				assertRenderingError(t, err)
				if strings.TrimSpace(name) != "" {
					assertRedacted(t, err, name)
				}
			}
		})
	}

	path, err := workspace.Path("document with spaces.pdf")
	if err != nil {
		t.Fatalf("Path() error = %v", err)
	}
	if filepath.Dir(path) != workspace.dir {
		t.Errorf("Path() = %q, want direct child of %q", path, workspace.dir)
	}
}

func TestWorkspaceOptionsValidation(t *testing.T) {
	for _, test := range []struct {
		name    string
		jobID   int64
		options WorkspaceOptions
	}{
		{name: "zero job", options: WorkspaceOptions{TemporaryByteBudget: 1}},
		{name: "negative job", jobID: -1, options: WorkspaceOptions{TemporaryByteBudget: 1}},
		{name: "zero budget", jobID: 1},
		{name: "negative budget", jobID: 1, options: WorkspaceOptions{TemporaryByteBudget: -1}},
		{name: "negative reserve", jobID: 1, options: WorkspaceOptions{TemporaryByteBudget: 1, MinimumFreeBytes: -1}},
	} {
		t.Run(test.name, func(t *testing.T) {
			workspace, err := NewWorkspace(context.Background(), test.jobID, test.options)
			if err == nil {
				workspace.Close()
				t.Fatal("NewWorkspace() error = nil")
			}
			assertRenderingError(t, err)
		})
	}
}

func TestWorkspaceReserveBudgetAndOverflow(t *testing.T) {
	workspace, err := newWorkspace(context.Background(), 5, WorkspaceOptions{
		TemporaryByteBudget: 10,
		MinimumFreeBytes:    5,
	}, workspaceHooks{root: t.TempDir(), availableBytes: fixedAvailableBytes(100)})
	if err != nil {
		t.Fatalf("newWorkspace() error = %v", err)
	}
	t.Cleanup(func() { workspace.Close() })

	if err := workspace.Reserve(context.Background(), 4); err != nil {
		t.Fatalf("Reserve(4) error = %v", err)
	}
	if err := workspace.Reserve(context.Background(), 6); err != nil {
		t.Fatalf("Reserve(6) error = %v", err)
	}
	if err := workspace.Reserve(context.Background(), 1); err == nil {
		t.Fatal("Reserve over budget error = nil")
	} else {
		assertRenderingError(t, err)
	}
	if err := workspace.Reserve(context.Background(), math.MaxInt64); err == nil {
		t.Fatal("Reserve overflow error = nil")
	} else {
		assertRenderingError(t, err)
	}
	if err := workspace.Reserve(context.Background(), -1); err == nil {
		t.Fatal("Reserve negative error = nil")
	} else {
		assertRenderingError(t, err)
	}
}

func TestWorkspaceReserveChecksFreeSpaceBeforeUpdating(t *testing.T) {
	var calls int
	workspace, err := newWorkspace(context.Background(), 6, WorkspaceOptions{
		TemporaryByteBudget: 100,
		MinimumFreeBytes:    10,
	}, workspaceHooks{
		root: t.TempDir(),
		availableBytes: func(string) (int64, error) {
			calls++
			if calls == 1 {
				return 100, nil
			}
			return 14, nil
		},
	})
	if err != nil {
		t.Fatalf("newWorkspace() error = %v", err)
	}
	t.Cleanup(func() { workspace.Close() })

	if err := workspace.Reserve(context.Background(), 5); err == nil {
		t.Fatal("Reserve() error = nil")
	}
	if workspace.reserved != 0 {
		t.Errorf("reserved = %d, want 0", workspace.reserved)
	}
}

func TestWorkspaceReserveChecksCurrentFreeSpaceForNewBytes(t *testing.T) {
	available := []int64{100, 100, 60}
	workspace, err := newWorkspace(context.Background(), 61, WorkspaceOptions{
		TemporaryByteBudget: 100,
		MinimumFreeBytes:    10,
	}, workspaceHooks{
		root: t.TempDir(),
		availableBytes: func(string) (int64, error) {
			value := available[0]
			available = available[1:]
			return value, nil
		},
	})
	if err != nil {
		t.Fatalf("newWorkspace() error = %v", err)
	}
	t.Cleanup(func() { workspace.Close() })

	if err := workspace.Reserve(context.Background(), 40); err != nil {
		t.Fatalf("first Reserve() error = %v", err)
	}
	if err := workspace.Reserve(context.Background(), 20); err != nil {
		t.Fatalf("second Reserve() error = %v", err)
	}
	if workspace.reserved != 60 {
		t.Errorf("reserved = %d, want 60", workspace.reserved)
	}
}

func TestWorkspaceReserveRejectsNewBytesBelowCurrentFreeSpace(t *testing.T) {
	workspace, err := newWorkspace(context.Background(), 62, WorkspaceOptions{
		TemporaryByteBudget: 100,
		MinimumFreeBytes:    10,
	}, workspaceHooks{root: t.TempDir(), availableBytes: fixedAvailableBytes(29)})
	if err != nil {
		t.Fatalf("newWorkspace() error = %v", err)
	}
	t.Cleanup(func() { workspace.Close() })

	if err := workspace.Reserve(context.Background(), 20); err == nil {
		t.Fatal("Reserve() error = nil")
	} else {
		assertRenderingError(t, err)
	}
}

func TestWorkspaceConstructorRejectsInsufficientFreeSpace(t *testing.T) {
	root := t.TempDir()
	workspace, err := newWorkspace(context.Background(), 7, WorkspaceOptions{
		TemporaryByteBudget: 100,
		MinimumFreeBytes:    11,
	}, workspaceHooks{root: root, availableBytes: fixedAvailableBytes(10)})
	if err == nil {
		workspace.Close()
		t.Fatal("newWorkspace() error = nil")
	}
	assertRenderingError(t, err)
	assertDirectoryEmpty(t, root)
}

func TestWorkspaceCloseRecursiveAndIdempotent(t *testing.T) {
	workspace := newTestWorkspace(t, context.Background(), 8, WorkspaceOptions{TemporaryByteBudget: testByteBudget})
	root := workspace.dir
	child, err := workspace.Path("child")
	if err != nil {
		t.Fatalf("Path() error = %v", err)
	}
	if err := os.Mkdir(child, 0o700); err != nil {
		t.Fatalf("os.Mkdir(): %v", err)
	}
	if err := os.WriteFile(filepath.Join(child, "data"), []byte("safe fixture"), 0o600); err != nil {
		t.Fatalf("os.WriteFile(): %v", err)
	}

	if err := workspace.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if err := workspace.Close(); err != nil {
		t.Fatalf("second Close() error = %v", err)
	}
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("os.Stat() error = %v, want os.ErrNotExist", err)
	}
}

func TestWorkspaceCanceledSetupCleansUp(t *testing.T) {
	root := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	workspace, err := newWorkspace(ctx, 9, WorkspaceOptions{TemporaryByteBudget: 100}, workspaceHooks{
		root: root,
		availableBytes: func(string) (int64, error) {
			cancel()
			return 100, nil
		},
	})
	if err == nil {
		workspace.Close()
		t.Fatal("newWorkspace() error = nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("errors.Is(context.Canceled) = false: %v", err)
	}
	assertDirectoryEmpty(t, root)

	preCanceled, preCancel := context.WithCancel(context.Background())
	preCancel()
	if workspace, err = newWorkspace(preCanceled, 10, WorkspaceOptions{TemporaryByteBudget: 100}, workspaceHooks{root: root}); err == nil {
		workspace.Close()
		t.Fatal("pre-canceled newWorkspace() error = nil")
	}
	assertDirectoryEmpty(t, root)
}

func TestWorkspaceReserveCancellation(t *testing.T) {
	workspace := newTestWorkspace(t, context.Background(), 11, WorkspaceOptions{TemporaryByteBudget: testByteBudget})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := workspace.Reserve(ctx, 1)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Reserve() error = %v, want context.Canceled", err)
	}
	assertRenderingError(t, err)
}

func TestInspectRealPDFs(t *testing.T) {
	inspector := newTestInspector(t, InspectOptions{Executable: "/usr/bin/pdfinfo", Timeout: 2 * time.Second})
	for _, test := range []struct {
		fixture string
		pages   int
	}{
		{fixture: "one-page.pdf", pages: 1},
		{fixture: "multi-page.pdf", pages: 3},
	} {
		t.Run(test.fixture, func(t *testing.T) {
			workspace := newTestWorkspace(t, context.Background(), 20, WorkspaceOptions{TemporaryByteBudget: testByteBudget})
			copyFixture(t, workspace, test.fixture, test.fixture)
			info, err := inspector.Inspect(context.Background(), workspace, test.fixture)
			if err != nil {
				t.Fatalf("Inspect() error = %v", err)
			}
			if info.Pages != test.pages {
				t.Errorf("Pages = %d, want %d", info.Pages, test.pages)
			}
		})
	}
}

func TestInspectMalformedPDF(t *testing.T) {
	inspector := newTestInspector(t, InspectOptions{Executable: "/usr/bin/pdfinfo", Timeout: 2 * time.Second})
	workspace := newTestWorkspace(t, context.Background(), 21, WorkspaceOptions{TemporaryByteBudget: testByteBudget})
	path := copyFixture(t, workspace, "malformed.pdf", "malformed.pdf")

	_, err := inspector.Inspect(context.Background(), workspace, "malformed.pdf")
	assertRenderingError(t, err)
	assertRedacted(t, err, path, "malformed.pdf", "fixture-content-canary")
}

func TestInspectCallerCancellation(t *testing.T) {
	started := filepath.Join(t.TempDir(), "started")
	executable := writeExecutable(t, "touch "+shellQuote(started)+"\nsleep 5\nprintf 'Pages: 1\\n'\n")
	inspector := newTestInspector(t, InspectOptions{Executable: executable, Timeout: 10 * time.Second})
	workspace := newWorkspaceFile(t, "cancel.pdf", []byte("safe"))
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := inspector.Inspect(ctx, workspace, "cancel.pdf")
		result <- err
	}()
	waitForFile(t, started)
	cancel()
	err := <-result
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Inspect() error = %v, want context.Canceled", err)
	}
	assertRenderingError(t, err)
}

func TestInspectPreCanceledContextDoesNotExecute(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "executed")
	executable := writeExecutable(t, "touch "+shellQuote(marker)+"\nprintf 'Pages: 1\\n'\n")
	inspector := newTestInspector(t, InspectOptions{Executable: executable, Timeout: time.Second})
	workspace := newWorkspaceFile(t, "cancel.pdf", []byte("safe"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := inspector.Inspect(ctx, workspace, "cancel.pdf")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Inspect() error = %v, want context.Canceled", err)
	}
	if _, statErr := os.Stat(marker); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("command executed, os.Stat() error = %v", statErr)
	}
}

func TestInspectInternalTimeoutTerminatesProcess(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "completed")
	executable := writeExecutable(t, "sleep 2\nprintf completed >"+shellQuote(marker)+"\nprintf 'Pages: 1\\n'\n")
	inspector := newTestInspector(t, InspectOptions{Executable: executable, Timeout: 30 * time.Millisecond})
	workspace := newWorkspaceFile(t, "timeout.pdf", []byte("safe"))

	started := time.Now()
	_, err := inspector.Inspect(context.Background(), workspace, "timeout.pdf")
	if err == nil {
		t.Fatal("Inspect() error = nil")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("internal timeout exposed as caller deadline: %v", err)
	}
	if time.Since(started) > time.Second {
		t.Fatalf("Inspect() did not terminate promptly")
	}
	time.Sleep(100 * time.Millisecond)
	if _, statErr := os.Stat(marker); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("timed-out command completed, os.Stat() error = %v", statErr)
	}
}

func TestInspectInternalTimeoutDoesNotWaitForDetachedPipeHolder(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("requires Linux process session semantics")
	}
	setsid, err := exec.LookPath("setsid")
	if err != nil {
		t.Skip("setsid is unavailable")
	}
	marker := filepath.Join(t.TempDir(), "descendant-completed")
	executable := writeExecutable(t, shellQuote(setsid)+" /bin/sh -c "+shellQuote("sleep 1; : >"+shellQuote(marker))+" &\nsleep 5\n")
	inspector := newTestInspector(t, InspectOptions{Executable: executable, Timeout: 30 * time.Millisecond})
	workspace := newWorkspaceFile(t, "detached.pdf", []byte("safe"))

	started := time.Now()
	_, err = inspector.Inspect(context.Background(), workspace, "detached.pdf")
	elapsed := time.Since(started)
	assertRenderingError(t, err)
	if elapsed > 500*time.Millisecond {
		t.Fatalf("Inspect() elapsed = %v, want at most 500ms", elapsed)
	}
	waitForFile(t, marker)
}

func TestNewInspectorExecutableValidation(t *testing.T) {
	for _, test := range []struct {
		name    string
		options InspectOptions
	}{
		{name: "relative", options: InspectOptions{Executable: "pdfinfo", Timeout: time.Second}},
		{name: "missing", options: InspectOptions{Executable: filepath.Join(t.TempDir(), "missing"), Timeout: time.Second}},
		{name: "directory", options: InspectOptions{Executable: t.TempDir(), Timeout: time.Second}},
		{name: "not executable", options: InspectOptions{Executable: writeFile(t, "not-executable", []byte("data"), 0o600), Timeout: time.Second}},
		{name: "negative timeout", options: InspectOptions{Executable: "/usr/bin/pdfinfo", Timeout: -time.Second}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := NewInspector(test.options); err == nil {
				t.Fatal("NewInspector() error = nil")
			} else {
				assertRenderingError(t, err)
				assertRedacted(t, err, test.options.Executable)
			}
		})
	}
}

func TestNewInspectorDefaults(t *testing.T) {
	inspector, err := NewInspector(InspectOptions{})
	if err != nil {
		t.Fatalf("NewInspector() error = %v", err)
	}
	if !filepath.IsAbs(inspector.executable) {
		t.Errorf("executable = %q, want absolute path", inspector.executable)
	}
	if inspector.timeout <= 0 {
		t.Errorf("timeout = %v, want positive", inspector.timeout)
	}
}

func TestNewInspectorDefaultExecutableLookupFailure(t *testing.T) {
	emptyPath := t.TempDir()
	t.Setenv("PATH", emptyPath)

	_, err := NewInspector(InspectOptions{})
	assertRenderingError(t, err)
	assertRedacted(t, err, emptyPath, "pdfinfo")
}

func TestInspectPassesSingleArgumentWithSpaces(t *testing.T) {
	workspace := newWorkspaceFile(t, "document with spaces.pdf", []byte("safe"))
	expected, err := workspace.Path("document with spaces.pdf")
	if err != nil {
		t.Fatalf("Path() error = %v", err)
	}
	executable := writeExecutable(t, "if [ \"$#\" -ne 1 ] || [ \"$1\" != "+shellQuote(expected)+" ]; then exit 9; fi\nprintf 'Pages: 2\\n'\n")
	inspector := newTestInspector(t, InspectOptions{Executable: executable, Timeout: time.Second})

	info, err := inspector.Inspect(context.Background(), workspace, "document with spaces.pdf")
	if err != nil {
		t.Fatalf("Inspect() error = %v", err)
	}
	if info.Pages != 2 {
		t.Errorf("Pages = %d, want 2", info.Pages)
	}
}

func TestInspectUsesCleanEnvironment(t *testing.T) {
	t.Setenv("PDF_INSPECT_SECRET_CANARY", "environment-secret-canary")
	executable := writeExecutable(t, "if env | grep -q PDF_INSPECT_SECRET_CANARY; then exit 8; fi\nprintf 'Pages: 1\\n'\n")
	inspector := newTestInspector(t, InspectOptions{Executable: executable, Timeout: time.Second})
	workspace := newWorkspaceFile(t, "clean-env.pdf", []byte("safe"))

	if _, err := inspector.Inspect(context.Background(), workspace, "clean-env.pdf"); err != nil {
		t.Fatalf("Inspect() error = %v", err)
	}
}

func TestInspectBoundsAndRedactsNoisyStderr(t *testing.T) {
	const stderrCanary = "stderr-secret-canary"
	executable := writeExecutable(t, "i=0\nwhile [ \"$i\" -lt 20000 ]; do printf '"+stderrCanary+"%08d\\n' \"$i\" >&2; i=$((i+1)); done\nexit 7\n")
	inspector := newTestInspector(t, InspectOptions{Executable: executable, Timeout: 3 * time.Second})
	workspace := newWorkspaceFile(t, "noisy.pdf", []byte("pdf-content-canary"))
	path, err := workspace.Path("noisy.pdf")
	if err != nil {
		t.Fatalf("Path() error = %v", err)
	}

	_, err = inspector.Inspect(context.Background(), workspace, "noisy.pdf")
	assertRenderingError(t, err)
	var failure commandFailure
	if !errors.As(err, &failure) {
		t.Fatalf("errors.As(commandFailure) = false: %v", err)
	}
	if got := len(failure.stderr); got != stderrLimit {
		t.Errorf("retained stderr bytes = %d, want %d", got, stderrLimit)
	}
	if wantPrefix := stderrCanary + "00000000\n"; !strings.HasPrefix(failure.stderr, wantPrefix) {
		t.Errorf("retained stderr does not start with expected command output")
	}
	assertRedacted(t, err, stderrCanary, "pdf-content-canary", "noisy.pdf", path)
}

func TestBoundedBufferCapsRetainedData(t *testing.T) {
	buffer := boundedBuffer{limit: stderrLimit}
	first := strings.Repeat("a", stderrLimit-1)
	second := "bcdef"

	written, err := buffer.Write([]byte(first))
	if err != nil {
		t.Fatalf("first Write() error = %v", err)
	}
	if written != len(first) {
		t.Errorf("first Write() = %d, want %d", written, len(first))
	}
	written, err = buffer.Write([]byte(second))
	if err != nil {
		t.Fatalf("second Write() error = %v", err)
	}
	if written != len(second) {
		t.Errorf("second Write() = %d, want %d", written, len(second))
	}
	if got := len(buffer.data); got != stderrLimit {
		t.Errorf("retained bytes = %d, want %d", got, stderrLimit)
	}
	if got := buffer.String(); got != first+second[:1] {
		t.Errorf("retained data has unexpected content")
	}
	if !buffer.truncated {
		t.Error("truncated = false, want true")
	}
}

func TestInspectStrictPagesParsing(t *testing.T) {
	for _, test := range []struct {
		name   string
		output string
	}{
		{name: "missing", output: "Title: fixture\\n"},
		{name: "duplicate", output: "Pages: 1\\nPages: 2\\n"},
		{name: "zero", output: "Pages: 0\\n"},
		{name: "negative", output: "Pages: -1\\n"},
		{name: "signed", output: "Pages: +1\\n"},
		{name: "decimal", output: "Pages: 1.0\\n"},
		{name: "overflow", output: "Pages: 999999999999999999999999999999999999\\n"},
		{name: "trailing junk", output: "Pages: 1 page\\n"},
		{name: "non ascii digit", output: "Pages: ١\\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			executable := writeExecutable(t, "printf "+shellQuote(test.output)+"\n")
			inspector := newTestInspector(t, InspectOptions{Executable: executable, Timeout: time.Second})
			workspace := newWorkspaceFile(t, "strict.pdf", []byte("safe"))
			if _, err := inspector.Inspect(context.Background(), workspace, "strict.pdf"); err == nil {
				t.Fatal("Inspect() error = nil")
			} else {
				assertRenderingError(t, err)
			}
		})
	}
}

func TestInspectRejectsTruncatedMetadata(t *testing.T) {
	executable := writeExecutable(t, "printf 'Pages: 1\\n'\ni=0\nwhile [ \"$i\" -lt 70000 ]; do printf x; i=$((i+1)); done\nprintf '\\nPages: 2\\n'\n")
	inspector := newTestInspector(t, InspectOptions{Executable: executable, Timeout: 3 * time.Second})
	workspace := newWorkspaceFile(t, "large-metadata.pdf", []byte("safe"))

	if _, err := inspector.Inspect(context.Background(), workspace, "large-metadata.pdf"); err == nil {
		t.Fatal("Inspect() error = nil")
	} else {
		assertRenderingError(t, err)
	}
}

func TestInspectRejectsForeignPath(t *testing.T) {
	executable := writeExecutable(t, "printf 'Pages: 1\\n'\n")
	inspector := newTestInspector(t, InspectOptions{Executable: executable, Timeout: time.Second})
	workspace := newTestWorkspace(t, context.Background(), 30, WorkspaceOptions{TemporaryByteBudget: testByteBudget})
	foreignName := "foreign.pdf"
	foreignPath := filepath.Join(workspace.dir, foreignName)
	if err := os.WriteFile(foreignPath, []byte("foreign-content-canary"), 0o600); err != nil {
		t.Fatalf("os.WriteFile(): %v", err)
	}

	_, err := inspector.Inspect(context.Background(), workspace, foreignName)
	assertRenderingError(t, err)
	assertRedacted(t, err, foreignName, foreignPath, "foreign-content-canary")
}

func newTestWorkspace(t *testing.T, ctx context.Context, jobID int64, options WorkspaceOptions) *Workspace {
	t.Helper()
	workspace, err := NewWorkspace(ctx, jobID, options)
	if err != nil {
		t.Fatalf("NewWorkspace() error = %v", err)
	}
	t.Cleanup(func() {
		if err := workspace.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})
	return workspace
}

func newWorkspaceFile(t *testing.T, name string, content []byte) *Workspace {
	t.Helper()
	workspace := newTestWorkspace(t, context.Background(), 40, WorkspaceOptions{TemporaryByteBudget: testByteBudget})
	path, err := workspace.Path(name)
	if err != nil {
		t.Fatalf("Path() error = %v", err)
	}
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatalf("os.WriteFile(): %v", err)
	}
	return workspace
}

func newTestInspector(t *testing.T, options InspectOptions) *Inspector {
	t.Helper()
	inspector, err := NewInspector(options)
	if err != nil {
		t.Fatalf("NewInspector() error = %v", err)
	}
	return inspector
}

func copyFixture(t *testing.T, workspace *Workspace, fixture, name string) string {
	t.Helper()
	content, err := os.ReadFile(filepath.Join("..", "..", "testdata", "pdfs", fixture))
	if err != nil {
		t.Fatalf("os.ReadFile(%q): %v", fixture, err)
	}
	path, err := workspace.Path(name)
	if err != nil {
		t.Fatalf("Path() error = %v", err)
	}
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatalf("os.WriteFile(): %v", err)
	}
	return path
}

func fixedAvailableBytes(bytes int64) func(string) (int64, error) {
	return func(string) (int64, error) { return bytes, nil }
}

func assertDirectoryEmpty(t *testing.T, path string) {
	t.Helper()
	entries, err := os.ReadDir(path)
	if err != nil {
		t.Fatalf("os.ReadDir(): %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("directory contains %d entries, want none", len(entries))
	}
}

func assertRenderingError(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("error = nil, want rendering error")
	}
	var safeError *saferr.Error
	if !errors.As(err, &safeError) {
		t.Fatalf("errors.As(*saferr.Error) = false: %v", err)
	}
	if safeError.Category() != saferr.CategoryRendering {
		t.Errorf("category = %q, want %q", safeError.Category(), saferr.CategoryRendering)
	}
}

func assertRedacted(t *testing.T, err error, canaries ...string) {
	t.Helper()
	for current := err; current != nil; current = errors.Unwrap(current) {
		for _, format := range []string{"%s", "%v", "%+v", "%#v"} {
			formatted := fmt.Sprintf(format, current)
			for _, canary := range canaries {
				if canary != "" && strings.Contains(formatted, canary) {
					t.Errorf("error chain format %s disclosed %q: %q", format, canary, formatted)
				}
			}
		}
	}
}

func writeExecutable(t *testing.T, body string) string {
	t.Helper()
	return writeFile(t, "fake-pdfinfo", []byte("#!/bin/sh\n"+body), 0o700)
}

func writeFile(t *testing.T, name string, content []byte, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, content, mode); err != nil {
		t.Fatalf("os.WriteFile(): %v", err)
	}
	return path
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		runtime.Gosched()
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %q", path)
}

var umaskMu sync.Mutex
