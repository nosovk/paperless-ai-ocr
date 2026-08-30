package pdf

import (
	"context"
	"errors"
	"io"
	"os"
	"testing"
)

func TestWorkspaceOwnedFileAccountsIncrementallyAndRetainsSuccessfulBytes(t *testing.T) {
	workspace := newTestWorkspace(t, context.Background(), 90, WorkspaceOptions{TemporaryByteBudget: 5})
	file, err := workspace.Create(context.Background(), "source.pdf")
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if _, err := file.Write([]byte("safe")); err != nil {
		t.Fatalf("Write(4) error = %v", err)
	}
	if _, err := file.Write([]byte("!")); err != nil {
		t.Fatalf("Write(exact fit) error = %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if workspace.reserved != 5 {
		t.Fatalf("reserved = %d, want 5", workspace.reserved)
	}
	if _, err := workspace.reserve(context.Background(), 1); err == nil {
		t.Fatal("reserve after source exact fit error = nil")
	}
	if err := workspace.Close(); err != nil {
		t.Fatalf("Workspace.Close() error = %v", err)
	}
}

func TestWorkspaceOwnedFileOverflowCleansPartialAndReservation(t *testing.T) {
	workspace := newTestWorkspace(t, context.Background(), 91, WorkspaceOptions{TemporaryByteBudget: 4})
	file, err := workspace.Create(context.Background(), "source.pdf")
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	path := file.Name()
	if _, err := file.Write([]byte("safe!")); err == nil {
		t.Fatal("Write(+1) error = nil")
	}
	if workspace.reserved != 0 {
		t.Fatalf("reserved = %d, want 0", workspace.reserved)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("os.Stat(partial) error = %v, want os.ErrNotExist", err)
	}
	if err := workspace.Close(); err != nil {
		t.Fatalf("Workspace.Close() error = %v", err)
	}
}

func TestWorkspaceOwnedFileCancellationAndIOFailureCleanup(t *testing.T) {
	for _, test := range []struct {
		name  string
		setup func(context.CancelFunc, *OwnedFile)
	}{
		{name: "cancellation", setup: func(cancel context.CancelFunc, _ *OwnedFile) { cancel() }},
		{name: "IO failure", setup: func(_ context.CancelFunc, file *OwnedFile) { _ = file.file.Close() }},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			workspace := newTestWorkspace(t, context.Background(), 92, WorkspaceOptions{TemporaryByteBudget: 10})
			file, err := workspace.Create(ctx, "source.pdf")
			if err != nil {
				t.Fatalf("Create() error = %v", err)
			}
			test.setup(cancel, file)
			if _, err := io.WriteString(file, "safe"); err == nil {
				t.Fatal("Write() error = nil")
			}
			if workspace.reserved != 0 {
				t.Fatalf("reserved = %d, want 0", workspace.reserved)
			}
			if _, err := os.Stat(file.Name()); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("os.Stat(partial) error = %v, want os.ErrNotExist", err)
			}
			if err := workspace.Close(); err != nil {
				t.Fatalf("Workspace.Close() error = %v", err)
			}
		})
	}
}
