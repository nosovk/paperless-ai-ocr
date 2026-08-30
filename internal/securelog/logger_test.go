package securelog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
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
		"document ID":        func() error { return logger.JobClaimed(0) },
		"page range":         func() error { return logger.BatchCompleted(1, 0, 2, time.Second) },
		"page span":          func() error { return logger.BatchCompleted(1, 1, 6, time.Second) },
		"page bound":         func() error { return logger.BatchCompleted(1, 10_001, 10_001, time.Second) },
		"duration":           func() error { return logger.JobFinished(1, queue.StateCompleted, -1) },
		"pending outcome":    func() error { return logger.JobFinished(1, queue.StatePending, time.Second) },
		"processing outcome": func() error { return logger.JobFinished(1, queue.StateProcessing, time.Second) },
		"state":              func() error { return logger.JobFinished(1, queue.State("completed\nCANARY"), time.Second) },
		"category":           func() error { return logger.BackgroundFailure(saferr.Category("provider\nCANARY")) },
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

func TestLoggerAcceptsLongDuration(t *testing.T) {
	var output bytes.Buffer
	logger := New(&output)
	duration := 72*time.Hour + 123*time.Millisecond
	if err := logger.JobFinished(1, queue.StateCompleted, duration); err != nil {
		t.Fatalf("JobFinished() error = %v", err)
	}
	if !strings.Contains(output.String(), `"duration_ms":259200123`) {
		t.Fatalf("log = %q, want long duration", output.String())
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

func TestLoggerWriterPanicIsSafe(t *testing.T) {
	const canary = "CANARY synchronous writer panic"
	logger := New(panicValueWriter{value: canary})
	var err error
	func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				t.Fatalf("Startup() panic = %v", recovered)
			}
		}()
		err = logger.Startup()
	}()
	if err == nil || err.Error() != "secure log write failed" {
		t.Fatalf("Startup() error = %v", err)
	}
	if strings.Contains(fmt.Sprintf("%v", err), canary) || errors.Unwrap(err) != nil {
		t.Fatalf("panic data exposed: %v", err)
	}
	if err := logger.Ready(); err == nil {
		t.Fatal("logger accepted entry after writer panic")
	}
}

func TestLoggerRejectsShortWrite(t *testing.T) {
	err := New(shortWriter{}).Startup()
	if err == nil || err.Error() != "secure log write failed" {
		t.Fatalf("Startup() error = %v, want safe write failure", err)
	}
}

func TestSynchronousLoggerFailurePermanentlyPoisonsWriter(t *testing.T) {
	const canary = "CANARY one-shot writer failure"
	for _, test := range []struct {
		name   string
		writer *oneShotFailureWriter
	}{
		{
			name: "partial error",
			writer: &oneShotFailureWriter{
				mode: failurePartialError,
				err:  errors.New(canary),
			},
		},
		{
			name: "partial panic",
			writer: &oneShotFailureWriter{
				mode:       failurePartialPanic,
				panicValue: canary,
			},
		},
		{
			name: "zero-byte error",
			writer: &oneShotFailureWriter{
				mode: failureZeroByteError,
				err:  errors.New(canary),
			},
		},
		{
			name: "invalid progress",
			writer: &oneShotFailureWriter{
				mode: failureZeroByteNoError,
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			logger := New(test.writer)
			assertSafeWriteError(t, logger.Startup(), canary)
			assertSafeWriteError(t, logger.Ready(), canary)
			assertSafeWriteError(t, logger.Shutdown(), canary)
			if calls := test.writer.calls.Load(); calls != 1 {
				t.Fatalf("writer calls = %d, want 1", calls)
			}
		})
	}
}

func assertSafeWriteError(t *testing.T, err error, canaries ...string) {
	t.Helper()
	if err != errWriteEntry {
		t.Fatalf("error = %v, want secure log write failure", err)
	}
	for _, formatted := range []string{err.Error(), fmt.Sprintf("%s", err), fmt.Sprintf("%v", err), fmt.Sprintf("%+v", err), fmt.Sprintf("%q", err)} {
		for _, canary := range canaries {
			if strings.Contains(formatted, canary) {
				t.Fatalf("error disclosed canary in %q", formatted)
			}
		}
	}
	if errors.Unwrap(err) != nil {
		t.Fatalf("writer cause remains traversable: %v", err)
	}
}

