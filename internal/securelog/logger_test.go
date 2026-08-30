package securelog

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nosovk/paperless-ai-ocr/internal/queue"
	"github.com/nosovk/paperless-ai-ocr/internal/saferr"
)

func TestLoggerWritesOnlyAllowlistedJSONFields(t *testing.T) {
	var output bytes.Buffer
	logger := New(&output)

	for _, call := range []func() error{
		logger.Startup,
		logger.Ready,
		func() error { return logger.JobClaimed(42) },
		func() error { return logger.BatchCompleted(42, 2, 4, 1250*time.Millisecond) },
		func() error { return logger.JobFinished(42, queue.StateCompleted, 2*time.Second) },
		func() error { return logger.BackgroundFailure(saferr.CategoryProvider) },
		logger.Shutdown,
	} {
		if err := call(); err != nil {
			t.Fatalf("log call: %v", err)
		}
	}

	want := []string{
		`{"level":"info","event":"startup"}`,
		`{"level":"info","event":"ready"}`,
		`{"level":"info","event":"job_claimed","document_id":42,"state":"processing"}`,
		`{"level":"info","event":"batch_completed","document_id":42,"first_page":2,"last_page":4,"duration_ms":1250,"state":"completed"}`,
		`{"level":"info","event":"job_finished","document_id":42,"duration_ms":2000,"state":"completed"}`,
		`{"level":"error","event":"background_failure","category":"provider"}`,
		`{"level":"info","event":"shutdown"}`,
	}
	if got := strings.Split(strings.TrimSuffix(output.String(), "\n"), "\n"); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("log lines =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	for _, line := range strings.Split(strings.TrimSpace(output.String()), "\n") {
		var fields map[string]any
		if err := json.Unmarshal([]byte(line), &fields); err != nil {
			t.Fatalf("invalid JSON line %q: %v", line, err)
		}
		for key := range fields {
			switch key {
			case "level", "event", "document_id", "first_page", "last_page", "duration_ms", "state", "category":
			default:
				t.Errorf("non-allowlisted key %q in %q", key, line)
			}
		}
	}
}

func TestLoggerRejectsInvalidTypedValuesWithoutWriting(t *testing.T) {
	var output bytes.Buffer
	logger := New(&output)
	for name, call := range map[string]func() error{
		"document ID": func() error { return logger.JobClaimed(0) },
		"page range":  func() error { return logger.BatchCompleted(1, 0, 2, time.Second) },
		"page span":   func() error { return logger.BatchCompleted(1, 1, 6, time.Second) },
		"page bound":  func() error { return logger.BatchCompleted(1, 10_001, 10_001, time.Second) },
		"duration":    func() error { return logger.JobFinished(1, queue.StateCompleted, -1) },
		"duration bound": func() error {
			return logger.JobFinished(1, queue.StateCompleted, 24*time.Hour+time.Nanosecond)
		},
		"state":    func() error { return logger.JobFinished(1, queue.State("completed\nCANARY"), time.Second) },
		"category": func() error { return logger.BackgroundFailure(saferr.Category("provider\nCANARY")) },
	} {
		t.Run(name, func(t *testing.T) {
			if err := call(); err == nil || strings.Contains(err.Error(), "CANARY") {
				t.Fatalf("error = %v, want safe validation error", err)
			}
		})
	}
	if output.Len() != 0 {
		t.Fatalf("invalid entries wrote %q", output.String())
	}
}

func TestLoggerSerializesConcurrentWritesAsJSONLines(t *testing.T) {
	var output bytes.Buffer
	logger := New(&output)
	const writers = 50
	var wait sync.WaitGroup
	for documentID := int64(1); documentID <= writers; documentID++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			if err := logger.JobClaimed(documentID); err != nil {
				t.Errorf("JobClaimed(): %v", err)
			}
		}()
	}
	wait.Wait()
	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	if len(lines) != writers {
		t.Fatalf("line count = %d, want %d", len(lines), writers)
	}
	for _, line := range lines {
		var entry struct {
			Event      string `json:"event"`
			DocumentID int64  `json:"document_id"`
		}
		if err := json.Unmarshal([]byte(line), &entry); err != nil || entry.Event != "job_claimed" || entry.DocumentID <= 0 {
			t.Errorf("invalid concurrent line %q: entry=%+v err=%v", line, entry, err)
		}
	}
}

func TestLoggerWriterFailureDoesNotDiscloseCause(t *testing.T) {
	const canary = "CANARY writer path /private/log and token"
	logger := New(errorWriter{err: errors.New(canary)})
	err := logger.Startup()
	if err == nil {
		t.Fatal("Startup() error = nil")
	}
	for _, formatted := range []string{err.Error(), fmt.Sprintf("%s", err), fmt.Sprintf("%v", err), fmt.Sprintf("%+v", err), fmt.Sprintf("%q", err)} {
		if strings.Contains(formatted, canary) {
			t.Errorf("writer error disclosed in %q", formatted)
		}
	}
	if errors.Is(err, io.ErrClosedPipe) || errors.Unwrap(err) != nil {
		t.Errorf("writer cause remains traversable: %v", err)
	}
}

func TestLoggerRejectsShortWrite(t *testing.T) {
	err := New(shortWriter{}).Startup()
	if err == nil || err.Error() != "secure log write failed" {
		t.Fatalf("Startup() error = %v, want safe write failure", err)
	}
}

type errorWriter struct{ err error }

func (writer errorWriter) Write([]byte) (int, error) { return 0, writer.err }

type shortWriter struct{}

func (shortWriter) Write(data []byte) (int, error) { return len(data) - 1, nil }
