package database

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const supportedSchemaVersion = 1

func TestOpenCreatesAndConfiguresDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queue.db")

	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { db.Close() })

	for _, table := range []string{"jobs", "batches", "settings"} {
		var count int
		if err := db.QueryRow(
			"SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = ?",
			table,
		).Scan(&count); err != nil {
			t.Fatalf("query table %q: %v", table, err)
		}
		if count != 1 {
			t.Errorf("table %q count = %d, want 1", table, count)
		}
	}

	assertPragmaValue(t, db, "journal_mode", "wal")
	assertPragmaValue(t, db, "foreign_keys", "1")
	assertPragmaValue(t, db, "busy_timeout", "5000")
	assertPragmaValue(t, db, "user_version", fmt.Sprint(supportedSchemaVersion))

	if got := db.Stats().MaxOpenConnections; got != 1 {
		t.Errorf("MaxOpenConnections = %d, want 1", got)
	}
}

func TestOpenMigratesOnlyOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queue.db")

	db := openTestDatabase(t, path)
	insertJob(t, db, 1, "checksum", "pending")
	if err := db.Close(); err != nil {
		t.Fatalf("close first database: %v", err)
	}

	db = openTestDatabase(t, path)
	var count int
	if err := db.QueryRow("SELECT count(*) FROM jobs").Scan(&count); err != nil {
		t.Fatalf("count jobs: %v", err)
	}
	if count != 1 {
		t.Fatalf("job count after reopen = %d, want 1", count)
	}
	assertPragmaValue(t, db, "user_version", fmt.Sprint(supportedSchemaVersion))
}

