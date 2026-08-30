// Package securelog writes structured diagnostics with a fixed allowlist.
package securelog

import (
	"encoding/json"
	"errors"
	"io"
	"sync"
	"time"

	"github.com/nosovk/paperless-ai-ocr/internal/queue"
	"github.com/nosovk/paperless-ai-ocr/internal/saferr"
)

var (
	errInvalidEntry = errors.New("invalid secure log entry")
	errWriteEntry   = errors.New("secure log write failed")
)

const (
	maximumPage       = 10_000
	maximumBatchPages = 5
	maximumDuration   = 24 * time.Hour
)

// Logger writes one complete JSON object per line.
type Logger struct {
	mu     sync.Mutex
	writer io.Writer
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
		lastPage-firstPage >= maximumBatchPages || duration < 0 || duration > maximumDuration {
		return errInvalidEntry
	}
	return logger.write(entry{Level: "info", Event: "batch_completed", DocumentID: documentID,
		FirstPage: firstPage, LastPage: lastPage, DurationMS: duration.Milliseconds(), State: queue.StateCompleted})
}

// JobFinished records a safe durable job outcome.
func (logger *Logger) JobFinished(documentID int64, state queue.State, duration time.Duration) error {
	if documentID <= 0 || duration < 0 || duration > maximumDuration || !validState(state) {
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
	data, err := json.Marshal(value)
	if err != nil {
		return errWriteEntry
	}
	data = append(data, '\n')
	logger.mu.Lock()
	defer logger.mu.Unlock()
	written, err := logger.writer.Write(data)
	if err != nil || written != len(data) {
		return errWriteEntry
	}
	return nil
}

func validState(state queue.State) bool {
	switch state {
	case queue.StatePending, queue.StateProcessing, queue.StateRetry, queue.StateCompleted, queue.StateFailed:
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
