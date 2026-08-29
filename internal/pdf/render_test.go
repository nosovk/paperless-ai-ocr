package pdf

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestNewRendererDefaults(t *testing.T) {
	renderer, err := NewRenderer(RenderOptions{})
	if err != nil {
		t.Fatalf("NewRenderer() error = %v", err)
	}
	if !filepath.IsAbs(renderer.executable) {
		t.Errorf("executable = %q, want absolute path", renderer.executable)
	}
	if renderer.dpi != 200 {
		t.Errorf("dpi = %d, want 200", renderer.dpi)
	}
	if renderer.timeout != 5*time.Minute {
		t.Errorf("timeout = %v, want 5m", renderer.timeout)
	}
}

func TestNewRendererDefaultExecutableLookupFailure(t *testing.T) {
	emptyPath := t.TempDir()
	t.Setenv("PATH", emptyPath)

	_, err := NewRenderer(RenderOptions{})
	assertRenderingError(t, err)
	assertRedacted(t, err, emptyPath, "pdftoppm")
}

func TestNewRendererValidation(t *testing.T) {
	for _, test := range []struct {
		name    string
		options RenderOptions
	}{
		{name: "relative", options: RenderOptions{Executable: "pdftoppm"}},
		{name: "missing", options: RenderOptions{Executable: filepath.Join(t.TempDir(), "missing")}},
		{name: "directory", options: RenderOptions{Executable: t.TempDir()}},
		{name: "not executable", options: RenderOptions{Executable: writeFile(t, "not-executable-renderer", []byte("data"), 0o600)}},
		{name: "negative DPI", options: RenderOptions{Executable: "/usr/bin/pdftoppm", DPI: -1}},
		{name: "negative timeout", options: RenderOptions{Executable: "/usr/bin/pdftoppm", Timeout: -time.Second}},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := NewRenderer(test.options)
			assertRenderingError(t, err)
			assertRedacted(t, err, test.options.Executable)
		})
	}
}

func TestRenderRejectsInvalidInputs(t *testing.T) {
	renderer := newTestRenderer(t, RenderOptions{Executable: "/usr/bin/pdftoppm"})
	workspace := newWorkspaceFile(t, "owned.pdf", []byte("safe"))
	callback := func([]Page) error { return nil }

	for _, test := range []struct {
		name      string
		renderer  *Renderer
		ctx       context.Context
		workspace *Workspace
		source    string
		first     int
		last      int
		visit     func([]Page) error
	}{
		{name: "nil renderer", ctx: context.Background(), workspace: workspace, source: "owned.pdf", first: 1, last: 1, visit: callback},
		{name: "nil context", renderer: renderer, workspace: workspace, source: "owned.pdf", first: 1, last: 1, visit: callback},
		{name: "nil workspace", renderer: renderer, ctx: context.Background(), source: "owned.pdf", first: 1, last: 1, visit: callback},
		{name: "nil callback", renderer: renderer, ctx: context.Background(), workspace: workspace, source: "owned.pdf", first: 1, last: 1},
		{name: "zero first", renderer: renderer, ctx: context.Background(), workspace: workspace, source: "owned.pdf", last: 1, visit: callback},
		{name: "zero last", renderer: renderer, ctx: context.Background(), workspace: workspace, source: "owned.pdf", first: 1, visit: callback},
		{name: "reversed", renderer: renderer, ctx: context.Background(), workspace: workspace, source: "owned.pdf", first: 2, last: 1, visit: callback},
		{name: "foreign source", renderer: renderer, ctx: context.Background(), workspace: workspace, source: "foreign.pdf", first: 1, last: 1, visit: callback},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := test.renderer.Render(test.ctx, test.workspace, test.source, test.first, test.last, test.visit)
			assertRenderingError(t, err)
			assertRedacted(t, err, test.source)
		})
	}
}

