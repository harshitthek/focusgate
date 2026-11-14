// Package store provides SQLite persistence for FocusGate.
package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// GetSetting returns the value of a key from the settings table.
func (s *Store) GetSetting(ctx context.Context, key string) (string, error) {
	var val string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&val)
	if err != nil {
		return "", err
	}
	return val, nil
}

// SetSetting writes or updates a key-value setting.
func (s *Store) SetSetting(ctx context.Context, key, val string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO settings(key, value, updated_at) VALUES(?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at
	`, key, val, time.Now().UTC())
	return err
}

// DeleteSetting removes a key from the settings table.
func (s *Store) DeleteSetting(ctx context.Context, key string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM settings WHERE key = ?`, key)
	return err
}

// ErrAlreadyInitialized is returned when attempting to initialize a vault that is already configured.
var ErrAlreadyInitialized = errors.New("vault already initialized")

// InitializeMasterPassphrase atomically checks if a passphrase is already set and sets it if not.
// Returns ErrAlreadyInitialized if already set.
func (s *Store) InitializeMasterPassphrase(ctx context.Context, hashB64, saltB64 string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM settings WHERE key = 'auth_passphrase_hash'`).Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		return ErrAlreadyInitialized
	}

	now := time.Now().UTC()
	if _, err := tx.ExecContext(ctx, `INSERT INTO settings(key, value, updated_at) VALUES('auth_passphrase_hash', ?, ?)`, hashB64, now); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO settings(key, value, updated_at) VALUES('auth_passphrase_salt', ?, ?)`, saltB64, now); err != nil {
		return err
	}
	return tx.Commit()
}

// InvalidateAllSessions deletes all active sessions from the database.
func (s *Store) InvalidateAllSessions(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions`)
	return err
}

// IsAuthSetupRequired returns true if no master passphrase has been set in SQLite.
func (s *Store) IsAuthSetupRequired(ctx context.Context) (bool, error) {
	var count int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM settings WHERE key = 'auth_passphrase_hash'`).Scan(&count)
	if err != nil {
		return false, err
	}
	return count == 0, nil
}

// SetMasterPassphrase writes the hashed passphrase and salt to settings.
func (s *Store) SetMasterPassphrase(ctx context.Context, hashB64, saltB64 string) error {
	now := time.Now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO settings(key, value, updated_at) VALUES('auth_passphrase_hash', ?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at
	`, hashB64, now); err != nil {
		return err
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO settings(key, value, updated_at) VALUES('auth_passphrase_salt', ?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at
	`, saltB64, now); err != nil {
		return err
	}

	return tx.Commit()
}

// GetMasterPassphrase returns the stored passphrase hash and salt in a single snapshot.
func (s *Store) GetMasterPassphrase(ctx context.Context) (hashB64, saltB64 string, err error) {
	rows, err := s.db.QueryContext(ctx, `SELECT key, value FROM settings WHERE key IN ('auth_passphrase_hash', 'auth_passphrase_salt')`)
	if err != nil {
		return "", "", err
	}
	defer rows.Close()

	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return "", "", err
		}
		if k == "auth_passphrase_hash" {
			hashB64 = v
		} else if k == "auth_passphrase_salt" {
			saltB64 = v
		}
	}
	if err := rows.Err(); err != nil {
		return "", "", err
	}
	if hashB64 == "" || saltB64 == "" {
		return "", "", sql.ErrNoRows
	}
	return hashB64, saltB64, nil
}

// CreateSession registers a new session token with an expiration time.
func (s *Store) CreateSession(ctx context.Context, token string, expiresAt time.Time) error {
	now := time.Now().UTC()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO sessions(token, expires_at, created_at) VALUES(?, ?, ?)
		ON CONFLICT(token) DO UPDATE SET expires_at = excluded.expires_at
	`, token, expiresAt.UTC(), now)
	return err
}

// ValidateSession checks if a session token exists and has not expired.
func (s *Store) ValidateSession(ctx context.Context, token string) (bool, error) {
	if strings.TrimSpace(token) == "" {
		return false, nil
	}
	var expiresAt time.Time
	err := s.db.QueryRowContext(ctx, `SELECT expires_at FROM sessions WHERE token = ?`, token).Scan(&expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return time.Now().UTC().Before(expiresAt), nil
}

// DeleteSession invalidates a session token.
func (s *Store) DeleteSession(ctx context.Context, token string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE token = ?`, token)
	return err
}

// PruneSessions removes all expired sessions.
func (s *Store) PruneSessions(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at < ?`, time.Now().UTC())
	return err
}

// GetGitHubCredentials returns stored GitHub token, user JSON, and participating_only preference.
func (s *Store) GetGitHubCredentials(ctx context.Context) (token, userJSON string, participatingOnly bool, err error) {
	token, err = s.GetSetting(ctx, "github_token")
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", "", true, err
	}
	userJSON, err = s.GetSetting(ctx, "github_user")
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", "", true, err
	}
	partStr, err := s.GetSetting(ctx, "github_participating_only")
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", "", true, err
	}
	participatingOnly = (partStr != "false") // default to true if empty or set to true
	return token, userJSON, participatingOnly, nil
}

// IsGitHubDisabled returns whether GitHub polling was explicitly disconnected by the user.
func (s *Store) IsGitHubDisabled(ctx context.Context) (bool, error) {
	val, err := s.GetSetting(ctx, "github_disabled")
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return val == "true", nil
}

// SetGitHubCredentials saves the GitHub token, user profile JSON, and preference.
func (s *Store) SetGitHubCredentials(ctx context.Context, token, userJSON string, participatingOnly bool) error {
	now := time.Now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	partStr := "false"
	if participatingOnly {
		partStr = "true"
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM settings WHERE key = 'github_disabled'`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO settings(key, value, updated_at) VALUES('github_token', ?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at
	`, token, now); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO settings(key, value, updated_at) VALUES('github_user', ?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at
	`, userJSON, now); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO settings(key, value, updated_at) VALUES('github_participating_only', ?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at
	`, partStr, now); err != nil {
		return err
	}
	return tx.Commit()
}

// ClearGitHubCredentials removes GitHub credentials and marks polling as explicitly disabled.
func (s *Store) ClearGitHubCredentials(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	now := time.Now().UTC()
	if _, err := tx.ExecContext(ctx, `DELETE FROM settings WHERE key IN ('github_token', 'github_user', 'github_participating_only')`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO settings(key, value, updated_at) VALUES('github_disabled', 'true', ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at
	`, now); err != nil {
		return err
	}
	return tx.Commit()
}
