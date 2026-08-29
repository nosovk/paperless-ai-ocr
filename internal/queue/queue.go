// Package queue provides a durable, single-active-worker priority queue.
package queue

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/nosovk/paperless-ai-ocr/internal/saferr"
)

const (
	timestampLayout           = "2006-01-02T15:04:05.000000000Z07:00"
	maxDiagnosticMessageBytes = 256
	rollbackTimeout           = 5 * time.Second
	defaultBusyTimeout        = 5 * time.Second
	busyRetryInterval         = time.Millisecond
	sqliteBusy                = 5
)

// Queue stores durable jobs in SQLite.
type Queue struct {
	db  *sql.DB
	now func() time.Time
}

// New constructs a queue around an opened, migrated database.
func New(db *sql.DB) *Queue {
	return &Queue{db: db, now: time.Now}
}

// EnqueueCandidate durably records a document that still needs job inputs resolved.
// The returned bool reports whether a new row was created.
func (q *Queue) EnqueueCandidate(ctx context.Context, documentID int64, priority Priority) (bool, error) {
	if documentID <= 0 || !priority.valid() {
		return false, validationError("invalid candidate input")
	}

	created := false
	err := q.writeContext(ctx, func(conn *sql.Conn) error {
		var current Priority
		err := conn.QueryRowContext(ctx,
			"SELECT priority FROM candidates WHERE document_id = ?", documentID,
		).Scan(&current)
		switch {
		case err == nil:
			if priority <= current {
				return nil
			}
			if _, err := conn.ExecContext(ctx, `UPDATE candidates
				SET priority = ?, updated_at = ? WHERE document_id = ?`,
				priority, formatTime(q.now()), documentID); err != nil {
				return internalError("cannot promote queued candidate", err)
			}
			return nil
		case !errors.Is(err, sql.ErrNoRows):
			return internalError("cannot inspect queued candidate", err)
		}

		now := formatTime(q.now())
		if _, err := conn.ExecContext(ctx, `INSERT INTO candidates (
			document_id, priority, created_at, updated_at
		) VALUES (?, ?, ?, ?)`, documentID, priority, now, now); err != nil {
			return internalError("cannot enqueue candidate", err)
		}
		created = true
		return nil
	})
	return created, err
}

// NextCandidate returns the highest-priority unresolved candidate without deleting it.
func (q *Queue) NextCandidate(ctx context.Context) (Candidate, bool, error) {
	var candidate Candidate
	var createdAt, updatedAt string
	err := q.db.QueryRowContext(ctx, `SELECT document_id, priority, created_at, updated_at
		FROM candidates ORDER BY priority DESC, created_at, document_id LIMIT 1`).Scan(
		&candidate.DocumentID, &candidate.Priority, &createdAt, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Candidate{}, false, nil
	}
	if err != nil {
		return Candidate{}, false, internalError("cannot inspect queued candidates", err)
	}
	if candidate.CreatedAt, err = parseTime(createdAt); err != nil {
		return Candidate{}, false, internalError("cannot load queued candidate", err)
	}
	if candidate.UpdatedAt, err = parseTime(updatedAt); err != nil {
		return Candidate{}, false, internalError("cannot load queued candidate", err)
	}
	return candidate, true, nil
}

// DiscardCandidate permanently removes an unresolvable candidate.
// Discarding an already absent candidate is idempotent.
func (q *Queue) DiscardCandidate(ctx context.Context, documentID int64) error {
	if documentID <= 0 {
		return validationError("invalid candidate discard input")
	}
	return q.writeContext(ctx, func(conn *sql.Conn) error {
		if _, err := conn.ExecContext(ctx,
			"DELETE FROM candidates WHERE document_id = ?", documentID,
		); err != nil {
			return internalError("cannot discard queued candidate", err)
		}
		return nil
	})
}

