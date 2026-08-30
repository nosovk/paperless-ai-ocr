// Package securelog writes structured diagnostics with a fixed allowlist.
package securelog

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nosovk/paperless-ai-ocr/internal/queue"
	"github.com/nosovk/paperless-ai-ocr/internal/saferr"
)

var (
	errInvalidEntry = errors.New("invalid secure log entry")
	errWriteEntry   = errors.New("secure log write failed")
	errDropEntry    = errors.New("secure log entry dropped")
)

const (
	maximumPage       = 10_000
	maximumBatchPages = 5
)

// Logger writes one complete JSON object per line.
type Logger struct {
	mu     sync.Mutex
	writer io.Writer
	async  *asyncLogger
}

type asyncLogger struct {
	entries chan entry
	close   chan struct{}
	done    chan struct{}
	closed  atomic.Bool
	failed  atomic.Bool
	dropped atomic.Uint64
}

type entry struct {
	Level      string          `json:"level"`
	Event      string          `json:"event"`
	DocumentID int64           `json:"document_id,omitempty"`
	FirstPage  int             `json:"first_page,omitempty"`
	LastPage   int             `json:"last_page,omitempty"`
	DurationMS int64           `json:"duration_ms,omitempty"`
	State      queue.State     `json:"state,omitempty"`
	Category   saferr.Category `json:"category,omitempty"`
}

// New creates a logger writing to writer.
func New(writer io.Writer) *Logger {
	return &Logger{writer: writer}
}

// NewAsync creates a nonblocking logger with a bounded in-memory queue.
func NewAsync(writer io.Writer, capacity int) (*Logger, error) {
	if writer == nil || capacity <= 0 {
		return nil, errInvalidEntry
	}
	async := &asyncLogger{
		entries: make(chan entry, capacity),
		close:   make(chan struct{}, 1),
		done:    make(chan struct{}),
	}
	logger := &Logger{writer: writer, async: async}
	go logger.runAsync()
	return logger, nil
}

// Startup records service startup.
func (logger *Logger) Startup() error {
	return logger.write(entry{Level: "info", Event: "startup"})
}

// Ready records that service dependencies are ready.
func (logger *Logger) Ready() error {
	return logger.write(entry{Level: "info", Event: "ready"})
}

// Shutdown records completed service shutdown.
func (logger *Logger) Shutdown() error {
	return logger.write(entry{Level: "info", Event: "shutdown"})
}

// JobClaimed records a claimed document without queue-private fields.
func (logger *Logger) JobClaimed(documentID int64) error {
	if documentID <= 0 {
		return errInvalidEntry
	}
	return logger.write(entry{Level: "info", Event: "job_claimed", DocumentID: documentID, State: queue.StateProcessing})
}

// BatchCompleted records one inclusive completed page range.
func (logger *Logger) BatchCompleted(documentID int64, firstPage, lastPage int, duration time.Duration) error {
	if documentID <= 0 || firstPage <= 0 || lastPage < firstPage || lastPage > maximumPage ||
		lastPage-firstPage >= maximumBatchPages || duration < 0 {
		return errInvalidEntry
	}
	return logger.write(entry{Level: "info", Event: "batch_completed", DocumentID: documentID,
		FirstPage: firstPage, LastPage: lastPage, DurationMS: duration.Milliseconds(), State: queue.StateCompleted})
}

// JobFinished records a safe durable job outcome.
func (logger *Logger) JobFinished(documentID int64, state queue.State, duration time.Duration) error {
	if documentID <= 0 || duration < 0 || !validTerminalState(state) {
		return errInvalidEntry
	}
	return logger.write(entry{Level: "info", Event: "job_finished", DocumentID: documentID,
		DurationMS: duration.Milliseconds(), State: state})
}

// BackgroundFailure records only a categorized failure.
func (logger *Logger) BackgroundFailure(category saferr.Category) error {
	if !validCategory(category) {
		return errInvalidEntry
	}
	return logger.write(entry{Level: "error", Event: "background_failure", Category: category})
}

func (logger *Logger) write(value entry) error {
	if logger == nil || logger.writer == nil {
		return errWriteEntry
	}
	if logger.async != nil {
		if logger.async.closed.Load() || logger.async.failed.Load() {
			logger.async.dropped.Add(1)
			return errDropEntry
		}
		select {
		case logger.async.entries <- value:
			return nil
		default:
			logger.async.dropped.Add(1)
			return errDropEntry
		}
	}
	return logger.writeEntry(value)
}

func (logger *Logger) writeEntry(value entry) error {
	data, err := json.Marshal(value)
	if err != nil {
		return errWriteEntry
	}
	data = append(data, '\n')
	logger.mu.Lock()
	defer logger.mu.Unlock()
	for len(data) > 0 {
		written, err := logger.writer.Write(data)
		if err != nil || written <= 0 || written > len(data) {
			return errWriteEntry
		}
		data = data[written:]
	}
	return nil
}

func (logger *Logger) runAsync() {
	defer close(logger.async.done)
	defer func() {
		if recover() != nil {
			logger.async.failed.Store(true)
		}
	}()
	for {
		select {
		case value := <-logger.async.entries:
			if err := logger.writeEntry(value); err != nil {
				logger.async.failed.Store(true)
				return
			}
		case <-logger.async.close:
			for {
				select {
				case value := <-logger.async.entries:
					if err := logger.writeEntry(value); err != nil {
						logger.async.failed.Store(true)
						return
					}
				default:
					return
				}
			}
		}
	}
}

// Close stops accepting entries and waits for queued entries within ctx.
func (logger *Logger) Close(ctx context.Context) error {
	if logger == nil || logger.async == nil {
		return nil
	}
	if logger.async.closed.CompareAndSwap(false, true) {
		logger.async.close <- struct{}{}
	}
	select {
	case <-logger.async.done:
		return nil
	case <-ctx.Done():
		return errWriteEntry
	}
}

// Dropped returns the number of entries rejected after saturation or failure.
func (logger *Logger) Dropped() uint64 {
	if logger == nil || logger.async == nil {
		return 0
	}
	return logger.async.dropped.Load()
}

func validTerminalState(state queue.State) bool {
	switch state {
	case queue.StateRetry, queue.StateCompleted, queue.StateFailed:
		return true
	default:
		return false
	}
}

func validCategory(category saferr.Category) bool {
	switch category {
	case saferr.CategoryConfiguration, saferr.CategoryPaperless, saferr.CategoryProvider,
		saferr.CategoryValidation, saferr.CategoryRendering, saferr.CategoryInternal:
		return true
	default:
		return false
	}
}
