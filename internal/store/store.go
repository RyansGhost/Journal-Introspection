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

type DB struct{ *sql.DB }

// Open opens (and creates if needed) the database in dir. Pass ":memory:" for tests.
func Open(dir string) (*DB, error) {
	dsn := "file::memory:?cache=shared"
	if dir != ":memory:" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
		dsn = "file:" + filepath.Join(dir, "introspect.db")
	}
	dsn += "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)"
	if dir == ":memory:" {
		dsn = "file::memory:?_pragma=foreign_keys(1)"
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// One connection keeps writes serialised and makes :memory: behave; the
	// workload is one user and one background worker.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	return &DB{db}, nil
}
