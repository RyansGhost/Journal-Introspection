// Package store owns the SQLite database.
package store

import (
	"database/sql"
	_ "embed"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schema string

// indexVersion changes whenever files, entries, org_ids or search change shape.
const indexVersion = 2

type DB struct{ *sql.DB }

// Open opens (and creates if needed) the database in dir. Pass ":memory:" for tests.
func Open(dir string) (*DB, error) {
	dsn := "file::memory:?_pragma=foreign_keys(1)"
	if dir != ":memory:" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
		dsn = "file:" + filepath.Join(dir, "introspect.db") +
			"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)"
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// One connection keeps writes serialised and makes :memory: behave; the
	// workload is one user and one background worker.
	db.SetMaxOpenConns(1)
	// When the index layout changes, drop the index and let the next scan
	// rebuild it from the org files. Grades and sessions are kept.
	var version int
	db.QueryRow(`PRAGMA user_version`).Scan(&version)
	if version != indexVersion {
		if _, err := db.Exec(`DROP TABLE IF EXISTS entries; DROP TABLE IF EXISTS org_ids; DROP TABLE IF EXISTS search; DROP TABLE IF EXISTS files`); err != nil {
			db.Close()
			return nil, fmt.Errorf("reset index: %w", err)
		}
	}
	if _, err := db.Exec(schema + fmt.Sprintf("; PRAGMA user_version = %d", indexVersion)); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	return &DB{db}, nil
}