// ResolveCandidate atomically enqueues resolved work and removes its candidate.
func (q *Queue) ResolveCandidate(ctx context.Context, documentID int64, input EnqueueInput) (Job, bool, error) {
	if documentID <= 0 || input.DocumentID != documentID || !validEnqueueInput(input) {
		return Job{}, false, validationError("invalid candidate resolution input")
	}

	var job Job
	created := false
	err := q.writeContext(ctx, func(conn *sql.Conn) error {
		var priority Priority
		if err := conn.QueryRowContext(ctx,
			"SELECT priority FROM candidates WHERE document_id = ?", documentID,
		).Scan(&priority); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return validationError("candidate no longer exists")
			}
			return internalError("cannot inspect queued candidate", err)
		}
		input.Priority = priority
		var err error
		job, created, err = q.enqueue(ctx, conn, input)
		if err != nil {
			return err
		}
		result, err := conn.ExecContext(ctx, "DELETE FROM candidates WHERE document_id = ?", documentID)
		if err != nil {
			return internalError("cannot delete resolved candidate", err)
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return internalError("cannot confirm resolved candidate", err)
		}
		if changed != 1 {
			return validationError("candidate changed during resolution")
		}
		return nil
	})
	return job, created, err
}

// Enqueue creates current work or returns an existing current or terminal job.
// The returned bool reports whether a new row was created.
func (q *Queue) Enqueue(input EnqueueInput) (Job, bool, error) {
	return q.EnqueueContext(context.Background(), input)
}

// EnqueueContext creates current work using the caller's cancellation context.
func (q *Queue) EnqueueContext(ctx context.Context, input EnqueueInput) (Job, bool, error) {
	if !validEnqueueInput(input) {
		return Job{}, false, validationError("invalid enqueue input")
	}

	var job Job
	created := false
	err := q.writeContext(ctx, func(conn *sql.Conn) error {
		var err error
		job, created, err = q.enqueue(ctx, conn, input)
		return err
	})
	return job, created, err
}

func (q *Queue) enqueue(ctx context.Context, conn *sql.Conn, input EnqueueInput) (Job, bool, error) {
	job, err := queryJob(conn.QueryRowContext(ctx, `SELECT `+jobColumns+`
			FROM jobs WHERE document_id = ? AND source_checksum = ?
			ORDER BY CASE
				WHEN state IN ('pending', 'processing', 'retry') THEN 0
				WHEN state = 'completed' THEN 1 ELSE 2 END, id DESC LIMIT 1`,
		input.DocumentID, input.SourceChecksum))
	if err == nil {
		if (job.State == StatePending || job.State == StateRetry) && input.Priority > job.Priority {
			now := q.now().UTC()
			if _, err := conn.ExecContext(ctx, `UPDATE jobs
					SET priority = ?, updated_at = ? WHERE id = ?`, input.Priority, formatTime(now), job.ID); err != nil {
				return Job{}, false, internalError("cannot promote queued job", err)
			}
			job.Priority = input.Priority
			job.UpdatedAt = now
		}
		return job, false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Job{}, false, internalError("cannot inspect queued jobs", err)
	}

	now := formatTime(q.now())
	result, err := conn.ExecContext(ctx, `INSERT INTO jobs (
			document_id, source_checksum, priority, state, attempts, available_at,
			model, prompt_version, created_at, updated_at
		) VALUES (?, ?, ?, 'pending', 0, ?, ?, ?, ?, ?)`, input.DocumentID,
		input.SourceChecksum, input.Priority, now, input.Model, input.PromptVersion, now, now)
	if err != nil {
		return Job{}, false, internalError("cannot enqueue job", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return Job{}, false, internalError("cannot identify enqueued job", err)
	}
	job, err = queryJob(conn.QueryRowContext(ctx, "SELECT "+jobColumns+" FROM jobs WHERE id = ?", id))
	if err != nil {
		return Job{}, false, internalError("cannot load enqueued job", err)
	}
	return job, true, nil
}

// Claim atomically leases the highest-priority due job when no job is active.
func (q *Queue) Claim(owner string, leaseDuration time.Duration) (Job, bool, error) {
	if blank(owner) || leaseDuration <= 0 {
		return Job{}, false, validationError("invalid claim input")
	}

	var job Job
	claimed := false
	err := q.write(func(conn *sql.Conn) error {
		var active int
		if err := conn.QueryRowContext(context.Background(),
			"SELECT count(*) FROM jobs WHERE state = 'processing'").Scan(&active); err != nil {
			return internalError("cannot inspect active jobs", err)
		}
		if active != 0 {
			return nil
		}

		now := q.now().UTC()
		candidate, err := queryJob(conn.QueryRowContext(context.Background(), `SELECT `+jobColumns+`
			FROM jobs WHERE state IN ('pending', 'retry') AND available_at <= ?
			ORDER BY priority DESC, created_at, id LIMIT 1`, formatTime(now)))
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return internalError("cannot select queued job", err)
		}

		result, err := conn.ExecContext(context.Background(), `UPDATE jobs SET
			state = 'processing', attempts = attempts + 1, lease_owner = ?,
			lease_expires_at = ?, updated_at = ?
			WHERE id = ? AND state IN ('pending', 'retry') AND available_at <= ?`,
			owner, formatTime(now.Add(leaseDuration)), formatTime(now), candidate.ID, formatTime(now))
		if err != nil {
			return internalError("cannot claim queued job", err)
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return internalError("cannot confirm queued job claim", err)
		}
		if changed != 1 {
			return validationError("queued job changed during claim")
		}
		job, err = getJob(conn, candidate.ID)
		if err != nil {
			return internalError("cannot load claimed job", err)
		}
		claimed = true
		return nil
	})
	return job, claimed, err
}