func TestRenderRejectsCanceledContext(t *testing.T) {
	renderer := newTestRenderer(t, RenderOptions{Executable: "/usr/bin/pdftoppm"})
	workspace := newWorkspaceFile(t, "owned.pdf", []byte("safe"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := renderer.Render(ctx, workspace, "owned.pdf", 1, 1, func([]Page) error { return nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Render() error = %v, want context.Canceled", err)
	}
}

func TestRenderRequestedRange(t *testing.T) {
	workspace := newWorkspaceFile(t, "document with spaces.pdf", []byte("safe"))
	source, err := workspace.ownedPath("document with spaces.pdf")
	if err != nil {
		t.Fatalf("ownedPath() error = %v", err)
	}
	executable := writeRenderExecutable(t, `
if [ "$#" -ne 9 ] || [ "$1" != -f ] || [ "$2" != 2 ] || [ "$3" != -l ] || [ "$4" != 3 ] || [ "$5" != -r ] || [ "$6" != 200 ] || [ "$7" != -png ] || [ "$8" != `+shellQuote(source)+` ]; then exit 9; fi
printf '\211PNG\r\n\032\n' >"$9-2.png"
printf '\211PNG\r\n\032\n' >"$9-3.png"
`)
	renderer := newTestRenderer(t, RenderOptions{Executable: executable})

	var pages []Page
	if err := renderer.Render(context.Background(), workspace, "document with spaces.pdf", 2, 3, func(batch []Page) error {
		pages = append(pages, batch...)
		info, err := os.Stat(filepath.Dir(batch[0].Path))
		if err != nil {
			return err
		}
		if info.Mode().Perm() != 0o700 {
			return fmt.Errorf("batch mode = %#o", info.Mode().Perm())
		}
		if strings.Contains(filepath.Base(filepath.Dir(batch[0].Path)), "document") {
			return errors.New("batch name contains source name")
		}
		return nil
	}); err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	if len(pages) != 2 || pages[0].Number != 2 || pages[1].Number != 3 {
		t.Fatalf("pages = %+v, want page numbers 2, 3", pages)
	}
}

func TestRenderDPI(t *testing.T) {
	workspace := newWorkspaceFile(t, "dpi.pdf", []byte("safe"))
	executable := writeRenderExecutable(t, `
if [ "$6" != 144 ]; then exit 9; fi
printf '\211PNG\r\n\032\n' >"$9-1.png"
`)
	renderer := newTestRenderer(t, RenderOptions{Executable: executable, DPI: 144})

	if err := renderer.Render(context.Background(), workspace, "dpi.pdf", 1, 1, func([]Page) error { return nil }); err != nil {
		t.Fatalf("Render() error = %v", err)
	}
}

func TestRenderUsesCleanEnvironment(t *testing.T) {
	t.Setenv("PDF_RENDER_SECRET_CANARY", "environment-secret-canary")
	workspace := newWorkspaceFile(t, "environment.pdf", []byte("safe"))
	executable := writeRenderExecutable(t, `
if [ "$LANG" != C ] || [ "$LC_ALL" != C ] || [ "$PATH" != /usr/bin:/bin ] || env | /usr/bin/grep -q PDF_RENDER_SECRET_CANARY; then exit 8; fi
printf '\211PNG\r\n\032\n' >"$9-1.png"
`)
	renderer := newTestRenderer(t, RenderOptions{Executable: executable})

	if err := renderer.Render(context.Background(), workspace, "environment.pdf", 1, 1, func([]Page) error { return nil }); err != nil {
		t.Fatalf("Render() error = %v", err)
	}
}

func TestRenderOutputOrderingFilenameAndPadding(t *testing.T) {
	workspace := newWorkspaceFile(t, "ordering.pdf", []byte("safe"))
	executable := writeRenderExecutable(t, `
printf '\211PNG\r\n\032\n' >"$9-0003.png"
printf '\211PNG\r\n\032\n' >"$9-2.png"
`)
	renderer := newTestRenderer(t, RenderOptions{Executable: executable})

	if err := renderer.Render(context.Background(), workspace, "ordering.pdf", 2, 3, func(pages []Page) error {
		if len(pages) != 2 {
			return fmt.Errorf("len(pages) = %d", len(pages))
		}
		for index, number := range []int{2, 3} {
			if pages[index].Number != number {
				return fmt.Errorf("pages[%d].Number = %d", index, pages[index].Number)
			}
			if got, want := filepath.Base(pages[index].Path), fmt.Sprintf("page-%06d.png", number); got != want {
				return fmt.Errorf("page name = %q, want %q", got, want)
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("Render() error = %v", err)
	}
}

func TestRenderOutputRejectsInvalidEntries(t *testing.T) {
	for _, test := range []struct {
		name  string
		body  string
		first int
		last  int
	}{
		{name: "missing", body: `printf '\211PNG\r\n\032\n' >"$9-1.png"`, first: 1, last: 2},
		{name: "duplicate padding", body: `printf '\211PNG\r\n\032\n' >"$9-1.png"; printf '\211PNG\r\n\032\n' >"$9-01.png"`, first: 1, last: 1},
		{name: "unknown page", body: `printf '\211PNG\r\n\032\n' >"$9-2.png"`, first: 1, last: 1},
		{name: "extra file", body: `printf '\211PNG\r\n\032\n' >"$9-1.png"; : >"$(dirname "$9")/extra.txt"`, first: 1, last: 1},
		{name: "directory", body: `mkdir "$9-1.png"`, first: 1, last: 1},
		{name: "symlink", body: `printf '\211PNG\r\n\032\n' >"$(dirname "$9")/target"; ln -s target "$9-1.png"`, first: 1, last: 1},
		{name: "empty", body: `: >"$9-1.png"`, first: 1, last: 1},
		{name: "truncated signature", body: `printf '\211PNG' >"$9-1.png"`, first: 1, last: 1},
		{name: "invalid signature", body: `printf 'notapng!' >"$9-1.png"`, first: 1, last: 1},
		{name: "non ascii page", body: `printf '\211PNG\r\n\032\n' >"$9-١.png"`, first: 1, last: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			workspace := newWorkspaceFile(t, "invalid-output.pdf", []byte("safe"))
			renderer := newTestRenderer(t, RenderOptions{Executable: writeRenderExecutable(t, test.body+"\n")})
			called := false
			err := renderer.Render(context.Background(), workspace, "invalid-output.pdf", test.first, test.last, func([]Page) error {
				called = true
				return nil
			})
			assertRenderingError(t, err)
			if called {
				t.Fatal("callback called for invalid output")
			}
		})
	}
}

func TestRenderOutputRejectsFIFOAndCleansUp(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("requires Linux FIFO support")
	}
	workspace := newWorkspaceFile(t, "fifo-output.pdf", []byte("safe"))
	renderer := newTestRenderer(t, RenderOptions{Executable: writeRenderExecutable(t, `mkfifo "$9-1.png"`)})
	called := false

	err := renderer.Render(context.Background(), workspace, "fifo-output.pdf", 1, 1, func([]Page) error {
		called = true
		return nil
	})
	assertRenderingError(t, err)
	if called {
		t.Fatal("callback called for FIFO output")
	}
	entries, readErr := os.ReadDir(workspace.dir)
	if readErr != nil {
		t.Fatalf("os.ReadDir() error = %v", readErr)
	}
	if len(entries) != 1 || entries[0].Name() != "fifo-output.pdf" {
		t.Fatalf("workspace entries = %+v, want only source", entries)
	}
}

func TestRenderByteLimits(t *testing.T) {
	for _, test := range []struct {
		name   string
		budget int64
		body   string
		first  int
		last   int
	}{
		{name: "per file", budget: 8, body: `printf '\211PNG\r\n\032\nx' >"$9-1.png"`, first: 1, last: 1},
		{name: "aggregate", budget: 16, body: `printf '\211PNG\r\n\032\nx' >"$9-1.png"; printf '\211PNG\r\n\032\ny' >"$9-2.png"`, first: 1, last: 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			workspace := newTestWorkspace(t, context.Background(), 77, WorkspaceOptions{TemporaryByteBudget: test.budget})
			path, err := workspace.Path("byte-limit.pdf")
			if err != nil {
				t.Fatalf("Path() error = %v", err)
			}
			if err := os.WriteFile(path, []byte("safe"), 0o600); err != nil {
				t.Fatalf("os.WriteFile() error = %v", err)
			}
			renderer := newTestRenderer(t, RenderOptions{Executable: writeRenderExecutable(t, test.body+"\n")})
			err = renderer.Render(context.Background(), workspace, "byte-limit.pdf", test.first, test.last, func([]Page) error { return nil })
			assertRenderingError(t, err)
		})
	}
}

func TestRenderOutputHugeRangeFailsSafely(t *testing.T) {
	_, err := collectRenderedPages(t.TempDir(), 1, int(^uint(0)>>1), 1)
	assertRenderingError(t, err)
}

func TestRenderCleanupAfterCallback(t *testing.T) {
	workspace := newWorkspaceFile(t, "cleanup.pdf", []byte("safe"))
	renderer := newTestRenderer(t, RenderOptions{Executable: writeRenderExecutable(t, `printf '\211PNG\r\n\032\n' >"$9-1.png"`)})
	var path string

	if err := renderer.Render(context.Background(), workspace, "cleanup.pdf", 1, 1, func(pages []Page) error {
		path = pages[0].Path
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if len(content) != len(pngSignature) {
			return fmt.Errorf("rendered size = %d", len(content))
		}
		return nil
	}); err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("os.Stat(callback path) error = %v, want os.ErrNotExist", err)
	}
}

func TestRenderCallbackErrorPreservesCauseAndCleansUp(t *testing.T) {
	callbackCause := errors.New("callback cause")
	workspace := newWorkspaceFile(t, "callback-error.pdf", []byte("safe"))
	renderer := newTestRenderer(t, RenderOptions{Executable: writeRenderExecutable(t, `printf '\211PNG\r\n\032\n' >"$9-1.png"`)})
	var path string

	err := renderer.Render(context.Background(), workspace, "callback-error.pdf", 1, 1, func(pages []Page) error {
		path = pages[0].Path
		return callbackCause
	})
	if !errors.Is(err, callbackCause) {
		t.Fatalf("Render() error = %v, want callback cause", err)
	}
	assertRenderingError(t, err)
	if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("os.Stat(callback path) error = %v, want os.ErrNotExist", statErr)
	}
}

func TestRenderCallbackErrorPreservesIdentityWithoutDisclosingCause(t *testing.T) {
	workspace := newWorkspaceFile(t, "callback-path-error.pdf", []byte("safe"))
	renderer := newTestRenderer(t, RenderOptions{Executable: writeRenderExecutable(t, `printf '\211PNG\r\n\032\n' >"$9-1.png"`)})
	const canary = "callback-error-secret-canary"
	var path string
	var callbackCause *os.PathError

	err := renderer.Render(context.Background(), workspace, "callback-path-error.pdf", 1, 1, func(pages []Page) error {
		path = pages[0].Path
		callbackCause = &os.PathError{Op: canary, Path: path, Err: errors.New(canary)}
		return callbackCause
	})
	if !errors.Is(err, callbackCause) {
		t.Fatalf("Render() error = %v, want callback cause", err)
	}
	assertRenderingError(t, err)
	assertRedacted(t, err, canary, path)
	if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("os.Stat(callback path) error = %v, want os.ErrNotExist", statErr)
	}
}

