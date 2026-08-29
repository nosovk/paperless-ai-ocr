package pdf

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/nosovk/paperless-ai-ocr/internal/saferr"
)

const (
	defaultInspectTimeout = 30 * time.Second
	commandWaitDelay      = 200 * time.Millisecond
	metadataLimit         = 64 << 10
	stderrLimit           = 16 << 10
)

// InspectOptions controls pdfinfo execution.
type InspectOptions struct {
	Executable string
	Timeout    time.Duration
}

// Info contains inspected PDF metadata.
type Info struct {
	Pages int
}

// Inspector safely invokes pdfinfo.
type Inspector struct {
	executable string
	timeout    time.Duration
}

// NewInspector validates and resolves an inspector configuration.
func NewInspector(options InspectOptions) (*Inspector, error) {
	executable := options.Executable
	if executable == "" {
		resolved, err := exec.LookPath("pdfinfo")
		if err != nil {
			return nil, saferr.New(saferr.CategoryRendering, "PDF inspector is unavailable")
		}
		executable, err = filepath.Abs(resolved)
		if err != nil {
			return nil, saferr.New(saferr.CategoryRendering, "PDF inspector is unavailable")
		}
	} else if !filepath.IsAbs(executable) {
		return nil, saferr.New(saferr.CategoryRendering, "invalid PDF inspector executable")
	}

	resolved, err := filepath.EvalSymlinks(executable)
	if err != nil {
		return nil, saferr.New(saferr.CategoryRendering, "PDF inspector is unavailable")
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return nil, saferr.New(saferr.CategoryRendering, "PDF inspector is unavailable")
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return nil, saferr.New(saferr.CategoryRendering, "invalid PDF inspector executable")
	}

	timeout := options.Timeout
	if timeout == 0 {
		timeout = defaultInspectTimeout
	}
	if timeout < 0 {
		return nil, saferr.New(saferr.CategoryRendering, "invalid PDF inspection timeout")
	}
	return &Inspector{executable: resolved, timeout: timeout}, nil
}

// Inspect returns metadata for a workspace-owned PDF.
func (inspector *Inspector) Inspect(ctx context.Context, workspace *Workspace, name string) (Info, error) {
	if inspector == nil || ctx == nil {
		return Info{}, saferr.New(saferr.CategoryRendering, "PDF inspection failed")
	}
	if err := ctx.Err(); err != nil {
		return Info{}, renderingError("PDF inspection canceled", err)
	}
	path, err := workspace.ownedPath(name)
	if err != nil {
		return Info{}, err
	}

	commandContext, cancel := context.WithTimeout(ctx, inspector.timeout)
	defer cancel()
	cmd := exec.CommandContext(commandContext, inspector.executable, path)
	cmd.Env = []string{"LANG=C", "LC_ALL=C", "PATH=/usr/bin:/bin"}
	cmd.WaitDelay = commandWaitDelay
	configureProcessTermination(cmd)
	var stdout boundedBuffer
	stdout.limit = metadataLimit
	var stderr boundedBuffer
	stderr.limit = stderrLimit
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return Info{}, renderingError("PDF inspection canceled", ctxErr)
		}
		if errors.Is(commandContext.Err(), context.DeadlineExceeded) {
			return Info{}, saferr.New(saferr.CategoryRendering, "PDF inspection timed out")
		}
		return Info{}, renderingError("PDF inspection failed", commandFailure{err: err, stderr: stderr.String()})
	}

	pages, err := parsePages(stdout.String())
	if stdout.truncated {
		return Info{}, saferr.New(saferr.CategoryRendering, "PDF inspection returned excessive metadata")
	}
	if err != nil {
		return Info{}, renderingError("PDF inspection returned invalid metadata", err)
	}
	return Info{Pages: pages}, nil
}

type boundedBuffer struct {
	data      []byte
	limit     int
	truncated bool
}

func (buffer *boundedBuffer) Write(data []byte) (int, error) {
	remaining := buffer.limit - len(buffer.data)
	if remaining > 0 {
		buffer.data = append(buffer.data, data[:min(len(data), remaining)]...)
	}
	if len(data) > remaining {
		buffer.truncated = true
	}
	return len(data), nil
}

func (buffer *boundedBuffer) String() string {
	return string(buffer.data)
}

type commandFailure struct {
	err    error
	stderr string
}

func (failure commandFailure) Error() string {
	return "PDF inspection command failed"
}

func (failure commandFailure) Format(state fmt.State, verb rune) {
	io.WriteString(state, failure.Error())
}

func parsePages(metadata string) (int, error) {
	found := false
	value := ""
	for line := range strings.SplitSeq(metadata, "\n") {
		if !strings.HasPrefix(line, "Pages:") {
			continue
		}
		if found {
			return 0, errors.New("duplicate Pages field")
		}
		found = true
		value = strings.TrimSpace(strings.TrimPrefix(line, "Pages:"))
	}
	if !found || value == "" {
		return 0, errors.New("missing Pages field")
	}
	for index := range len(value) {
		if value[index] < '0' || value[index] > '9' {
			return 0, errors.New("invalid Pages field")
		}
	}
	pages, err := strconv.ParseInt(value, 10, strconv.IntSize)
	if err != nil || pages <= 0 {
		return 0, errors.New("invalid Pages field")
	}
	return int(pages), nil
}

func configureProcessTermination(cmd *exec.Cmd) {
	if runtime.GOOS == "windows" {
		return
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
}

var _ io.Writer = (*boundedBuffer)(nil)
