package pdf

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nosovk/paperless-ai-ocr/internal/saferr"
)

const (
	defaultRenderDPI     = 200
	defaultRenderTimeout = 5 * time.Minute
)

var pngSignature = [8]byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}

// RenderOptions controls pdftoppm execution.
type RenderOptions struct {
	Executable string
	DPI        int
	Timeout    time.Duration
}

// Page describes a rendered page available during a render callback.
type Page struct {
	Number int
	Path   string
	Size   int64
}

// Renderer safely invokes pdftoppm for requested page ranges.
type Renderer struct {
	executable string
	dpi        int
	timeout    time.Duration
}

// NewRenderer validates and resolves a renderer configuration.
func NewRenderer(options RenderOptions) (*Renderer, error) {
	executable := options.Executable
	if executable == "" {
		resolved, err := exec.LookPath("pdftoppm")
		if err != nil {
			return nil, saferr.New(saferr.CategoryRendering, "PDF renderer is unavailable")
		}
		executable, err = filepath.Abs(resolved)
		if err != nil {
			return nil, saferr.New(saferr.CategoryRendering, "PDF renderer is unavailable")
		}
	} else if !filepath.IsAbs(executable) {
		return nil, saferr.New(saferr.CategoryRendering, "invalid PDF renderer executable")
	}

	resolved, err := filepath.EvalSymlinks(executable)
	if err != nil {
		return nil, saferr.New(saferr.CategoryRendering, "PDF renderer is unavailable")
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return nil, saferr.New(saferr.CategoryRendering, "PDF renderer is unavailable")
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return nil, saferr.New(saferr.CategoryRendering, "invalid PDF renderer executable")
	}

	dpi := options.DPI
	if dpi == 0 {
		dpi = defaultRenderDPI
	}
	if dpi < 0 {
		return nil, saferr.New(saferr.CategoryRendering, "invalid PDF render DPI")
	}
	timeout := options.Timeout
	if timeout == 0 {
		timeout = defaultRenderTimeout
	}
	if timeout < 0 {
		return nil, saferr.New(saferr.CategoryRendering, "invalid PDF render timeout")
	}
	return &Renderer{executable: resolved, dpi: dpi, timeout: timeout}, nil
}

// Render renders a complete ordered page batch for the duration of visit.
func (renderer *Renderer) Render(ctx context.Context, workspace *Workspace, sourceName string, firstPage, lastPage int, visit func([]Page) error) (err error) {
	if renderer == nil || ctx == nil || workspace == nil || visit == nil {
		return saferr.New(saferr.CategoryRendering, "PDF rendering failed")
	}
	if err := ctx.Err(); err != nil {
		return renderingError("PDF rendering canceled", err)
	}
	if firstPage <= 0 || lastPage <= 0 || firstPage > lastPage {
		return saferr.New(saferr.CategoryRendering, "invalid PDF page range")
	}
	sourcePath, err := workspace.ownedPath(sourceName)
	if err != nil {
		return err
	}
	lease, err := workspace.reserveRemaining(ctx)
	if err != nil {
		return err
	}
	defer lease.release()
	batchDir, err := os.MkdirTemp(workspace.dir, "render-batch-*")
	if err != nil {
		return saferr.New(saferr.CategoryRendering, "PDF render workspace setup failed")
	}
	defer func() {
		if cleanupErr := os.RemoveAll(batchDir); cleanupErr != nil && err == nil {
			err = saferr.New(saferr.CategoryRendering, "PDF render cleanup failed")
		}
	}()
	if err := os.Chmod(batchDir, 0o700); err != nil {
		return saferr.New(saferr.CategoryRendering, "PDF render workspace setup failed")
	}

	outputPrefix := filepath.Join(batchDir, "render")
	commandContext, cancel := context.WithTimeout(ctx, renderer.timeout)
	defer cancel()
	cmd := exec.CommandContext(commandContext, renderer.executable,
		"-f", strconv.Itoa(firstPage), "-l", strconv.Itoa(lastPage),
		"-r", strconv.Itoa(renderer.dpi), "-png", sourcePath, outputPrefix,
	)
	cmd.Env = []string{"LANG=C", "LC_ALL=C", "PATH=/usr/bin:/bin"}
	cmd.WaitDelay = commandWaitDelay
	configureProcessTermination(cmd)
	var stdout boundedBuffer
	stdout.limit = stderrLimit
	var stderr boundedBuffer
	stderr.limit = stderrLimit
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if runErr := cmd.Run(); runErr != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return renderingError("PDF rendering canceled", ctxErr)
		}
		if errors.Is(commandContext.Err(), context.DeadlineExceeded) {
			return saferr.New(saferr.CategoryRendering, "PDF rendering timed out")
		}
		return renderingError("PDF rendering failed", renderCommandFailure{})
	}

	pages, err := collectRenderedPages(batchDir, firstPage, lastPage, lease.bytes)
	if err != nil {
		return err
	}
	var outputBytes int64
	for _, page := range pages {
		outputBytes += page.Size
	}
	if err := lease.resize(outputBytes); err != nil {
		return err
	}
	if callbackErr := visit(pages); callbackErr != nil {
		return renderingError("PDF render callback failed", callbackFailure{err: callbackErr})
	}
	return nil
}