func TestRenderCleanupDuringPanicUnwinding(t *testing.T) {
	workspace := newWorkspaceFile(t, "callback-panic.pdf", []byte("safe"))
	renderer := newTestRenderer(t, RenderOptions{Executable: writeRenderExecutable(t, `printf '\211PNG\r\n\032\n' >"$9-1.png"`)})
	var path string
	const panicValue = "callback panic"

	func() {
		defer func() {
			if recovered := recover(); recovered != panicValue {
				t.Fatalf("recover() = %v, want %q", recovered, panicValue)
			}
		}()
		_ = renderer.Render(context.Background(), workspace, "callback-panic.pdf", 1, 1, func(pages []Page) error {
			path = pages[0].Path
			panic(panicValue)
		})
	}()
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("os.Stat(callback path) error = %v, want os.ErrNotExist", err)
	}
}

func TestWorkspaceCloseRejectsActiveRenderCallback(t *testing.T) {
	workspace := newWorkspaceFile(t, "active-close.pdf", []byte("safe"))
	workspaceDir := workspace.dir
	renderer := newTestRenderer(t, RenderOptions{Executable: writeRenderExecutable(t, `printf '\211PNG\r\n\032\n' >"$9-1.png"`)})
	callbackStarted := make(chan struct{})
	releaseCallback := make(chan struct{})
	closeResult := make(chan error, 1)
	readResult := make(chan error, 1)
	renderResult := make(chan error, 1)

	go func() {
		renderResult <- renderer.Render(context.Background(), workspace, "active-close.pdf", 1, 1, func(pages []Page) error {
			close(callbackStarted)
			closeResult <- workspace.Close()
			_, err := os.ReadFile(pages[0].Path)
			readResult <- err
			<-releaseCallback
			return nil
		})
	}()
	<-callbackStarted
	closeErr := <-closeResult
	assertRenderingError(t, closeErr)
	if readErr := <-readResult; readErr != nil {
		t.Fatalf("os.ReadFile(rendered page) error = %v", readErr)
	}
	close(releaseCallback)
	if err := <-renderResult; err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	if err := workspace.Close(); err != nil {
		t.Fatalf("Close() after render error = %v", err)
	}
	if _, err := os.Stat(workspaceDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("os.Stat(workspace) error = %v, want os.ErrNotExist", err)
	}
}