// ScheduleRetry releases the active claim generation until a future time.
func (q *Queue) ScheduleRetry(id int64, attempt int, owner string, availableAt time.Time, diagnostic SafeDiagnostic) error {
	if id <= 0 || attempt <= 0 || blank(owner) || !diagnostic.valid() {
		return validationError("invalid retry input")
	}
	return q.transitionProcessing(func(conn *sql.Conn, now time.Time) error {
		if !availableAt.After(now) {
			return validationError("invalid retry input")
		}
		timestamp := formatTime(now)
		return updateOne(conn, `UPDATE jobs SET state = 'retry', available_at = ?,
			lease_owner = NULL, lease_expires_at = NULL, error_category = ?, error_message = ?,
			updated_at = ? WHERE id = ? AND state = 'processing' AND attempts = ?
			AND lease_owner = ? AND lease_expires_at > ?`, formatTime(availableAt),
			diagnostic.Category, diagnostic.Message, timestamp, id, attempt, owner, timestamp)
	})
}

// Complete marks the active claim generation completed.
func (q *Queue) Complete(id int64, attempt int, owner string) error {
	if id <= 0 || attempt <= 0 || blank(owner) {
		return validationError("invalid completion input")
	}
	return q.transitionProcessing(func(conn *sql.Conn, now time.Time) error {
		timestamp := formatTime(now)
		return updateOne(conn, `UPDATE jobs SET state = 'completed',
			lease_owner = NULL, lease_expires_at = NULL, error_category = NULL,
			error_message = NULL, completed_at = ?, updated_at = ?
			WHERE id = ? AND state = 'processing' AND attempts = ?
			AND lease_owner = ? AND lease_expires_at > ?`, timestamp, timestamp, id, attempt, owner, timestamp)
	})
}

// Fail marks the active claim generation terminally failed.
func (q *Queue) Fail(id int64, attempt int, owner string, diagnostic SafeDiagnostic) error {
	if id <= 0 || attempt <= 0 || blank(owner) || !diagnostic.valid() {
		return validationError("invalid failure input")
	}
	return q.transitionProcessing(func(conn *sql.Conn, now time.Time) error {
		timestamp := formatTime(now)
		return updateOne(conn, `UPDATE jobs SET state = 'failed',
			lease_owner = NULL, lease_expires_at = NULL, error_category = ?, error_message = ?,
			completed_at = ?, updated_at = ?
			WHERE id = ? AND state = 'processing' AND attempts = ?
			AND lease_owner = ? AND lease_expires_at > ?`, diagnostic.Category,
			diagnostic.Message, timestamp, timestamp, id, attempt, owner, timestamp)
	})
}