type callbackFailure struct {
	err error
}

func (callbackFailure) Error() string {
	return "PDF render callback failed"
}

func (failure callbackFailure) Format(state fmt.State, verb rune) {
	io.WriteString(state, failure.Error())
}

func (failure callbackFailure) Is(target error) bool {
	return errors.Is(failure.err, target)
}

type renderCommandFailure struct{}

func (renderCommandFailure) Error() string {
	return "PDF rendering command failed"
}

func (failure renderCommandFailure) Format(state fmt.State, verb rune) {
	io.WriteString(state, failure.Error())
}

func collectRenderedPages(batchDir string, firstPage, lastPage int, budget int64) ([]Page, error) {
	entries, err := os.ReadDir(batchDir)
	if err != nil {
		return nil, saferr.New(saferr.CategoryRendering, "PDF render output validation failed")
	}
	pages := make([]Page, 0, len(entries))
	seen := make(map[int]struct{}, len(entries))
	var total int64
	for _, entry := range entries {
		number, ok := parseRenderedPageName(entry.Name())
		if !ok || number < firstPage || number > lastPage {
			return nil, saferr.New(saferr.CategoryRendering, "PDF renderer returned unexpected output")
		}
		if _, duplicate := seen[number]; duplicate {
			return nil, saferr.New(saferr.CategoryRendering, "PDF renderer returned duplicate output")
		}
		seen[number] = struct{}{}
		path := filepath.Join(batchDir, entry.Name())
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || info.Size() > budget {
			return nil, saferr.New(saferr.CategoryRendering, "PDF renderer returned invalid output")
		}
		if info.Size() > budget-total {
			return nil, saferr.New(saferr.CategoryRendering, "PDF renderer exceeded temporary byte budget")
		}
		total += info.Size()
		file, err := os.Open(path)
		if err != nil {
			return nil, saferr.New(saferr.CategoryRendering, "PDF renderer returned invalid output")
		}
		var signature [8]byte
		_, readErr := io.ReadFull(file, signature[:])
		closeErr := file.Close()
		if readErr != nil || closeErr != nil || !bytes.Equal(signature[:], pngSignature[:]) {
			return nil, saferr.New(saferr.CategoryRendering, "PDF renderer returned invalid PNG output")
		}
		deterministicPath := filepath.Join(batchDir, fmt.Sprintf("page-%06d.png", number))
		if err := os.Rename(path, deterministicPath); err != nil {
			return nil, saferr.New(saferr.CategoryRendering, "PDF render output preparation failed")
		}
		pages = append(pages, Page{Number: number, Path: deterministicPath, Size: info.Size()})
	}
	expected := int64(lastPage) - int64(firstPage) + 1
	if int64(len(pages)) != expected {
		return nil, saferr.New(saferr.CategoryRendering, "PDF renderer returned incomplete output")
	}
	sort.Slice(pages, func(i, j int) bool { return pages[i].Number < pages[j].Number })
	for index, page := range pages {
		if page.Number != firstPage+index {
			return nil, saferr.New(saferr.CategoryRendering, "PDF renderer returned incomplete output")
		}
	}
	return pages, nil
}

func parseRenderedPageName(name string) (int, bool) {
	if !strings.HasPrefix(name, "render-") || !strings.HasSuffix(name, ".png") {
		return 0, false
	}
	digits := strings.TrimSuffix(strings.TrimPrefix(name, "render-"), ".png")
	if digits == "" {
		return 0, false
	}
	for index := range len(digits) {
		if digits[index] < '0' || digits[index] > '9' {
			return 0, false
		}
	}
	number, err := strconv.ParseInt(digits, 10, strconv.IntSize)
	return int(number), err == nil && number > 0
}
