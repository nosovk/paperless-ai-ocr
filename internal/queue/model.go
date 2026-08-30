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

// FinalizationStage is the last externally confirmed finalization side effect.
type FinalizationStage string

const (
	FinalizationPending            FinalizationStage = "pending"
	FinalizationContentUpdated     FinalizationStage = "content_updated"
	FinalizationCompleteTagAdded   FinalizationStage = "complete_tag_added"
	FinalizationFailedTagRemoved   FinalizationStage = "failed_tag_removed"
	FinalizationMetadataDispatched FinalizationStage = "metadata_dispatched"
	FinalizationFailurePending     FinalizationStage = "failure_pending"
	FinalizationFailureTagAdded    FinalizationStage = "failure_tag_added"
)

// DispatchState records downstream dispatch admission and confirmation.
type DispatchState string

const (
	DispatchNone      DispatchState = "none"
	DispatchReserved  DispatchState = "reserved"
	DispatchConfirmed DispatchState = "confirmed"
)

// FinalizationState is durable orchestration state under one admission token.
type FinalizationState struct {
	Stage           FinalizationStage
	Dispatch        DispatchState
	FailureCategory saferr.Category
	FailureMessage  string
}

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

// BatchRange is an exact inclusive planned page range.
type BatchRange struct {
	FirstPage int
	LastPage  int
}

// Batch is a durable checkpoint owned by its parent job lease.
type Batch struct {
	ID           int64
	JobID        int64
	FirstPage    int
	LastPage     int
	RenderDPI    int
	RenderFormat string
	State        State
	ResultText   string
	CreatedAt    time.Time
	UpdatedAt    time.Time
	CompletedAt  time.Time
}