// RetryFailed explicitly returns a terminally failed job to the queue.
func (q *Queue) RetryFailed(id int64, availableAt time.Time) error {
	if id <= 0 || !availableAt.After(q.now()) {
		return validationError("invalid failed retry input")
	}
	return q.write(func(conn *sql.Conn) error {
		var conflict int
		if err := conn.QueryRowContext(context.Background(), `SELECT count(*) FROM jobs AS current
			JOIN jobs AS failed ON failed.id = ?
			WHERE current.id != failed.id
			AND current.document_id = failed.document_id
			AND current.source_checksum = failed.source_checksum
			AND current.state IN ('pending', 'processing', 'retry')`, id).Scan(&conflict); err != nil {
			return internalError("cannot inspect failed job retry", err)
		}
		if conflict != 0 {
			return validationError("current work already exists for failed job")
		}
		return updateOne(conn, `UPDATE jobs SET state = 'retry', available_at = ?,
			lease_owner = NULL, lease_expires_at = NULL, error_category = NULL,
			error_message = NULL, completed_at = NULL, updated_at = ?
			WHERE id = ? AND state = 'failed'`, formatTime(availableAt), formatTime(q.now()), id)
	})
}

// RecoverExpiredLeases releases interrupted processing jobs for immediate retry.
func (q *Queue) RecoverExpiredLeases() (int64, error) {
	var recovered int64
	err := q.write(func(conn *sql.Conn) error {
		now := formatTime(q.now())
		result, err := conn.ExecContext(context.Background(), `UPDATE jobs SET
			state = 'retry', available_at = ?, lease_owner = NULL, lease_expires_at = NULL,
			error_category = 'internal', error_message = 'lease expired', updated_at = ?
			WHERE state = 'processing' AND lease_expires_at <= ?`, now, now, now)
		if err != nil {
			return internalError("cannot recover expired job leases", err)
		}
		recovered, err = result.RowsAffected()
		if err != nil {
			return internalError("cannot count recovered job leases", err)
		}
		return nil
	})
	return recovered, err
}

func (q *Queue) updateOne(statement string, args ...any) error {
	return q.write(func(conn *sql.Conn) error {
		return updateOne(conn, statement, args...)
	})
}

func (q *Queue) transitionProcessing(fn func(*sql.Conn, time.Time) error) error {
	return q.write(func(conn *sql.Conn) error {
		return fn(conn, q.now().UTC())
	})
}

func updateOne(conn *sql.Conn, statement string, args ...any) error {
	result, err := conn.ExecContext(context.Background(), statement, args...)
	if err != nil {
		return internalError("cannot transition queued job", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return internalError("cannot confirm queued job transition", err)
	}
	if changed != 1 {
		return validationError("illegal job transition or stale lease owner")
	}
	return nil
}

func (q *Queue) get(id int64) (Job, error) {
	job, err := getJob(q.db, id)
	if err != nil {
		return Job{}, internalError("cannot load queued job", err)
	}
	return job, nil
}

func (q *Queue) write(fn func(*sql.Conn) error) (err error) {
	return q.writeContext(context.Background(), fn)
}

func (q *Queue) writeContext(ctx context.Context, fn func(*sql.Conn) error) (err error) {
	conn, err := q.db.Conn(ctx)
	if err != nil {
		return internalError("cannot acquire database connection", err)
	}
	defer conn.Close()
	if err := beginImmediate(ctx, conn); err != nil {
		return internalError("cannot begin queue transaction", err)
	}
	defer func() {
		if err != nil {
			rollbackCtx, cancel := context.WithTimeout(context.Background(), rollbackTimeout)
			defer cancel()
			conn.ExecContext(rollbackCtx, "ROLLBACK")
		}
	}()
	if err = fn(conn); err != nil {
		return err
	}
	if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
		return internalError("cannot commit queue transaction", err)
	}
	return nil
}

