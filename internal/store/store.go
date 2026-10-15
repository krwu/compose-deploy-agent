// Package store persists everything docker itself cannot answer: deploy
// history/audit, last_healthy versions, and client certificate
// registration/revocation. sqlite here is a supplement, never the
// authority, for "what is actually running" - that question is always
// answered by docker.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"time"

	_ "modernc.org/sqlite"
)

// Store wraps a sqlite database opened in WAL mode with a busy timeout, as
// required by the single-writer constraint.
type Store struct {
	db *sql.DB
}

// Open opens (creating if necessary) the sqlite database at path and
// applies schema migrations.
func Open(path string) (*Store, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// sqlite only tolerates a single writer; the process itself already
	// serializes writes, but cap the pool defensively.
	db.SetMaxOpenConns(1)

	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// OpenReadOnly opens an existing database without applying migrations or
// taking the writer role, so it is safe to use while `deploy-agent serve`
// owns the database (e.g. from `deploy-agent self-upgrade`).
func OpenReadOnly(path string) (*Store, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	dsn := fmt.Sprintf("file:%s?mode=ro&_pragma=busy_timeout(5000)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	return &Store{db: db}, nil
}

// Close closes the underlying database handle.
func (s *Store) Close() error {
	return s.db.Close()
}

func (store *Store) migrate() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS deploys (
			deploy_id          TEXT PRIMARY KEY,
			service            TEXT NOT NULL,
			target_tag         TEXT NOT NULL,
			running_tag        TEXT,
			running_image_id   TEXT,
			running_healthy    INTEGER,
			target_image_id    TEXT,
			state              TEXT NOT NULL,
			result             TEXT,
			rollback           TEXT,
			rolled_back_to     TEXT,
			message            TEXT,
			started_at         TEXT NOT NULL,
			finished_at        TEXT
		)`,
		`CREATE INDEX IF NOT EXISTS idx_deploys_service_time ON deploys(service, started_at DESC)`,
		`CREATE TABLE IF NOT EXISTS last_healthy (
			service    TEXT PRIMARY KEY,
			tag        TEXT NOT NULL,
			image_id   TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS client_certs (
			fingerprint TEXT PRIMARY KEY,
			name        TEXT NOT NULL,
			issued_at   TEXT NOT NULL,
			expires_at  TEXT NOT NULL,
			revoked_at  TEXT
		)`,
		`CREATE TABLE IF NOT EXISTS oidc_jti (
			jti        TEXT PRIMARY KEY,
			expires_at INTEGER NOT NULL
		)`,
	}
	for _, stmt := range stmts {
		if _, err := store.db.Exec(stmt); err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
	}
	return nil
}

const timeLayout = time.RFC3339

func formatTime(t time.Time) string {
	return t.UTC().Format(timeLayout)
}

func parseTime(s string) (time.Time, error) {
	return time.Parse(timeLayout, s)
}

// ctx is a small helper so call sites don't need to import context
// separately when they don't already have one to pass.
func ctx() context.Context { return context.Background() }