func TestRenderPartialCommandFailureCleansUpAndRedacts(t *testing.T) {
	const stderrCanary = "stderr-secret-canary"
	workspace := newWorkspaceFile(t, "document-secret-canary.pdf", []byte("content-secret-canary"))
	sourcePath, err := workspace.ownedPath("document-secret-canary.pdf")
	if err != nil {
		t.Fatalf("ownedPath() error = %v", err)
	}
	executable := writeRenderExecutable(t, `printf '\211PNG\r\n\032\n' >"$9-1.png"; printf '`+stderrCanary+`' >&2; exit 7`)
	renderer := newTestRenderer(t, RenderOptions{Executable: executable})

	err = renderer.Render(context.Background(), workspace, "document-secret-canary.pdf", 1, 2, func([]Page) error {
		t.Fatal("callback called after command failure")
		return nil
	})
	assertRenderingError(t, err)
	assertRedacted(t, err, stderrCanary, "document-secret-canary.pdf", sourcePath, "content-secret-canary", executable, workspace.dir)
	entries, readErr := os.ReadDir(workspace.dir)
	if readErr != nil {
		t.Fatalf("os.ReadDir() error = %v", readErr)
	}
	if len(entries) != 1 || entries[0].Name() != "document-secret-canary.pdf" {
		t.Fatalf("workspace entries = %+v, want only source", entries)
	}
}

