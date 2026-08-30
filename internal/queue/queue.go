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

const batchColumns = `id, job_id, page_start, page_end, render_dpi, render_format,
	state, result_text, created_at, updated_at, completed_at`

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
			if _, err := conn.ExecContext(ctx, `UPDATE candidates
				SET priority = ?, generation = generation + 1, updated_at = ?
				WHERE document_id = ?`, max(priority, current), formatTime(q.now()), documentID); err != nil {
				return internalError("cannot refresh queued candidate", err)
			}
			return nil
		case !errors.Is(err, sql.ErrNoRows):
			return internalError("cannot inspect queued candidate", err)
		}

		now := formatTime(q.now())
		if _, err := conn.ExecContext(ctx, `INSERT INTO candidates (
			document_id, priority, generation, created_at, updated_at
		) VALUES (?, ?, 1, ?, ?)`, documentID, priority, now, now); err != nil {
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
	err := q.db.QueryRowContext(ctx, `SELECT document_id, priority, generation, created_at, updated_at
		FROM candidates ORDER BY priority DESC, created_at, document_id LIMIT 1`).Scan(
		&candidate.DocumentID, &candidate.Priority, &candidate.Generation, &createdAt, &updatedAt)
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

// DiscardCandidate removes an unresolvable candidate if it was not refreshed.
func (q *Queue) DiscardCandidate(ctx context.Context, documentID, generation int64) (bool, error) {
	if documentID <= 0 || generation <= 0 {
		return false, validationError("invalid candidate discard input")
	}
	discarded := false
	err := q.writeContext(ctx, func(conn *sql.Conn) error {
		result, err := conn.ExecContext(ctx,
			"DELETE FROM candidates WHERE document_id = ? AND generation = ?", documentID, generation,
		)
		if err != nil {
			return internalError("cannot discard queued candidate", err)
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return internalError("cannot confirm discarded candidate", err)
		}
		discarded = changed == 1
		return nil
	})
	return discarded, err
}

// ResolveCandidate atomically enqueues resolved work if the candidate was not refreshed.
func (q *Queue) ResolveCandidate(ctx context.Context, documentID, generation int64, input EnqueueInput) (Job, bool, bool, error) {
	if documentID <= 0 || generation <= 0 || input.DocumentID != documentID || !validEnqueueInput(input) {
		return Job{}, false, false, validationError("invalid candidate resolution input")
	}

	var job Job
	created := false
	resolved := false
	err := q.writeContext(ctx, func(conn *sql.Conn) error {
		var priority Priority
		if err := conn.QueryRowContext(ctx,
			"SELECT priority FROM candidates WHERE document_id = ? AND generation = ?", documentID, generation,
		).Scan(&priority); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			return internalError("cannot inspect queued candidate", err)
		}
		input.Priority = priority
		var err error
		job, created, err = q.enqueue(ctx, conn, input)
		if err != nil {
			return err
		}
		result, err := conn.ExecContext(ctx,
			"DELETE FROM candidates WHERE document_id = ? AND generation = ?", documentID, generation)
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
		resolved = true
		return nil
	})
	return job, created, resolved, err
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

// RenewLease extends the active parent job lease.
func (q *Queue) RenewLease(id int64, attempt int, owner string, leaseDuration time.Duration) error {
	return q.RenewLeaseContext(context.Background(), id, attempt, owner, leaseDuration)
}

// RenewLeaseContext extends the active parent job lease using the caller context.
func (q *Queue) RenewLeaseContext(ctx context.Context, id int64, attempt int, owner string, leaseDuration time.Duration) error {
	if id <= 0 || attempt <= 0 || blank(owner) || leaseDuration <= 0 {
		return validationError("invalid lease renewal input")
	}
	return q.writeContext(ctx, func(conn *sql.Conn) error {
		now := q.now().UTC()
		return updateOneContext(ctx, conn, `UPDATE jobs SET lease_expires_at = ?, updated_at = ?
			WHERE id = ? AND state = 'processing' AND attempts = ? AND lease_owner = ?
			AND lease_expires_at > ?`, formatTime(now.Add(leaseDuration)), formatTime(now),
			id, attempt, owner, formatTime(now))
	})
}

// EnsureBatchesContext creates or verifies the exact batch plan under the active parent lease.
func (q *Queue) EnsureBatchesContext(ctx context.Context, id int64, attempt int, owner string, ranges []BatchRange, dpi int, format string) ([]Batch, error) {
	if id <= 0 || attempt <= 0 || blank(owner) || !validBatchRanges(ranges) || dpi <= 0 || blank(format) {
		return nil, validationError("invalid batch plan")
	}
	var batches []Batch
	err := q.writeContext(ctx, func(conn *sql.Conn) error {
		if err := requireActiveLease(ctx, conn, q.now().UTC(), id, attempt, owner); err != nil {
			return err
		}
		var err error
		batches, err = listBatches(ctx, conn, id)
		if err != nil {
			return err
		}
		if len(batches) != 0 {
			if !sameBatchPlan(batches, ranges, dpi, format) {
				return validationError("batch plan does not match existing checkpoints")
			}
			return nil
		}
		now := formatTime(q.now())
		for _, pageRange := range ranges {
			if _, err := conn.ExecContext(ctx, `INSERT INTO batches (
				job_id, page_start, page_end, render_dpi, render_format, state,
				attempts, available_at, created_at, updated_at
			) VALUES (?, ?, ?, ?, ?, 'pending', 0, ?, ?, ?)`, id, pageRange.FirstPage,
				pageRange.LastPage, dpi, format, now, now, now); err != nil {
				return internalError("cannot create batch checkpoints", err)
			}
		}
		batches, err = listBatches(ctx, conn, id)
		return err
	})
	return batches, err
}

// ListBatchesContext loads checkpoints while verifying the active parent lease.
func (q *Queue) ListBatchesContext(ctx context.Context, id int64, attempt int, owner string) ([]Batch, error) {
	if id <= 0 || attempt <= 0 || blank(owner) {
		return nil, validationError("invalid batch list input")
	}
	var batches []Batch
	err := q.writeContext(ctx, func(conn *sql.Conn) error {
		if err := requireActiveLease(ctx, conn, q.now().UTC(), id, attempt, owner); err != nil {
			return err
		}
		var err error
		batches, err = listBatches(ctx, conn, id)
		return err
	})
	return batches, err
}

// CheckpointBatchContext stores canonical validated JSON under the active parent lease.
func (q *Queue) CheckpointBatchContext(ctx context.Context, id int64, attempt int, owner string, pageRange BatchRange, dpi int, format, resultText string) error {
	if id <= 0 || attempt <= 0 || blank(owner) || !validBatchRange(pageRange) || dpi <= 0 || blank(format) || blank(resultText) {
		return validationError("invalid batch checkpoint input")
	}
	return q.writeContext(ctx, func(conn *sql.Conn) error {
		now := q.now().UTC()
		if err := requireActiveLease(ctx, conn, now, id, attempt, owner); err != nil {
			return err
		}
		var state State
		var existing sql.NullString
		if err := conn.QueryRowContext(ctx, `SELECT state, result_text FROM batches WHERE job_id = ?
			AND page_start = ? AND page_end = ? AND render_dpi = ? AND render_format = ?`,
			id, pageRange.FirstPage, pageRange.LastPage, dpi, format).Scan(&state, &existing); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return validationError("batch checkpoint does not match planned range")
			}
			return internalError("cannot inspect batch checkpoint", err)
		}
		if state == StateCompleted {
			if existing.String == resultText {
				return nil
			}
			return validationError("completed batch checkpoint cannot be replaced")
		}
		if state != StatePending {
			return validationError("batch checkpoint is not pending")
		}
		return updateOneContext(ctx, conn, `UPDATE batches SET state = 'completed', result_text = ?,
			completed_at = ?, updated_at = ? WHERE job_id = ? AND page_start = ? AND page_end = ?
			AND render_dpi = ? AND render_format = ? AND state = 'pending'`,
			resultText, formatTime(now), formatTime(now), id, pageRange.FirstPage,
			pageRange.LastPage, dpi, format)
	})
}

