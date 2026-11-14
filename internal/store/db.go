// Package store provides SQLite persistence for FocusGate.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	// Register modernc pure-Go SQLite driver.
	_ "modernc.org/sqlite"
)

// Store wraps SQLite persistence configured for minimal RAM and disk usage.
type Store struct {
	db *sql.DB
}

// Open initializes the SQLite database at dbPath with WAL mode and a ~2 MB page cache.
func Open(dbPath string) (*Store, error) {
	if dbPath != ":memory:" {
		dir := filepath.Dir(dbPath)
		if dir != "" && dir != "." {
			if err := os.MkdirAll(dir, 0o700); err != nil {
				return nil, fmt.Errorf("create db dir: %w", err)
			}
		}
		f, err := os.OpenFile(dbPath, os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			return nil, fmt.Errorf("create db file: %w", err)
		}
		_ = f.Close()
	}

	dsn := dbPath + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=cache_size(-2000)&_pragma=temp_store(FILE)&_pragma=mmap_size(0)&_pragma=foreign_keys(1)&_time_format=sqlite"
	if dbPath == ":memory:" {
		dsn = "file::memory:?_pragma=busy_timeout(5000)&_pragma=cache_size(-2000)&_pragma=temp_store(FILE)&_pragma=mmap_size(0)&_pragma=foreign_keys(1)&_time_format=sqlite"
	}

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// Single connection keeps SQLite RAM minimal and avoids intra-process write lock contention.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)

	s := &Store{db: db}
	if err := s.migrate(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the underlying database handle.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// DBFileSizeBytes returns the current SQLite file size on disk, or 0 for in-memory DBs.
func (s *Store) DBFileSizeBytes(dbPath string) int64 {
	if dbPath == "" || dbPath == ":memory:" {
		return 0
	}
	info, err := os.Stat(dbPath)
	if err != nil {
		return 0
	}
	return info.Size()
}

func (s *Store) migrate(ctx context.Context) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS settings (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL,
			updated_at DATETIME NOT NULL
		);`,
		`CREATE TABLE IF NOT EXISTS feed_state (
			source_key TEXT PRIMARY KEY,
			etag TEXT NOT NULL DEFAULT '',
			last_modified TEXT NOT NULL DEFAULT '',
			last_uid INTEGER NOT NULL DEFAULT 0,
			updated_at DATETIME NOT NULL
		);`,
		`CREATE TABLE IF NOT EXISTS items (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			external_id TEXT NOT NULL,
			source_type TEXT NOT NULL,
			source_name TEXT NOT NULL,
			title TEXT NOT NULL,
			body TEXT NOT NULL,
			sender TEXT NOT NULL,
			url TEXT NOT NULL,
			tag TEXT NOT NULL,
			reason TEXT NOT NULL,
			route TEXT NOT NULL,
			priority INTEGER NOT NULL,
			confidence REAL NOT NULL,
			uncertain INTEGER NOT NULL DEFAULT 0,
			decision_source TEXT NOT NULL,
			decision_reason TEXT NOT NULL,
			signals_json TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL,
			published_at DATETIME NOT NULL,
			created_at DATETIME NOT NULL,
			UNIQUE(source_type, external_id)
		);`,
		`CREATE INDEX IF NOT EXISTS idx_items_status_created ON items(status, created_at DESC);`,
		`CREATE INDEX IF NOT EXISTS idx_items_created ON items(created_at DESC);`,
		`CREATE TABLE IF NOT EXISTS sessions (
			token TEXT PRIMARY KEY,
			expires_at DATETIME NOT NULL,
			created_at DATETIME NOT NULL
		);`,
		`CREATE INDEX IF NOT EXISTS idx_sessions_expires_at ON sessions(expires_at);`,
	}
	for _, stmt := range stmts {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("migrate schema: %w", err)
		}
	}
	return nil
}

// MaxQueuedBacklog caps the number of undelivered queued items retained before overflow
// transitions the oldest excess to archived status to bound SQLite database growth.
const MaxQueuedBacklog = 1000

// PruneOld removes completed items older than retentionDays to bound disk space.
// If digest delivery is unavailable, an overflow policy transitions undelivered queued items
// that exceed the retention window or backlog limit to 'archived' status so they remain
// visible in UI history rather than growing SQLite indefinitely or being silently deleted.
func (s *Store) PruneOld(ctx context.Context, retentionDays int) (int64, error) {
	if retentionDays <= 0 {
		return 0, nil
	}
	now := time.Now().UTC()
	cutoff := now.Add(-time.Duration(retentionDays) * 24 * time.Hour)

	// Overflow policy: Transition undelivered queued items older than the retention cutoff to 'archived',
	// resetting created_at to now so they remain visible in history for the retention period rather
	// than being silently deleted without visibility.
	if _, err := s.db.ExecContext(ctx,
		`UPDATE items
		 SET status = 'archived',
		     decision_reason = decision_reason || ' (digest backlog expired)',
		     created_at = ?
		 WHERE status = 'queued' AND created_at < ?`,
		now, cutoff,
	); err != nil {
		return 0, fmt.Errorf("archive expired queued items: %w", err)
	}

	// Cap maximum queued backlog to prevent unbounded storage during prolonged digest delivery outages.
	if _, err := s.db.ExecContext(ctx,
		`UPDATE items
		 SET status = 'archived',
		     decision_reason = decision_reason || ' (digest backlog cap reached)',
		     created_at = ?
		 WHERE id IN (
			SELECT id FROM items WHERE status = 'queued'
			ORDER BY created_at ASC
			LIMIT MAX(0, (SELECT COUNT(*) FROM items WHERE status = 'queued') - ?)
		 )`,
		now, MaxQueuedBacklog,
	); err != nil {
		return 0, fmt.Errorf("archive excess queued backlog: %w", err)
	}

	res, err := s.db.ExecContext(ctx,
		`DELETE FROM items WHERE status IN ('pushed', 'digested', 'archived', 'read') AND created_at < ?`,
		cutoff,
	)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	if n > 0 {
		_, _ = s.db.ExecContext(ctx, `PRAGMA optimize;`)
	}
	return n, nil
}