func TestAsyncLoggerCallsAreNonblockingAndDropsWhenFull(t *testing.T) {
	writer := newBlockingWriter()
	logger, err := NewAsync(writer, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.unblock()
	if err := logger.Startup(); err != nil {
		t.Fatal(err)
	}
	writer.waitStarted(t)
	if err := logger.Ready(); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if err := logger.JobClaimed(1); err == nil {
		t.Fatal("saturated logger error = nil")
	}
	if elapsed := time.Since(started); elapsed > 100*time.Millisecond {
		t.Fatalf("saturated logger blocked for %s", elapsed)
	}
	if logger.Dropped() == 0 {
		t.Fatal("drop count = 0")
	}
}

func TestAsyncLoggerPoisonedSinkDropsFurtherEntries(t *testing.T) {
	for _, test := range []struct {
		name   string
		writer io.Writer
	}{
		{name: "error", writer: errorWriter{err: errors.New("CANARY writer secret")}},
		{name: "short write", writer: &shortThenRecordWriter{}},
		{name: "panic", writer: panicWriter{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			logger, err := NewAsync(test.writer, 4)
			if err != nil {
				t.Fatal(err)
			}
			_ = logger.Startup()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if err := logger.Close(ctx); err != nil {
				t.Fatalf("Close() error = %v", err)
			}
			if err := logger.Ready(); err == nil {
				t.Fatal("poisoned/closed logger accepted entry")
			}
			if logger.Dropped() == 0 {
				t.Fatal("drop count = 0")
			}
		})
	}
}

func TestAsyncLoggerConcurrentCallsProduceCompleteLines(t *testing.T) {
	var output lockedBuffer
	logger, err := NewAsync(&output, 256)
	if err != nil {
		t.Fatal(err)
	}
	var wait sync.WaitGroup
	for documentID := int64(1); documentID <= 100; documentID++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			_ = logger.JobClaimed(documentID)
		}()
	}
	wait.Wait()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := logger.Close(ctx); err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(output.String()), "\n") {
		if !json.Valid([]byte(line)) {
			t.Fatalf("invalid JSON line %q", line)
		}
	}
}

