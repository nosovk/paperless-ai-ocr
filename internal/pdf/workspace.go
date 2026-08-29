// Package pdf provides protected PDF processing workspaces and inspection.
package pdf

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/nosovk/paperless-ai-ocr/internal/saferr"
	"golang.org/x/sys/unix"
)

// WorkspaceOptions controls temporary storage limits for a workspace.
type WorkspaceOptions struct {
	TemporaryByteBudget int64
	MinimumFreeBytes    int64
}

type workspaceHooks struct {
	root           string
	availableBytes func(string) (int64, error)
}

// Workspace owns a protected temporary directory for one job.
type Workspace struct {
	mu             sync.Mutex
	dir            string
	budget         int64
	minimumFree    int64
	reserved       int64
	availableBytes func(string) (int64, error)
	paths          map[string]string
	closed         bool
}

// NewWorkspace creates a protected workspace below the system temporary root.
func NewWorkspace(ctx context.Context, jobID int64, options WorkspaceOptions) (*Workspace, error) {
	return newWorkspace(ctx, jobID, options, workspaceHooks{})
}

func newWorkspace(ctx context.Context, jobID int64, options WorkspaceOptions, hooks workspaceHooks) (_ *Workspace, err error) {
	if ctx == nil {
		return nil, renderingError("workspace setup failed", errors.New("nil context"))
	}
	if err := ctx.Err(); err != nil {
		return nil, renderingError("workspace setup canceled", err)
	}
	if jobID <= 0 || options.TemporaryByteBudget <= 0 || options.MinimumFreeBytes < 0 {
		return nil, saferr.New(saferr.CategoryRendering, "invalid workspace options")
	}
	root := hooks.root
	if root == "" {
		root = os.TempDir()
	}
	availableBytes := hooks.availableBytes
	if availableBytes == nil {
		availableBytes = filesystemAvailableBytes
	}

	dir, err := os.MkdirTemp(root, "paperless-ai-ocr-"+strconv.FormatInt(jobID, 10)+"-*")
	if err != nil {
		return nil, renderingError("workspace setup failed", err)
	}
	cleanup := true
	defer func() {
		if cleanup {
			os.RemoveAll(dir)
		}
	}()
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, renderingError("workspace setup failed", err)
	}
	available, err := availableBytes(dir)
	if err != nil {
		return nil, renderingError("workspace storage check failed", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, renderingError("workspace setup canceled", err)
	}
	if available < 0 || available < options.MinimumFreeBytes {
		return nil, saferr.New(saferr.CategoryRendering, "insufficient temporary storage")
	}

	cleanup = false
	return &Workspace{
		dir:            dir,
		budget:         options.TemporaryByteBudget,
		minimumFree:    options.MinimumFreeBytes,
		availableBytes: availableBytes,
		paths:          make(map[string]string),
	}, nil
}

// Path returns a safe direct child path owned by the workspace.
func (workspace *Workspace) Path(name string) (string, error) {
	if workspace == nil || !validChildName(name) {
		return "", saferr.New(saferr.CategoryRendering, "invalid workspace filename")
	}
	workspace.mu.Lock()
	defer workspace.mu.Unlock()
	if workspace.closed {
		return "", saferr.New(saferr.CategoryRendering, "workspace is closed")
	}
	path := filepath.Join(workspace.dir, name)
	workspace.paths[name] = path
	return path, nil
}

// Reserve accounts for temporary bytes before external work starts.
func (workspace *Workspace) Reserve(ctx context.Context, bytes int64) error {
	if workspace == nil || ctx == nil {
		return saferr.New(saferr.CategoryRendering, "temporary storage reservation failed")
	}
	if err := ctx.Err(); err != nil {
		return renderingError("temporary storage reservation canceled", err)
	}
	if bytes < 0 {
		return saferr.New(saferr.CategoryRendering, "invalid temporary storage reservation")
	}

	workspace.mu.Lock()
	defer workspace.mu.Unlock()
	if workspace.closed {
		return saferr.New(saferr.CategoryRendering, "workspace is closed")
	}
	if bytes > math.MaxInt64-workspace.reserved || workspace.reserved+bytes > workspace.budget {
		return saferr.New(saferr.CategoryRendering, "temporary byte budget exceeded")
	}
	available, err := workspace.availableBytes(workspace.dir)
	if err != nil {
		return renderingError("workspace storage check failed", err)
	}
	if err := ctx.Err(); err != nil {
		return renderingError("temporary storage reservation canceled", err)
	}
	if available < 0 || bytes > math.MaxInt64-workspace.minimumFree || available < bytes+workspace.minimumFree {
		return saferr.New(saferr.CategoryRendering, "insufficient temporary storage")
	}
	workspace.reserved += bytes
	return nil
}

// Close recursively removes the workspace. Repeated calls are safe.
func (workspace *Workspace) Close() error {
	if workspace == nil {
		return nil
	}
	workspace.mu.Lock()
	defer workspace.mu.Unlock()
	if workspace.closed {
		return nil
	}
	if err := os.RemoveAll(workspace.dir); err != nil {
		return renderingError("workspace cleanup failed", err)
	}
	workspace.closed = true
	return nil
}

func (workspace *Workspace) ownedPath(name string) (string, error) {
	if workspace == nil || !validChildName(name) {
		return "", saferr.New(saferr.CategoryRendering, "invalid workspace filename")
	}
	workspace.mu.Lock()
	defer workspace.mu.Unlock()
	if workspace.closed {
		return "", saferr.New(saferr.CategoryRendering, "workspace is closed")
	}
	path, ok := workspace.paths[name]
	if !ok {
		return "", saferr.New(saferr.CategoryRendering, "file is not owned by workspace")
	}
	return path, nil
}

func validChildName(name string) bool {
	return strings.TrimSpace(name) != "" && name != "." && name != ".." && !filepath.IsAbs(name) &&
		!strings.ContainsAny(name, `/\\`) && filepath.Base(name) == name
}

func filesystemAvailableBytes(path string) (int64, error) {
	var stats unix.Statfs_t
	if err := unix.Statfs(path, &stats); err != nil {
		return 0, err
	}
	if stats.Bavail > math.MaxInt64/uint64(stats.Bsize) {
		return math.MaxInt64, nil
	}
	return int64(stats.Bavail * uint64(stats.Bsize)), nil
}

func renderingError(message string, cause error) error {
	return saferr.Wrap(saferr.CategoryRendering, message, cause)
}