func beginImmediate(ctx context.Context, conn *sql.Conn) error {
	waitCtx := ctx
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		waitCtx, cancel = context.WithTimeout(ctx, defaultBusyTimeout)
		defer cancel()
	}
	if _, err := conn.ExecContext(ctx, "PRAGMA busy_timeout = 0"); err != nil {
		return err
	}
	defer func() {
		restoreCtx, cancel := context.WithTimeout(context.Background(), rollbackTimeout)
		defer cancel()
		conn.ExecContext(restoreCtx, "PRAGMA busy_timeout = 5000")
	}()
	for {
		if _, err := conn.ExecContext(waitCtx, "BEGIN IMMEDIATE"); err == nil {
			return nil
		} else if !isSQLiteBusy(err) {
			return err
		}
		timer := time.NewTimer(busyRetryInterval)
		select {
		case <-waitCtx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return waitCtx.Err()
		case <-timer.C:
		}
	}
}

func isSQLiteBusy(err error) bool {
	var sqliteError interface{ Code() int }
	return errors.As(err, &sqliteError) && sqliteError.Code()&0xff == sqliteBusy
}

const jobColumns = `id, document_id, source_checksum, priority, state, attempts,
	available_at, lease_owner, lease_expires_at, model, prompt_version,
	error_category, error_message, created_at, updated_at, completed_at`

type rowScanner interface {
	Scan(...any) error
}

func getJob(queryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, id int64) (Job, error) {
	return queryJob(queryer.QueryRowContext(context.Background(),
		"SELECT "+jobColumns+" FROM jobs WHERE id = ?", id))
}

func queryJob(row rowScanner) (Job, error) {
	var job Job
	var availableAt, createdAt, updatedAt string
	var leaseOwner, leaseExpiresAt, errorCategory, errorMessage, completedAt sql.NullString
	if err := row.Scan(&job.ID, &job.DocumentID, &job.SourceChecksum, &job.Priority,
		&job.State, &job.Attempts, &availableAt, &leaseOwner, &leaseExpiresAt,
		&job.Model, &job.PromptVersion, &errorCategory, &errorMessage, &createdAt,
		&updatedAt, &completedAt); err != nil {
		return Job{}, err
	}
	var err error
	if job.AvailableAt, err = parseTime(availableAt); err != nil {
		return Job{}, err
	}
	if job.CreatedAt, err = parseTime(createdAt); err != nil {
		return Job{}, err
	}
	if job.UpdatedAt, err = parseTime(updatedAt); err != nil {
		return Job{}, err
	}
	job.LeaseOwner = leaseOwner.String
	job.ErrorCategory = saferr.Category(errorCategory.String)
	job.ErrorMessage = errorMessage.String
	if leaseExpiresAt.Valid {
		if job.LeaseExpiresAt, err = parseTime(leaseExpiresAt.String); err != nil {
			return Job{}, err
		}
	}
	if completedAt.Valid {
		if job.CompletedAt, err = parseTime(completedAt.String); err != nil {
			return Job{}, err
		}
	}
	return job, nil
}

func formatTime(value time.Time) string {
	return value.UTC().Format(timestampLayout)
}

func parseTime(value string) (time.Time, error) {
	return time.Parse(timestampLayout, value)
}

func blank(value string) bool {
	return strings.TrimSpace(value) == ""
}

func validEnqueueInput(input EnqueueInput) bool {
	return input.DocumentID > 0 && input.Priority.valid() && !blank(input.SourceChecksum) && !blank(input.Model) && !blank(input.PromptVersion)
}

func (priority Priority) valid() bool {
	return priority == PriorityBackfill || priority == PriorityWebhook
}

func (diagnostic SafeDiagnostic) valid() bool {
	if !validDiagnosticCategory(diagnostic.Category) || blank(diagnostic.Message) || len(diagnostic.Message) > maxDiagnosticMessageBytes {
		return false
	}
	for _, character := range []byte(diagnostic.Message) {
		if character < 0x20 || character == 0x7f {
			return false
		}
	}
	return true
}

func validDiagnosticCategory(category saferr.Category) bool {
	switch category {
	case saferr.CategoryConfiguration,
		saferr.CategoryPaperless,
		saferr.CategoryProvider,
		saferr.CategoryValidation,
		saferr.CategoryRendering,
		saferr.CategoryInternal:
		return true
	default:
		return false
	}
}

func validationError(message string) error {
	return saferr.New(saferr.CategoryValidation, message)
}

func internalError(message string, cause error) error {
	return saferr.Wrap(saferr.CategoryInternal, message, cause)
}