func TestDatabaseConstraints(t *testing.T) {
	db := openTestDatabase(t, filepath.Join(t.TempDir(), "queue.db"))

	t.Run("job positive ID", func(t *testing.T) {
		for _, id := range []int64{0, -1} {
			_, err := db.Exec(`INSERT INTO jobs (
				id, document_id, source_checksum, priority, state, attempts, model,
				prompt_version, available_at, created_at, updated_at
			) VALUES (?, 10, ?, 0, 'pending', 0, 'model', 'v1',
				CURRENT_TIMESTAMP, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`, id, fmt.Sprintf("invalid-id-%d", id))
			assertConstraintError(t, err)
		}
	})

	t.Run("omitted job ID auto-allocates positive ID", func(t *testing.T) {
		id := insertJob(t, db, 11, "auto-job-id", "pending")
		if id <= 0 {
			t.Errorf("auto-allocated job ID = %d, want positive", id)
		}
	})

	t.Run("job state", func(t *testing.T) {
		_, err := db.Exec(`INSERT INTO jobs (
			document_id, source_checksum, priority, state, attempts, model,
			prompt_version, available_at, created_at, updated_at
		) VALUES (1, 'bad-state', 0, 'unknown', 0, 'model', 'v1',
			CURRENT_TIMESTAMP, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`)
		assertConstraintError(t, err)
	})

	t.Run("job positive document ID", func(t *testing.T) {
		_, err := db.Exec(`INSERT INTO jobs (
			document_id, source_checksum, priority, state, attempts, model,
			prompt_version, available_at, created_at, updated_at
		) VALUES (0, 'invalid-document', 0, 'pending', 0, 'model', 'v1',
			CURRENT_TIMESTAMP, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`)
		assertConstraintError(t, err)
	})

	t.Run("job nonnegative attempts", func(t *testing.T) {
		_, err := db.Exec(`INSERT INTO jobs (
			document_id, source_checksum, priority, state, attempts, model,
			prompt_version, available_at, created_at, updated_at
		) VALUES (2, 'invalid-attempts', 0, 'pending', -1, 'model', 'v1',
			CURRENT_TIMESTAMP, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`)
		assertConstraintError(t, err)
	})

	t.Run("job integer priority", func(t *testing.T) {
		_, err := db.Exec(`INSERT INTO jobs (
			document_id, source_checksum, priority, state, attempts, model,
			prompt_version, available_at, created_at, updated_at
		) VALUES (6, 'fractional-priority', 1.5, 'pending', 0, 'model', 'v1',
			CURRENT_TIMESTAMP, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`)
		assertConstraintError(t, err)
	})

	t.Run("job nonempty timestamps", func(t *testing.T) {
		_, err := db.Exec(`INSERT INTO jobs (
			document_id, source_checksum, priority, state, attempts, model,
			prompt_version, available_at, created_at, updated_at
		) VALUES (7, 'empty-timestamp', 0, 'pending', 0, 'model', 'v1',
			'', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`)
		assertConstraintError(t, err)
	})

	t.Run("current job uniqueness", func(t *testing.T) {
		insertJob(t, db, 3, "same-source", "pending")
		_, err := db.Exec(`INSERT INTO jobs (
			document_id, source_checksum, priority, state, attempts, model,
			prompt_version, available_at, created_at, updated_at
		) VALUES (3, 'same-source', 10, 'retry', 1, 'model', 'v1',
			CURRENT_TIMESTAMP, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`)
		assertConstraintError(t, err)

		if _, err := db.Exec("UPDATE jobs SET state = 'completed' WHERE document_id = 3"); err != nil {
			t.Fatalf("complete existing job: %v", err)
		}
		insertJob(t, db, 3, "same-source", "pending")
		if _, err := db.Exec(`INSERT INTO jobs (
			document_id, source_checksum, priority, state, attempts, model,
			prompt_version, available_at, created_at, updated_at
		) VALUES (3, 'same-source', 0, 'failed', 0, 'model', 'v1',
			CURRENT_TIMESTAMP, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`); err != nil {
			t.Fatalf("retain failed history: %v", err)
		}
	})

	t.Run("batch checks", func(t *testing.T) {
		jobID := insertJob(t, db, 4, "batch-checks", "pending")
		for name, values := range map[string]string{
			"page start": "0, 1, 200, 'png', 'pending', 0",
			"page range": "2, 1, 200, 'png', 'pending', 0",
			"DPI":        "1, 1, 0, 'png', 'pending', 0",
			"state":      "1, 1, 200, 'png', 'unknown', 0",
			"attempts":   "1, 1, 200, 'png', 'pending', -1",
		} {
			t.Run(name, func(t *testing.T) {
				_, err := db.Exec(fmt.Sprintf(`INSERT INTO batches (
						job_id, page_start, page_end, render_dpi, render_format,
						state, attempts, available_at, created_at, updated_at
					) VALUES (?, %s, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP,
						CURRENT_TIMESTAMP)`, values), jobID)
				assertConstraintError(t, err)
			})
		}
	})

	t.Run("batch positive ID", func(t *testing.T) {
		jobID := insertJob(t, db, 12, "batch-id", "pending")
		for page, id := range []int64{0, -1} {
			_, err := db.Exec(`INSERT INTO batches (
				id, job_id, page_start, page_end, render_dpi, render_format,
				state, attempts, available_at, created_at, updated_at
			) VALUES (?, ?, ?, ?, 200, 'png', 'pending', 0,
				CURRENT_TIMESTAMP, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`, id, jobID, page+1, page+1)
			assertConstraintError(t, err)
		}
	})

	t.Run("omitted batch ID auto-allocates positive ID", func(t *testing.T) {
		jobID := insertJob(t, db, 13, "auto-batch-id", "pending")
		result, err := db.Exec(`INSERT INTO batches (
			job_id, page_start, page_end, render_dpi, render_format, state,
			attempts, available_at, created_at, updated_at
		) VALUES (?, 1, 1, 200, 'png', 'pending', 0,
			CURRENT_TIMESTAMP, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`, jobID)
		if err != nil {
			t.Fatalf("insert batch without ID: %v", err)
		}
		id, err := result.LastInsertId()
		if err != nil {
			t.Fatalf("batch LastInsertId(): %v", err)
		}
		if id <= 0 {
			t.Errorf("auto-allocated batch ID = %d, want positive", id)
		}
	})

	t.Run("batch requires available timestamp", func(t *testing.T) {
		jobID := insertJob(t, db, 14, "batch-available-at", "pending")
		for name, availableAt := range map[string]any{
			"NULL":  nil,
			"empty": "",
		} {
			t.Run(name, func(t *testing.T) {
				_, err := db.Exec(`INSERT INTO batches (
					job_id, page_start, page_end, render_dpi, render_format,
					state, attempts, available_at, created_at, updated_at
				) VALUES (?, 1, 1, 200, 'png', 'pending', 0, ?,
					CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`, jobID, availableAt)
				assertConstraintError(t, err)
			})
		}
	})

	t.Run("safe error columns", func(t *testing.T) {
		for _, table := range []string{"jobs", "batches"} {
			columns := tableColumns(t, db, table)
			if !columns["error_category"] || !columns["error_message"] {
				t.Errorf("%s safe error columns = %v", table, columns)
			}
			for _, forbidden := range []string{"provider_body", "raw_error", "error_body"} {
				if columns[forbidden] {
					t.Errorf("%s unexpectedly contains unsafe column %q", table, forbidden)
				}
			}
		}
	})

	t.Run("completed batch requires nonempty result", func(t *testing.T) {
		jobID := insertJob(t, db, 8, "empty-result", "pending")
		for name, resultText := range map[string]any{
			"NULL":  nil,
			"empty": "",
		} {
			t.Run(name, func(t *testing.T) {
				_, err := db.Exec(`INSERT INTO batches (
					job_id, page_start, page_end, render_dpi, render_format,
					state, attempts, available_at, result_text, created_at, updated_at
				) VALUES (?, 1, 1, 200, 'png', 'completed', 0,
					CURRENT_TIMESTAMP, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`, jobID, resultText)
				assertConstraintError(t, err)
			})
		}
	})
}