func TestAsyncLoggerCloseIsBoundedByContext(t *testing.T) {
	writer := newBlockingWriter()
	logger, err := NewAsync(writer, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := logger.Startup(); err != nil {
		t.Fatal(err)
	}
	writer.waitStarted(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	started := time.Now()
	if err := logger.Close(ctx); err == nil {
		t.Fatal("Close() error = nil")
	}
	if elapsed := time.Since(started); elapsed > 200*time.Millisecond {
		t.Fatalf("Close() blocked for %s", elapsed)
	}
	writer.unblock()
}

func TestAsyncLoggerCloseLinearizesWithProducers(t *testing.T) {
	writer := &countingLineWriter{}
	logger, err := NewAsync(writer, 1024)
	if err != nil {
		t.Fatal(err)
	}
	var accepted atomic.Uint64
	var wait sync.WaitGroup
	start := make(chan struct{})
	for worker := range 20 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			for sequence := range 1000 {
				if logger.JobClaimed(int64(worker*1000+sequence+1)) == nil {
					accepted.Add(1)
				}
			}
		}()
	}
	close(start)
	time.Sleep(time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := logger.Close(ctx); err != nil {
		t.Fatal(err)
	}
	wait.Wait()
	if err := logger.JobClaimed(999999); err == nil {
		t.Fatal("post-close call succeeded")
	}
	if writer.Lines() != accepted.Load() {
		t.Fatalf("successful close wrote %d of %d accepted entries", writer.Lines(), accepted.Load())
	}
}

func TestAsyncLoggerFailureAccountsAcceptedEntries(t *testing.T) {
	writer := &failAfterWriter{allowed: 5}
	logger, err := NewAsync(writer, 128)
	if err != nil {
		t.Fatal(err)
	}
	for documentID := int64(1); documentID <= 100; documentID++ {
		_ = logger.JobClaimed(documentID)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := logger.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if err := logger.JobClaimed(200); err == nil {
		t.Fatal("post-failure call succeeded")
	}
	if got, want := writer.Lines()+logger.Dropped(), uint64(101); got != want {
		t.Fatalf("written+dropped = %d, want attempted calls %d", got, want)
	}
}

type errorWriter struct{ err error }

func (writer errorWriter) Write([]byte) (int, error) { return 0, writer.err }

type shortWriter struct{}

func (shortWriter) Write(data []byte) (int, error) { return len(data) - 1, nil }

type failureMode uint8

const (
	failurePartialError failureMode = iota
	failurePartialPanic
	failureZeroByteError
	failureZeroByteNoError
)

type oneShotFailureWriter struct {
	mode       failureMode
	err        error
	panicValue any
	calls      atomic.Int64
	output     bytes.Buffer
}

func (writer *oneShotFailureWriter) Write(data []byte) (int, error) {
	if writer.calls.Add(1) > 1 {
		return writer.output.Write(data)
	}
	switch writer.mode {
	case failurePartialError:
		written, _ := writer.output.Write(data[:len(data)/2])
		return written, writer.err
	case failurePartialPanic:
		_, _ = writer.output.Write(data[:len(data)/2])
		panic(writer.panicValue)
	case failureZeroByteError:
		return 0, writer.err
	case failureZeroByteNoError:
		return 0, nil
	default:
		return len(data), nil
	}
}

type blockingWriter struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func newBlockingWriter() *blockingWriter {
	return &blockingWriter{started: make(chan struct{}), release: make(chan struct{})}
}

func (writer *blockingWriter) Write(data []byte) (int, error) {
	writer.once.Do(func() { close(writer.started) })
	<-writer.release
	return len(data), nil
}

func (writer *blockingWriter) waitStarted(t *testing.T) {
	t.Helper()
	select {
	case <-writer.started:
	case <-time.After(time.Second):
		t.Fatal("writer did not start")
	}
}

func (writer *blockingWriter) unblock() {
	select {
	case <-writer.release:
	default:
		close(writer.release)
	}
}

type panicWriter struct{}

func (panicWriter) Write([]byte) (int, error) { panic("CANARY writer panic") }

type panicValueWriter struct{ value string }

func (writer panicValueWriter) Write([]byte) (int, error) { panic(writer.value) }

type shortThenRecordWriter struct{ calls atomic.Int64 }

func (writer *shortThenRecordWriter) Write(data []byte) (int, error) {
	if writer.calls.Add(1) == 1 {
		return len(data) / 2, errors.New("CANARY short write")
	}
	return len(data), nil
}

type lockedBuffer struct {
	mu sync.Mutex
	bytes.Buffer
}

func (buffer *lockedBuffer) Write(data []byte) (int, error) {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.Buffer.Write(data)
}

func (buffer *lockedBuffer) String() string {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.Buffer.String()
}

type countingLineWriter struct{ lines atomic.Uint64 }

func (writer *countingLineWriter) Write(data []byte) (int, error) {
	writer.lines.Add(uint64(bytes.Count(data, []byte{'\n'})))
	return len(data), nil
}

func (writer *countingLineWriter) Lines() uint64 { return writer.lines.Load() }

type failAfterWriter struct {
	allowed uint64
	lines   atomic.Uint64
}

func (writer *failAfterWriter) Write(data []byte) (int, error) {
	if writer.lines.Load() >= writer.allowed {
		return 0, errors.New("CANARY failed sink")
	}
	writer.lines.Add(uint64(bytes.Count(data, []byte{'\n'})))
	return len(data), nil
}

func (writer *failAfterWriter) Lines() uint64 { return writer.lines.Load() }
