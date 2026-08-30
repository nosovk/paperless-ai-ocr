package database

import (
	"database/sql"
	_ "embed"
	"fmt"

	"github.com/nosovk/paperless-ai-ocr/internal/saferr"
)

//go:embed migrations/001_initial.sql
var initialMigration string

//go:embed migrations/002_candidates.sql
var candidatesMigration string

//go:embed migrations/003_candidate_generation.sql
var candidateGenerationMigration string

//go:embed migrations/004_finalizations.sql
var finalizationsMigration string

type migration struct {
	version int
	sql     string
}

var migrations = []migration{
	{version: 1, sql: initialMigration},
	{version: 2, sql: candidatesMigration},
	{version: 3, sql: candidateGenerationMigration},
	{version: 4, sql: finalizationsMigration},
}

func applyMigrations(db *sql.DB, available []migration) error {
	var currentVersion int
	if err := db.QueryRow("PRAGMA user_version").Scan(&currentVersion); err != nil {
		return saferr.Wrap(
			saferr.CategoryInternal,
			"cannot read database schema version",
			err,
		)
	}

	supportedVersion := 0
	if len(available) > 0 {
		supportedVersion = available[len(available)-1].version
	}
	if currentVersion > supportedVersion {
		return saferr.New(
			saferr.CategoryConfiguration,
			"database schema is newer than this service supports",
		)
	}

	for _, migration := range available {
		if migration.version <= currentVersion {
			continue
		}
		if migration.version != currentVersion+1 {
			return saferr.New(
				saferr.CategoryInternal,
				"database migrations are not consecutive",
			)
		}
		if err := applyMigration(db, migration); err != nil {
			return err
		}
		currentVersion = migration.version
	}

	return nil
}

func applyMigration(db *sql.DB, migration migration) error {
	tx, err := db.Begin()
	if err != nil {
		return saferr.Wrap(
			saferr.CategoryInternal,
			"cannot begin database migration",
			err,
		)
	}

	if _, err := tx.Exec(migration.sql); err != nil {
		tx.Rollback()
		return saferr.Wrap(
			saferr.CategoryInternal,
			"cannot apply database migration",
			err,
		)
	}
	if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", migration.version)); err != nil {
		tx.Rollback()
		return saferr.Wrap(
			saferr.CategoryInternal,
			"cannot record database schema version",
			err,
		)
	}
	if err := tx.Commit(); err != nil {
		return saferr.Wrap(
			saferr.CategoryInternal,
			"cannot commit database migration",
			err,
		)
	}
	return nil
}