func TestForeignKeysAndBatchDeleteBehavior(t *testing.T) {
	db := openTestDatabase(t, filepath.Join(t.TempDir(), "queue.db"))

	_, err := db.Exec(`INSERT INTO batches (
		job_id, page_start, page_end, render_dpi, render_format, state,
		attempts, available_at, created_at, updated_at
	) VALUES (999, 1, 1, 200, 'png', 'pending', 0,
		CURRENT_TIMESTAMP, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`)
	assertConstraintError(t, err)

	jobID := insertJob(t, db, 5, "cascade", "pending")
	if _, err := db.Exec(`INSERT INTO batches (
		job_id, page_start, page_end, render_dpi, render_format, state,
		attempts, available_at, result_text, created_at, updated_at
	) VALUES (?, 1, 5, 200, 'png', 'completed', 0,
		CURRENT_TIMESTAMP, 'validated text', CURRENT_TIMESTAMP,
		CURRENT_TIMESTAMP)`, jobID); err != nil {
		t.Fatalf("insert batch: %v", err)
	}
	if _, err := db.Exec("DELETE FROM jobs WHERE id = ?", jobID); err != nil {
		t.Fatalf("delete job: %v", err)
	}
	var count int
	if err := db.QueryRow("SELECT count(*) FROM batches WHERE job_id = ?", jobID).Scan(&count); err != nil {
		t.Fatalf("count batches: %v", err)
	}
	if count != 0 {
		t.Errorf("batch count after job deletion = %d, want 0", count)
	}
}

func TestOpenRestrictsDatabasePermissions(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("permission policy is verified on Linux")
	}

	t.Run("new file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "queue.db")
		db := openTestDatabase(t, path)
		if err := db.Close(); err != nil {
			t.Fatalf("close database: %v", err)
		}
		assertFileMode(t, path, 0o600)
	})

	t.Run("existing broad file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "queue.db")
		file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o666)
		if err != nil {
			t.Fatalf("create broad database file: %v", err)
		}
		if err := file.Close(); err != nil {
			t.Fatalf("close broad database file: %v", err)
		}
		if err := os.Chmod(path, 0o666); err != nil {
			t.Fatalf("broaden database permissions: %v", err)
		}

		db := openTestDatabase(t, path)
		if err := db.Close(); err != nil {
			t.Fatalf("close database: %v", err)
		}
		assertFileMode(t, path, 0o600)
	})
}