func TestRenderCallerCancellation(t *testing.T) {
	started := filepath.Join(t.TempDir(), "started")
	executable := writeRenderExecutable(t, `: >`+shellQuote(started)+`; sleep 5`)
	renderer := newTestRenderer(t, RenderOptions{Executable: executable, Timeout: 10 * time.Second})
	workspace := newWorkspaceFile(t, "cancel-render.pdf", []byte("safe"))
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		result <- renderer.Render(ctx, workspace, "cancel-render.pdf", 1, 1, func([]Page) error { return nil })
	}()
	waitForFile(t, started)
	cancel()
	err := <-result
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Render() error = %v, want context.Canceled", err)
	}
}

func TestRenderInternalTimeoutTerminatesProcess(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "completed")
	executable := writeRenderExecutable(t, `sleep 2; : >`+shellQuote(marker))
	renderer := newTestRenderer(t, RenderOptions{Executable: executable, Timeout: 30 * time.Millisecond})
	workspace := newWorkspaceFile(t, "timeout-render.pdf", []byte("safe"))

	started := time.Now()
	err := renderer.Render(context.Background(), workspace, "timeout-render.pdf", 1, 1, func([]Page) error { return nil })
	if err == nil {
		t.Fatal("Render() error = nil")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("internal timeout exposed as caller deadline: %v", err)
	}
	if time.Since(started) > time.Second {
		t.Fatal("Render() did not terminate promptly")
	}
	time.Sleep(100 * time.Millisecond)
	if _, statErr := os.Stat(marker); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("timed-out command completed, os.Stat() error = %v", statErr)
	}
}

