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

const timestampLayout = "2006-01-02T15:04:05.000000000Z07:00"

// Queue stores durable jobs in SQLite.
type Queue struct {
	db  *sql.DB
	now func() time.Time
}

// New constructs a queue around an opened, migrated database.
func New(db *sql.DB) *Queue {
	return &Queue{db: db, now: time.Now}
}

// Enqueue creates current work or returns an existing current or terminal job.
// The returned bool reports whether a new row was created.
func (q *Queue) Enqueue(input EnqueueInput) (Job, bool, error) {
	if input.DocumentID <= 0 || blank(input.SourceChecksum) || blank(input.Model) || blank(input.PromptVersion) {
		return Job{}, false, validationError("invalid enqueue input")
	}

	var job Job
	created := false
	err := q.write(func(conn *sql.Conn) error {
		var err error
		job, err = queryJob(conn.QueryRowContext(context.Background(), `SELECT `+jobColumns+`
			FROM jobs WHERE document_id = ? AND source_checksum = ?
			ORDER BY CASE
				WHEN state IN ('pending', 'processing', 'retry') THEN 0
				WHEN state = 'completed' THEN 1 ELSE 2 END, id DESC LIMIT 1`,
			input.DocumentID, input.SourceChecksum))
		if err == nil {
			if (job.State == StatePending || job.State == StateRetry) && input.Priority > job.Priority {
				if _, err := conn.ExecContext(context.Background(), `UPDATE jobs
					SET priority = ?, updated_at = ? WHERE id = ?`, input.Priority, formatTime(q.now()), job.ID); err != nil {
					return internalError("cannot promote queued job", err)
				}
				job.Priority = input.Priority
			}
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return internalError("cannot inspect queued jobs", err)
		}

		now := formatTime(q.now())
		result, err := conn.ExecContext(context.Background(), `INSERT INTO jobs (
			document_id, source_checksum, priority, state, attempts, available_at,
			model, prompt_version, created_at, updated_at
		) VALUES (?, ?, ?, 'pending', 0, ?, ?, ?, ?, ?)`, input.DocumentID,
			input.SourceChecksum, input.Priority, now, input.Model, input.PromptVersion, now, now)
		if err != nil {
			return internalError("cannot enqueue job", err)
		}
		id, err := result.LastInsertId()
		if err != nil {
			return internalError("cannot identify enqueued job", err)
		}
		job, err = getJob(conn, id)
		if err != nil {
			return internalError("cannot load enqueued job", err)
		}
		created = true
		return nil
	})
	return job, created, err
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

// ScheduleRetry releases a processing job until a future time.
func (q *Queue) ScheduleRetry(id int64, owner string, availableAt time.Time, category, message string) error {
	if id <= 0 || blank(owner) || blank(category) || blank(message) || !availableAt.After(q.now()) {
		return validationError("invalid retry input")
	}
	return q.updateOne(`UPDATE jobs SET state = 'retry', available_at = ?,
		lease_owner = NULL, lease_expires_at = NULL, error_category = ?, error_message = ?,
		updated_at = ? WHERE id = ? AND state = 'processing' AND lease_owner = ?`,
		formatTime(availableAt), category, message, formatTime(q.now()), id, owner)
}

// Complete marks an owned processing job completed.
func (q *Queue) Complete(id int64, owner string) error {
	if id <= 0 || blank(owner) {
		return validationError("invalid completion input")
	}
	now := formatTime(q.now())
	return q.updateOne(`UPDATE jobs SET state = 'completed',
		lease_owner = NULL, lease_expires_at = NULL, error_category = NULL,
		error_message = NULL, completed_at = ?, updated_at = ?
		WHERE id = ? AND state = 'processing' AND lease_owner = ?`, now, now, id, owner)
}

// Fail marks an owned processing job terminally failed using safe diagnostics.
func (q *Queue) Fail(id int64, owner, category, message string) error {
	if id <= 0 || blank(owner) || blank(category) || blank(message) {
		return validationError("invalid failure input")
	}
	now := formatTime(q.now())
	return q.updateOne(`UPDATE jobs SET state = 'failed',
		lease_owner = NULL, lease_expires_at = NULL, error_category = ?, error_message = ?,
		completed_at = ?, updated_at = ?
		WHERE id = ? AND state = 'processing' AND lease_owner = ?`, category, message, now, now, id, owner)
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
	conn, err := q.db.Conn(context.Background())
	if err != nil {
		return internalError("cannot acquire database connection", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(context.Background(), "BEGIN IMMEDIATE"); err != nil {
		return internalError("cannot begin queue transaction", err)
	}
	defer func() {
		if err != nil {
			conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()
	if err = fn(conn); err != nil {
		return err
	}
	if _, err = conn.ExecContext(context.Background(), "COMMIT"); err != nil {
		return internalError("cannot commit queue transaction", err)
	}
	return nil
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
	job.ErrorCategory = errorCategory.String
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

func validationError(message string) error {
	return saferr.New(saferr.CategoryValidation, message)
}

func internalError(message string, cause error) error {
	return saferr.Wrap(saferr.CategoryInternal, message, cause)
}
