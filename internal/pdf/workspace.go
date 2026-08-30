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
	active         int
	availableBytes func(string) (int64, error)
	paths          map[string]string
	closed         bool
}

type reservation struct {
	workspace *Workspace
	bytes     int64
	released  bool
}

// OwnedFile is a workspace-owned file whose written bytes are charged to the
// workspace budget. Successful bytes remain reserved until Workspace.Close.
type OwnedFile struct {
	workspace   *Workspace
	ctx         context.Context
	file        *os.File
	reservation *reservation
	closed      bool
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

// Create opens a new workspace-owned file with incremental byte accounting.
func (workspace *Workspace) Create(ctx context.Context, name string) (*OwnedFile, error) {
	if ctx == nil {
		return nil, saferr.New(saferr.CategoryRendering, "workspace file creation failed")
	}
	path, err := workspace.Path(name)
	if err != nil {
		return nil, err
	}
	lease, err := workspace.reserve(ctx, 0)
	if err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		lease.release()
		return nil, saferr.New(saferr.CategoryRendering, "workspace file creation failed")
	}
	return &OwnedFile{workspace: workspace, ctx: ctx, file: file, reservation: lease}, nil
}

func (workspace *Workspace) reserve(ctx context.Context, bytes int64) (*reservation, error) {
	if workspace == nil || ctx == nil {
		return nil, saferr.New(saferr.CategoryRendering, "temporary storage reservation failed")
	}
	if err := ctx.Err(); err != nil {
		return nil, renderingError("temporary storage reservation canceled", err)
	}
	if bytes < 0 {
		return nil, saferr.New(saferr.CategoryRendering, "invalid temporary storage reservation")
	}

	workspace.mu.Lock()
	defer workspace.mu.Unlock()
	if workspace.closed {
		return nil, saferr.New(saferr.CategoryRendering, "workspace is closed")
	}
	if bytes > math.MaxInt64-workspace.reserved || workspace.reserved+bytes > workspace.budget {
		return nil, saferr.New(saferr.CategoryRendering, "temporary byte budget exceeded")
	}
	available, err := workspace.availableBytes(workspace.dir)
	if err != nil {
		return nil, renderingError("workspace storage check failed", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, renderingError("temporary storage reservation canceled", err)
	}
	if available < 0 || bytes > math.MaxInt64-workspace.minimumFree || available < bytes+workspace.minimumFree {
		return nil, saferr.New(saferr.CategoryRendering, "insufficient temporary storage")
	}
	workspace.reserved += bytes
	workspace.active++
	return &reservation{workspace: workspace, bytes: bytes}, nil
}

func (reservation *reservation) grow(ctx context.Context, bytes int64) error {
	if reservation == nil || reservation.workspace == nil || bytes < 0 {
		return saferr.New(saferr.CategoryRendering, "temporary storage reservation failed")
	}
	if bytes == 0 {
		return nil
	}
	workspace := reservation.workspace
	workspace.mu.Lock()
	defer workspace.mu.Unlock()
	if reservation.released || workspace.closed {
		return saferr.New(saferr.CategoryRendering, "temporary storage reservation failed")
	}
	if err := ctx.Err(); err != nil {
		return renderingError("temporary storage reservation canceled", err)
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
	reservation.bytes += bytes
	return nil
}

func (reservation *reservation) accountExisting(ctx context.Context, bytes int64) error {
	if reservation == nil || reservation.workspace == nil || bytes < 0 {
		return saferr.New(saferr.CategoryRendering, "temporary storage reservation failed")
	}
	workspace := reservation.workspace
	workspace.mu.Lock()
	defer workspace.mu.Unlock()
	if reservation.released || workspace.closed {
		return saferr.New(saferr.CategoryRendering, "temporary storage reservation failed")
	}
	if err := ctx.Err(); err != nil {
		return renderingError("temporary storage reservation canceled", err)
	}
	if bytes > math.MaxInt64-workspace.reserved || workspace.reserved+bytes > workspace.budget {
		return saferr.New(saferr.CategoryRendering, "temporary byte budget exceeded")
	}
	workspace.reserved += bytes
	reservation.bytes += bytes
	return nil
}

func (reservation *reservation) shrink(bytes int64) {
	if reservation == nil || reservation.workspace == nil || bytes <= 0 {
		return
	}
	workspace := reservation.workspace
	workspace.mu.Lock()
	defer workspace.mu.Unlock()
	if reservation.released {
		return
	}
	bytes = min(bytes, reservation.bytes)
	reservation.bytes -= bytes
	workspace.reserved -= bytes
}

func (reservation *reservation) release() {
	if reservation == nil || reservation.workspace == nil {
		return
	}
	workspace := reservation.workspace
	workspace.mu.Lock()
	defer workspace.mu.Unlock()
	if reservation.released {
		return
	}
	reservation.released = true
	workspace.reserved = max(0, workspace.reserved-reservation.bytes)
	workspace.active = max(0, workspace.active-1)
}

func (reservation *reservation) retain() {
	if reservation == nil || reservation.workspace == nil {
		return
	}
	workspace := reservation.workspace
	workspace.mu.Lock()
	defer workspace.mu.Unlock()
	if reservation.released {
		return
	}
	reservation.released = true
	workspace.active = max(0, workspace.active-1)
}

// Write writes without buffering the full file and reserves only bytes accepted.
func (file *OwnedFile) Write(data []byte) (int, error) {
	if file == nil || file.closed {
		return 0, saferr.New(saferr.CategoryRendering, "workspace file write failed")
	}
	if err := file.reservation.grow(file.ctx, int64(len(data))); err != nil {
		_ = file.Abort()
		return 0, err
	}
	written, err := file.file.Write(data)
	if err != nil || written != len(data) {
		file.reservation.shrink(int64(len(data) - written))
		_ = file.Abort()
		return written, saferr.New(saferr.CategoryRendering, "workspace file write failed")
	}
	return written, nil
}

// Sync flushes the owned file to storage.
func (file *OwnedFile) Sync() error {
	if file == nil || file.closed || file.file.Sync() != nil {
		if file != nil {
			_ = file.Abort()
		}
		return saferr.New(saferr.CategoryRendering, "workspace file sync failed")
	}
	return nil
}

// Name returns the workspace-owned path.
func (file *OwnedFile) Name() string {
	if file == nil || file.file == nil {
		return ""
	}
	return file.file.Name()
}

// Close closes the file while retaining its byte reservation until workspace cleanup.
func (file *OwnedFile) Close() error {
	if file == nil || file.closed {
		return nil
	}
	file.closed = true
	if err := file.file.Close(); err != nil {
		file.reservation.release()
		_ = os.Remove(file.file.Name())
		return saferr.New(saferr.CategoryRendering, "workspace file close failed")
	}
	file.reservation.retain()
	return nil
}

// Abort removes a partial file and releases all bytes reserved for it.
func (file *OwnedFile) Abort() error {
	if file == nil || file.closed {
		return nil
	}
	file.closed = true
	_ = file.file.Close()
	removeErr := os.Remove(file.file.Name())
	file.reservation.release()
	if removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
		return saferr.New(saferr.CategoryRendering, "workspace file cleanup failed")
	}
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
	if workspace.active != 0 {
		return saferr.New(saferr.CategoryRendering, "workspace is in use")
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