// ScheduleRetry releases the active claim generation until a future time.
func (q *Queue) ScheduleRetry(id int64, attempt int, owner string, availableAt time.Time, diagnostic SafeDiagnostic) error {
	return q.ScheduleRetryContext(context.Background(), id, attempt, owner, availableAt, diagnostic)
}

// ScheduleRetryContext releases the active claim generation using the caller context.
func (q *Queue) ScheduleRetryContext(ctx context.Context, id int64, attempt int, owner string, availableAt time.Time, diagnostic SafeDiagnostic) error {
	if id <= 0 || attempt <= 0 || blank(owner) || !diagnostic.valid() {
		return validationError("invalid retry input")
	}
	return q.transitionProcessingContext(ctx, func(conn *sql.Conn, now time.Time) error {
		if !availableAt.After(now) {
			return validationError("invalid retry input")
		}
		timestamp := formatTime(now)
		return updateOneContext(ctx, conn, `UPDATE jobs SET state = 'retry', available_at = ?,
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
	return q.FailContext(context.Background(), id, attempt, owner, diagnostic)
}

// FailContext marks the active claim generation terminally failed using the caller context.
func (q *Queue) FailContext(ctx context.Context, id int64, attempt int, owner string, diagnostic SafeDiagnostic) error {
	if id <= 0 || attempt <= 0 || blank(owner) || !diagnostic.valid() {
		return validationError("invalid failure input")
	}
	return q.transitionProcessingContext(ctx, func(conn *sql.Conn, now time.Time) error {
		timestamp := formatTime(now)
		return updateOneContext(ctx, conn, `UPDATE jobs SET state = 'failed',
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
	return q.transitionProcessingContext(context.Background(), fn)
}

func (q *Queue) transitionProcessingContext(ctx context.Context, fn func(*sql.Conn, time.Time) error) error {
	return q.writeContext(ctx, func(conn *sql.Conn) error {
		return fn(conn, q.now().UTC())
	})
}

func updateOne(conn *sql.Conn, statement string, args ...any) error {
	return updateOneContext(context.Background(), conn, statement, args...)
}

func updateOneContext(ctx context.Context, conn *sql.Conn, statement string, args ...any) error {
	result, err := conn.ExecContext(ctx, statement, args...)
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

func requireActiveLease(ctx context.Context, conn *sql.Conn, now time.Time, id int64, attempt int, owner string) error {
	var active int
	if err := conn.QueryRowContext(ctx, `SELECT count(*) FROM jobs WHERE id = ?
		AND state = 'processing' AND attempts = ? AND lease_owner = ? AND lease_expires_at > ?`,
		id, attempt, owner, formatTime(now)).Scan(&active); err != nil {
		return internalError("cannot validate active job lease", err)
	}
	if active != 1 {
		return validationError("illegal job transition or stale lease owner")
	}
	return nil
}

func listBatches(ctx context.Context, conn *sql.Conn, jobID int64) ([]Batch, error) {
	rows, err := conn.QueryContext(ctx, `SELECT `+batchColumns+` FROM batches
		WHERE job_id = ? ORDER BY page_start, page_end`, jobID)
	if err != nil {
		return nil, internalError("cannot load batch checkpoints", err)
	}
	defer rows.Close()
	var batches []Batch
	for rows.Next() {
		var batch Batch
		var result, createdAt, updatedAt, completedAt sql.NullString
		if err := rows.Scan(&batch.ID, &batch.JobID, &batch.FirstPage, &batch.LastPage,
			&batch.RenderDPI, &batch.RenderFormat, &batch.State, &result, &createdAt,
			&updatedAt, &completedAt); err != nil {
			return nil, internalError("cannot load batch checkpoints", err)
		}
		batch.ResultText = result.String
		var parseErr error
		if batch.CreatedAt, parseErr = parseTime(createdAt.String); parseErr != nil {
			return nil, internalError("cannot load batch checkpoints", parseErr)
		}
		if batch.UpdatedAt, parseErr = parseTime(updatedAt.String); parseErr != nil {
			return nil, internalError("cannot load batch checkpoints", parseErr)
		}
		if completedAt.Valid {
			if batch.CompletedAt, parseErr = parseTime(completedAt.String); parseErr != nil {
				return nil, internalError("cannot load batch checkpoints", parseErr)
			}
		}
		batches = append(batches, batch)
	}
	if err := rows.Err(); err != nil {
		return nil, internalError("cannot load batch checkpoints", err)
	}
	return batches, nil
}

func validBatchRange(pageRange BatchRange) bool {
	return pageRange.FirstPage > 0 && pageRange.LastPage >= pageRange.FirstPage && pageRange.LastPage-pageRange.FirstPage < 5
}

func validBatchRanges(ranges []BatchRange) bool {
	if len(ranges) == 0 || ranges[0].FirstPage != 1 {
		return false
	}
	for index, pageRange := range ranges {
		if !validBatchRange(pageRange) || index > 0 && pageRange.FirstPage != ranges[index-1].LastPage+1 {
			return false
		}
	}
	return true
}

func sameBatchPlan(batches []Batch, ranges []BatchRange, dpi int, format string) bool {
	if len(batches) != len(ranges) {
		return false
	}
	for index, batch := range batches {
		if batch.FirstPage != ranges[index].FirstPage || batch.LastPage != ranges[index].LastPage || batch.RenderDPI != dpi || batch.RenderFormat != format {
			return false
		}
	}
	return true
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
