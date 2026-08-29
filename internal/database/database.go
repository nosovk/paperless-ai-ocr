// Package database opens and migrates the service's durable SQLite database.
package database

import (
	"database/sql"
	"fmt"
	"net/url"
	"os"

	"github.com/nosovk/paperless-ai-ocr/internal/saferr"
	_ "modernc.org/sqlite"
)

const busyTimeoutMilliseconds = 5000

// Open opens the SQLite database at path and applies all supported migrations.
// The caller must create the parent directory before calling Open.
func Open(path string) (*sql.DB, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, saferr.Wrap(
			saferr.CategoryConfiguration,
			"cannot open database file",
			err,
		)
	}
	if err := file.Close(); err != nil {
		return nil, saferr.Wrap(
			saferr.CategoryInternal,
			"cannot close database file during setup",
			err,
		)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return nil, saferr.Wrap(
			saferr.CategoryConfiguration,
			"cannot secure database file permissions",
			err,
		)
	}

	db, err := sql.Open("sqlite", dataSourceName(path))
	if err != nil {
		return nil, saferr.Wrap(
			saferr.CategoryConfiguration,
			"cannot initialize database",
			err,
		)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	if err := db.Ping(); err != nil {
		db.Close()
		return nil, saferr.Wrap(
			saferr.CategoryConfiguration,
			"cannot connect to database",
			err,
		)
	}
	if err := applyMigrations(db, migrations); err != nil {
		db.Close()
		return nil, err
	}

	return db, nil
}

func dataSourceName(path string) string {
	uri := url.URL{Scheme: "file", Path: path}
	query := url.Values{}
	query.Add("_pragma", "foreign_keys(1)")
	query.Add("_pragma", fmt.Sprintf("busy_timeout(%d)", busyTimeoutMilliseconds))
	query.Add("_pragma", "journal_mode(WAL)")
	uri.RawQuery = query.Encode()
	return uri.String()
}
