package queue

import (
	"time"

	"github.com/nosovk/paperless-ai-ocr/internal/saferr"
)

// State is a durable job lifecycle state.
type State string

const (
	StatePending    State = "pending"
	StateProcessing State = "processing"
	StateRetry      State = "retry"
	StateCompleted  State = "completed"
	StateFailed     State = "failed"
)

// Priority controls claim order. Higher values are claimed first.
type Priority int

const (
	PriorityBackfill Priority = 0
	PriorityWebhook  Priority = 100
)

// EnqueueInput describes a document source to process.
type EnqueueInput struct {
	DocumentID     int64
	SourceChecksum string
	Priority       Priority
	Model          string
	PromptVersion  string
}

// Candidate is unresolved durable work ordered ahead of archive backfill.
type Candidate struct {
	DocumentID int64
	Priority   Priority
	Generation int64
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// SafeDiagnostic is a caller-vetted, operator-facing one-line diagnostic.
// Message is limited to 256 bytes and cannot contain ASCII control characters.
// Queue validation does not detect secrets or document content; callers remain
// responsible for ensuring Message is safe to persist and display.
type SafeDiagnostic struct {
	Category saferr.Category
	Message  string
}

// Job is the durable queue record returned to workers.
type Job struct {
	ID             int64
	DocumentID     int64
	SourceChecksum string
	Priority       Priority
	State          State
	Attempts       int
	AvailableAt    time.Time
	LeaseOwner     string
	LeaseExpiresAt time.Time
	Model          string
	PromptVersion  string
	ErrorCategory  saferr.Category
	ErrorMessage   string
	CreatedAt      time.Time
	UpdatedAt      time.Time
	CompletedAt    time.Time
}
