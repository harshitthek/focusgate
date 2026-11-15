// Package store provides SQLite persistence for FocusGate.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

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
		`CREATE TABLE IF NOT EXISTS users (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			username TEXT COLLATE NOCASE UNIQUE NOT NULL,
			password_hash TEXT NOT NULL,
			password_salt TEXT NOT NULL,
			role TEXT NOT NULL DEFAULT 'user',
			created_at DATETIME NOT NULL,
			updated_at DATETIME NOT NULL
		);`,
		`CREATE INDEX IF NOT EXISTS idx_users_username ON users(username);`,
		`CREATE TABLE IF NOT EXISTS feed_state (
			source_key TEXT PRIMARY KEY,
			etag TEXT NOT NULL DEFAULT '',
			last_modified TEXT NOT NULL DEFAULT '',
			last_uid INTEGER NOT NULL DEFAULT 0,
			updated_at DATETIME NOT NULL
		);`,
		`CREATE TABLE IF NOT EXISTS items (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			user_id INTEGER NOT NULL DEFAULT 1,
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
			UNIQUE(user_id, source_type, external_id)
		);`,
		`CREATE INDEX IF NOT EXISTS idx_items_status_created ON items(status, created_at DESC);`,
		`CREATE INDEX IF NOT EXISTS idx_items_created ON items(created_at DESC);`,
		`CREATE TABLE IF NOT EXISTS sessions (
			token TEXT PRIMARY KEY,
			user_id INTEGER NOT NULL DEFAULT 1,
			expires_at DATETIME NOT NULL,
			created_at DATETIME NOT NULL
		);`,
		`CREATE INDEX IF NOT EXISTS idx_sessions_expires_at ON sessions(expires_at);`,
		`CREATE TABLE IF NOT EXISTS user_settings (
			user_id INTEGER NOT NULL,
			key TEXT NOT NULL,
			value TEXT NOT NULL,
			updated_at DATETIME NOT NULL,
			PRIMARY KEY(user_id, key)
		);`,
		`CREATE TABLE IF NOT EXISTS user_feeds (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			user_id INTEGER NOT NULL,
			name TEXT NOT NULL,
			url TEXT NOT NULL,
			poll_minutes INTEGER NOT NULL DEFAULT 30,
			created_at DATETIME NOT NULL,
			UNIQUE(user_id, url)
		);`,
		`CREATE INDEX IF NOT EXISTS idx_user_feeds_user ON user_feeds(user_id);`,
	}
	// Inspect recovery state if previous migration was interrupted before schema creation
	var hasItems, hasItemsNew int
	_ = s.db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'items'`).Scan(&hasItems)
	_ = s.db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'items_new'`).Scan(&hasItemsNew)

	if hasItems == 0 && hasItemsNew > 0 {
		if _, err := s.db.ExecContext(ctx, `ALTER TABLE items_new RENAME TO items;`); err != nil {
			return fmt.Errorf("recover items_new to items: %w", err)
		}
	} else if hasItems > 0 && hasItemsNew > 0 {
		_, _ = s.db.ExecContext(ctx, `DROP TABLE items_new;`)
	}

	for _, stmt := range stmts {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("migrate schema: %w", err)
		}
	}

	// Safe column migration for items.user_id if running on existing database
	if !s.columnExists(ctx, "items", "user_id") {
		if _, err := s.db.ExecContext(ctx, `ALTER TABLE items ADD COLUMN user_id INTEGER NOT NULL DEFAULT 1;`); err != nil {
			return fmt.Errorf("migrate items user_id column: %w", err)
		}
	}

	// Inspect items table in sqlite_master: if it lacks user_id in its UNIQUE constraint, rebuild it atomically
	var tableSQL string
	_ = s.db.QueryRowContext(ctx, `SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'items'`).Scan(&tableSQL)
	normSQL := strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, tableSQL)
	if strings.Contains(normSQL, "UNIQUE(source_type,external_id)") || (tableSQL != "" && !strings.Contains(normSQL, "UNIQUE(user_id,source_type,external_id)")) {
		hasUserID := s.columnExists(ctx, "items", "user_id")
		copyStmt := `INSERT INTO items_new SELECT id, user_id, external_id, source_type, source_name, title, body, sender, url, tag, reason, route, priority, confidence, uncertain, decision_source, decision_reason, signals_json, status, published_at, created_at FROM items;`
		if !hasUserID {
			copyStmt = `INSERT INTO items_new (id, user_id, external_id, source_type, source_name, title, body, sender, url, tag, reason, route, priority, confidence, uncertain, decision_source, decision_reason, signals_json, status, published_at, created_at)
				SELECT id, 1, external_id, source_type, source_name, title, body, sender, url, tag, reason, route, priority, confidence, uncertain, decision_source, decision_reason, signals_json, status, published_at, created_at FROM items;`
		}

		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin items rebuild tx: %w", err)
		}
		defer func() { _ = tx.Rollback() }()

		rebuildStmts := []string{
			`CREATE TABLE items_new (
				id INTEGER PRIMARY KEY AUTOINCREMENT,
				user_id INTEGER NOT NULL DEFAULT 1,
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
				UNIQUE(user_id, source_type, external_id)
			);`,
			copyStmt,
			`DROP TABLE items;`,
			`ALTER TABLE items_new RENAME TO items;`,
		}
		// Capture existing sqlite_sequence for items before DROP TABLE items purges it
		var oldItemsSeq sql.NullInt64
		_ = tx.QueryRowContext(ctx, `SELECT seq FROM sqlite_sequence WHERE name = 'items'`).Scan(&oldItemsSeq)

		for _, stmt := range rebuildStmts {
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				return fmt.Errorf("execute items rebuild statement: %w", err)
			}
		}
		// Restore at least the captured sequence for items in the same transaction
		if oldItemsSeq.Valid && oldItemsSeq.Int64 > 0 {
			seqRes, err := tx.ExecContext(ctx, `UPDATE sqlite_sequence SET seq = MAX(seq, ?) WHERE name = 'items'`, oldItemsSeq.Int64)
			if err != nil {
				return fmt.Errorf("restore sqlite_sequence for items: %w", err)
			}
			if rows, _ := seqRes.RowsAffected(); rows == 0 {
				if _, err := tx.ExecContext(ctx, `INSERT INTO sqlite_sequence (name, seq) VALUES ('items', ?)`, oldItemsSeq.Int64); err != nil {
					return fmt.Errorf("insert restored sqlite_sequence for items: %w", err)
				}
			}
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit items rebuild tx: %w", err)
		}
	}
	_, _ = s.db.ExecContext(ctx, `DROP INDEX IF EXISTS idx_items_source_ext;`)

	// Safe column migration for sessions.user_id if running on existing database
	if !s.columnExists(ctx, "sessions", "user_id") {
		if _, err := s.db.ExecContext(ctx, `ALTER TABLE sessions ADD COLUMN user_id INTEGER NOT NULL DEFAULT 1;`); err != nil {
			return fmt.Errorf("migrate sessions user_id column: %w", err)
		}
	}

	// Create user-scoped indexes after validating user_id columns exist
	userIndexes := []string{
		`CREATE INDEX IF NOT EXISTS idx_items_status_created ON items(status, created_at DESC);`,
		`CREATE INDEX IF NOT EXISTS idx_items_created ON items(created_at DESC);`,
		`CREATE INDEX IF NOT EXISTS idx_items_user_status ON items(user_id, status, created_at DESC);`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_items_user_source_ext ON items(user_id, source_type, external_id);`,
		`CREATE INDEX IF NOT EXISTS idx_sessions_user_id ON sessions(user_id);`,
	}
	for _, stmt := range userIndexes {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("create user index: %w", err)
		}
	}

	// Migrate legacy single-tenant passphrase into users table if no users exist
	var userCount int
	_ = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&userCount)
	if userCount == 0 {
		// Ensure sqlite_sequence for users starts at 1 so AUTOINCREMENT never assigns 1 to regular users
		seqRes, err := s.db.ExecContext(ctx, `UPDATE sqlite_sequence SET seq = MAX(seq, 1) WHERE name = 'users'`)
		if err != nil {
			return fmt.Errorf("update sqlite_sequence for users: %w", err)
		}
		if rows, _ := seqRes.RowsAffected(); rows == 0 {
			if _, err := s.db.ExecContext(ctx, `INSERT INTO sqlite_sequence (name, seq) VALUES ('users', 1)`); err != nil {
				return fmt.Errorf("insert sqlite_sequence for users: %w", err)
			}
		}
		var hashB64, saltB64 string
		err = s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = 'auth_passphrase_hash'`).Scan(&hashB64)
		if err == nil && hashB64 != "" {
			_ = s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = 'auth_passphrase_salt'`).Scan(&saltB64)
			now := time.Now().UTC()
			if _, err := s.db.ExecContext(ctx, `
				INSERT INTO users(id, username, password_hash, password_salt, role, created_at, updated_at)
				VALUES(1, 'admin', ?, ?, 'admin', ?, ?)
				ON CONFLICT(id) DO UPDATE SET password_hash = excluded.password_hash, password_salt = excluded.password_salt, updated_at = excluded.updated_at
			`, hashB64, saltB64, now, now); err != nil {
				return fmt.Errorf("initialize administrator user: %w", err)
			}
		}
	}

	// Ensure non-admin users never have stranded "pushed" items from prior versions
	_, _ = s.db.ExecContext(ctx, `UPDATE items SET status = 'queued' WHERE user_id > 1 AND status = 'pushed'`)

	return nil
}

func (s *Store) columnExists(ctx context.Context, table, column string) bool {
	rows, err := s.db.QueryContext(ctx, fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return false
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dfltValue sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dfltValue, &pk); err == nil {
			if strings.EqualFold(name, column) {
				return true
			}
		}
	}
	return false
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