func TestRenderInternalTimeoutDoesNotWaitForDetachedPipeHolder(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("requires Linux process session semantics")
	}
	setsid, err := exec.LookPath("setsid")
	if err != nil {
		t.Skip("setsid is unavailable")
	}
	marker := filepath.Join(t.TempDir(), "descendant-completed")
	executable := writeRenderExecutable(t, shellQuote(setsid)+" /bin/sh -c "+shellQuote("sleep 1; : >"+shellQuote(marker))+" &\nsleep 5\n")
	renderer := newTestRenderer(t, RenderOptions{Executable: executable, Timeout: 30 * time.Millisecond})
	workspace := newWorkspaceFile(t, "detached-render.pdf", []byte("safe"))

	started := time.Now()
	err = renderer.Render(context.Background(), workspace, "detached-render.pdf", 1, 1, func([]Page) error { return nil })
	assertRenderingError(t, err)
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("Render() elapsed = %v, want at most 500ms", elapsed)
	}
	waitForFile(t, marker)
}

func TestRenderRealPDFOnePage(t *testing.T) {
	renderer := newTestRenderer(t, RenderOptions{Executable: requirePoppler(t), Timeout: 10 * time.Second})
	workspace := newTestWorkspace(t, context.Background(), 80, WorkspaceOptions{TemporaryByteBudget: testByteBudget})
	copyFixture(t, workspace, "one-page.pdf", "one-page.pdf")
	var path string

	if err := renderer.Render(context.Background(), workspace, "one-page.pdf", 1, 1, func(pages []Page) error {
		if len(pages) != 1 || pages[0].Number != 1 || filepath.Base(pages[0].Path) != "page-000001.png" {
			return fmt.Errorf("pages = %+v", pages)
		}
		path = pages[0].Path
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if len(content) < len(pngSignature) || !strings.HasPrefix(string(content), string(pngSignature[:])) {
			return errors.New("invalid PNG signature")
		}
		return nil
	}); err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("os.Stat(callback path) error = %v, want os.ErrNotExist", err)
	}
}

func TestRenderRealPDFRangeAndSequentialReuse(t *testing.T) {
	renderer := newTestRenderer(t, RenderOptions{Executable: requirePoppler(t), Timeout: 10 * time.Second})
	workspace := newTestWorkspace(t, context.Background(), 81, WorkspaceOptions{TemporaryByteBudget: testByteBudget})
	copyFixture(t, workspace, "multi-page.pdf", "multi-page.pdf")

	for iteration := range 2 {
		var paths []string
		if err := renderer.Render(context.Background(), workspace, "multi-page.pdf", 2, 3, func(pages []Page) error {
			if len(pages) != 2 || pages[0].Number != 2 || pages[1].Number != 3 {
				return fmt.Errorf("iteration %d pages = %+v", iteration, pages)
			}
			for index, page := range pages {
				if filepath.Base(page.Path) != fmt.Sprintf("page-%06d.png", index+2) {
					return fmt.Errorf("page path = %q", page.Path)
				}
				paths = append(paths, page.Path)
			}
			return nil
		}); err != nil {
			t.Fatalf("iteration %d Render() error = %v", iteration, err)
		}
		for _, path := range paths {
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("os.Stat(%q) error = %v, want os.ErrNotExist", path, err)
			}
		}
	}
}

func newTestRenderer(t *testing.T, options RenderOptions) *Renderer {
	t.Helper()
	renderer, err := NewRenderer(options)
	if err != nil {
		t.Fatalf("NewRenderer() error = %v", err)
	}
	return renderer
}

func requirePoppler(t *testing.T) string {
	t.Helper()
	const executable = "/usr/bin/pdftoppm"
	if _, err := os.Stat(executable); err != nil {
		t.Skipf("pdftoppm unavailable: %v", err)
	}
	return executable
}

func writeRenderExecutable(t *testing.T, body string) string {
	t.Helper()
	return writeFile(t, "fake-pdftoppm-"+strconv.FormatInt(time.Now().UnixNano(), 10), []byte("#!/bin/sh\n"+body), 0o700)
}