func TestOpenRejectsNewerSchemaWithoutSensitiveDetails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sensitive-name.db")
	db := openTestDatabase(t, path)
	if _, err := db.Exec("PRAGMA user_version = 99"); err != nil {
		t.Fatalf("set newer user_version: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close database: %v", err)
	}

	_, err := Open(path)
	if err == nil {
		t.Fatal("Open() error = nil, want newer-schema error")
	}
	if !strings.Contains(err.Error(), "configuration:") {
		t.Errorf("Open() error = %q, want configuration category", err)
	}
	if strings.Contains(err.Error(), path) || strings.Contains(err.Error(), "sensitive-name") {
		t.Errorf("Open() error exposes database path: %q", err)
	}
}

func TestFailedMigrationRollsBackSchemaAndVersion(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("sql.Open(): %v", err)
	}
	t.Cleanup(func() { db.Close() })

	err = applyMigrations(db, []migration{{
		version: 1,
		sql: `CREATE TABLE partial (id INTEGER PRIMARY KEY);
			CREATE TABLE broken (id INTEGER PRIMARY KEY,);`,
	}})
	if err == nil {
		t.Fatal("applyMigrations() error = nil, want migration failure")
	}

	assertPragmaValue(t, db, "user_version", "0")
	var count int
	if err := db.QueryRow(
		"SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'partial'",
	).Scan(&count); err != nil {
		t.Fatalf("query partial table: %v", err)
	}
	if count != 0 {
		t.Errorf("partial table count = %d, want 0", count)
	}
}

func openTestDatabase(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func insertJob(t *testing.T, db *sql.DB, documentID int64, checksum, state string) int64 {
	t.Helper()
	result, err := db.Exec(`INSERT INTO jobs (
		document_id, source_checksum, priority, state, attempts, model,
		prompt_version, available_at, created_at, updated_at
	) VALUES (?, ?, 0, ?, 0, 'model', 'v1', CURRENT_TIMESTAMP,
		CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`, documentID, checksum, state)
	if err != nil {
		t.Fatalf("insert job: %v", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		t.Fatalf("job LastInsertId(): %v", err)
	}
	return id
}

func assertPragmaValue(t *testing.T, db *sql.DB, pragma, want string) {
	t.Helper()
	var got string
	if err := db.QueryRow("PRAGMA " + pragma).Scan(&got); err != nil {
		t.Fatalf("query PRAGMA %s: %v", pragma, err)
	}
	if got != want {
		t.Errorf("PRAGMA %s = %q, want %q", pragma, got, want)
	}
}

func assertConstraintError(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("constraint error = nil")
	}
}

func tableColumns(t *testing.T, db *sql.DB, table string) map[string]bool {
	t.Helper()
	rows, err := db.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		t.Fatalf("query columns for %s: %v", table, err)
	}
	defer rows.Close()

	columns := make(map[string]bool)
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, columnType string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			t.Fatalf("scan column for %s: %v", table, err)
		}
		columns[name] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate columns for %s: %v", table, err)
	}
	return columns
}

func assertFileMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat database: %v", err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Errorf("database permissions = %04o, want %04o", got, want)
	}
}

func TestOpenErrorIsSafe(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing", "secret-database.db")
	_, err := Open(path)
	if err == nil {
		t.Fatal("Open() error = nil, want missing-parent error")
	}
	if strings.Contains(err.Error(), path) || strings.Contains(err.Error(), "secret-database") {
		t.Errorf("Open() error exposes database path: %q", err)
	}
	var pathError *os.PathError
	if !errors.As(err, &pathError) {
		t.Error("Open() error does not retain private cause")
	}
}
