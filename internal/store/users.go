package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"focusgate/internal/auth"
	"focusgate/internal/ingest"
)

var (
	ErrUserNotFound      = errors.New("user not found")
	ErrUserAlreadyExists = errors.New("username already exists")
	ErrInvalidPassword   = errors.New("invalid username or password")
	ErrInvalidFeed       = errors.New("invalid feed")
	ErrFeedAlreadyExists = errors.New("feed already exists")
	ErrFeedLimit         = errors.New("feed subscription limit reached")
	ErrFeedNotFound      = errors.New("feed not found")
)

const MaxUserFeeds = 8

// User represents an authenticated account in FocusGate.
type User struct {
	ID           int64     `json:"id"`
	Username     string    `json:"username"`
	PasswordHash string    `json:"-"`
	PasswordSalt string    `json:"-"`
	Role         string    `json:"role"` // "admin" or "user"
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// SessionUser holds authenticated identity metadata derived from a session token.
type SessionUser struct {
	UserID   int64  `json:"user_id"`
	Username string `json:"username"`
	Role     string `json:"role"`
}

// UserFeed represents an RSS/Atom source belonging to a specific user.
type UserFeed struct {
	ID          int64     `json:"id"`
	UserID      int64     `json:"user_id"`
	Name        string    `json:"name"`
	URL         string    `json:"url"`
	PollMinutes int       `json:"poll_minutes"`
	CreatedAt   time.Time `json:"created_at"`
}

// CreateUser registers a new user with standard credentials.
// If this is the first registered user in the database, role is automatically set to "admin".
func (s *Store) CreateUser(ctx context.Context, username, password string) (*User, error) {
	return s.CreateUserAuthorized(ctx, username, password, false)
}

// CreateUserAuthorized registers a new user, with bootstrapAuth indicating whether the caller
// proved authorization via the configured server token (allowing password reset if setup ran).
func (s *Store) CreateUserAuthorized(ctx context.Context, username, password string, bootstrapAuth bool) (*User, error) {
	username = strings.TrimSpace(username)
	if len(username) < 3 {
		return nil, errors.New("username must be at least 3 characters")
	}
	if err := auth.ValidateNewPassphrase(password); err != nil {
		return nil, err
	}

	hashB64, saltB64, err := auth.HashPassphrase(password)
	if err != nil {
		return nil, fmt.Errorf("hash password: %w", err)
	}

	now := time.Now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	var hasUsers bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM users)`).Scan(&hasUsers); err != nil {
		return nil, err
	}

	var existingID int64
	if err := tx.QueryRowContext(ctx, `SELECT id FROM users WHERE username = ? COLLATE NOCASE`, username).Scan(&existingID); err == nil {
		return nil, ErrUserAlreadyExists
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}

	if hasUsers && strings.EqualFold(username, "admin") {
		return nil, ErrUserAlreadyExists
	}

	role := "user"
	var res sql.Result
	if !hasUsers {
		role = "admin"
		mHash, mSalt, readErr := readMasterPassphrase(ctx, tx)
		absent := errors.Is(readErr, sql.ErrNoRows)
		if readErr != nil && !absent && !(bootstrapAuth && errors.Is(readErr, errIncompleteMaster)) {
			return nil, fmt.Errorf("read administrator credentials: %w", readErr)
		}
		if !bootstrapAuth && !absent && !auth.VerifyPassphrase(password, mHash, mSalt) {
			return nil, errors.New("admin setup already completed; credentials required")
		}
		if bootstrapAuth || absent {
			_, err = tx.ExecContext(ctx, `
				INSERT INTO settings(key, value, updated_at) VALUES('auth_passphrase_hash', ?, ?)
				ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at
			`, hashB64, now)
			if err != nil {
				return nil, fmt.Errorf("set admin passphrase hash: %w", err)
			}
			_, err = tx.ExecContext(ctx, `
				INSERT INTO settings(key, value, updated_at) VALUES('auth_passphrase_salt', ?, ?)
				ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at
			`, saltB64, now)
			if err != nil {
				return nil, fmt.Errorf("set admin passphrase salt: %w", err)
			}
		}
		// Invalidate existing sessions when replacing master credentials
		if _, sErr := tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = 1 OR user_id = 0`); sErr != nil {
			return nil, fmt.Errorf("invalidate admin sessions: %w", sErr)
		}
		// Explicitly reserve user ID 1 for administrator
		res, err = tx.ExecContext(ctx, `
			INSERT INTO users (id, username, password_hash, password_salt, role, created_at, updated_at)
			VALUES (1, ?, ?, ?, 'admin', ?, ?)
		`, username, hashB64, saltB64, now, now)
	} else {
		res, err = tx.ExecContext(ctx, `
			INSERT INTO users (username, password_hash, password_salt, role, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?)
		`, username, hashB64, saltB64, role, now, now)
	}
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") || strings.Contains(err.Error(), "constraint") {
			return nil, ErrUserAlreadyExists
		}
		return nil, fmt.Errorf("insert user: %w", err)
	}

	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}

	return &User{
		ID:           id,
		Username:     username,
		PasswordHash: hashB64,
		PasswordSalt: saltB64,
		Role:         role,
		CreatedAt:    now,
		UpdatedAt:    now,
	}, nil
}

// AuthenticateUser verifies a username and password and returns the User model if valid.
func (s *Store) AuthenticateUser(ctx context.Context, username, password string) (*User, error) {
	username = strings.TrimSpace(username)
	if username == "" || password == "" {
		return nil, ErrInvalidPassword
	}

	u, err := s.GetUserByUsername(ctx, username)
	if errors.Is(err, ErrUserNotFound) {
		return nil, ErrInvalidPassword
	}
	if err != nil {
		return nil, err
	}
	if !auth.VerifyPassphrase(password, u.PasswordHash, u.PasswordSalt) {
		return nil, ErrInvalidPassword
	}

	return u, nil
}

// GetUserByID looks up a user by primary key ID.
func (s *Store) GetUserByID(ctx context.Context, id int64) (*User, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, username, password_hash, password_salt, role, created_at, updated_at
		FROM users WHERE id = ?
	`, id)

	var u User
	if err := row.Scan(&u.ID, &u.Username, &u.PasswordHash, &u.PasswordSalt, &u.Role, &u.CreatedAt, &u.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrUserNotFound
		}
		return nil, err
	}
	return &u, nil
}

// GetUserByUsername looks up a user by username (case-insensitive).
func (s *Store) GetUserByUsername(ctx context.Context, username string) (*User, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, username, password_hash, password_salt, role, created_at, updated_at
		FROM users WHERE username = ? COLLATE NOCASE
	`, strings.TrimSpace(username))

	var u User
	if err := row.Scan(&u.ID, &u.Username, &u.PasswordHash, &u.PasswordSalt, &u.Role, &u.CreatedAt, &u.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrUserNotFound
		}
		return nil, err
	}
	return &u, nil
}

// HasUsers reports whether an account has been created.
func (s *Store) HasUsers(ctx context.Context) (bool, error) {
	var exists bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM users)`).Scan(&exists)
	return exists, err
}

// ListUsers returns all user accounts in ID order.
func (s *Store) ListUsers(ctx context.Context) ([]User, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, username, role, created_at, updated_at
		FROM users
		ORDER BY id ASC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var users []User
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.ID, &u.Username, &u.Role, &u.CreatedAt, &u.UpdatedAt); err != nil {
			return nil, err
		}
		users = append(users, u)
	}
	return users, rows.Err()
}

// CreateUserSession records an active session tied to a specific user.
func (s *Store) CreateUserSession(ctx context.Context, token string, userID int64, expiresAt time.Time) error {
	now := time.Now().UTC()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO sessions (token, user_id, expires_at, created_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(token) DO UPDATE SET user_id = excluded.user_id, expires_at = excluded.expires_at
	`, token, userID, expiresAt.UTC(), now)
	return err
}

// CreateUserSessionChecked records an active session atomically inside a transaction only if the user's
// current password hash or master passphrase hash matches the verified hash.
func (s *Store) CreateUserSessionChecked(ctx context.Context, token string, userID int64, expectedHash string, expiresAt time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if expectedHash != "" && userID > 0 {
		var currentHash string
		err := tx.QueryRowContext(ctx, `SELECT password_hash FROM users WHERE id = ?`, userID).Scan(&currentHash)
		if err != nil {
			if !errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("verify user credential version: %w", err)
			}
			// When user row does not exist, validate against master passphrase hash
			var masterHash string
			_ = tx.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = 'auth_passphrase_hash'`).Scan(&masterHash)
			if masterHash == "" || masterHash != expectedHash {
				return errors.New("user does not exist or credentials changed during login")
			}
		} else if currentHash != expectedHash {
			var masterHash string
			_ = tx.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = 'auth_passphrase_hash'`).Scan(&masterHash)
			if masterHash != expectedHash {
				return errors.New("credentials changed during login, please re-authenticate")
			}
		}
	}

	now := time.Now().UTC()
	_, err = tx.ExecContext(ctx, `
		INSERT INTO sessions (token, user_id, expires_at, created_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(token) DO UPDATE SET user_id = excluded.user_id, expires_at = excluded.expires_at
	`, token, userID, expiresAt.UTC(), now)
	if err != nil {
		return err
	}

	return tx.Commit()
}

// ValidateSessionUser retrieves the user identity for an active, unexpired session token.
func (s *Store) ValidateSessionUser(ctx context.Context, token string) (*SessionUser, error) {
	if strings.TrimSpace(token) == "" {
		return nil, nil
	}
	now := time.Now().UTC()
	var userID int64
	var username, role sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT s.user_id, u.username, u.role
		FROM sessions s
		LEFT JOIN users u ON s.user_id = u.id
		WHERE s.token = ? AND s.expires_at > ?
	`, token, now).Scan(&userID, &username, &role)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}

	if username.Valid && role.Valid {
		return &SessionUser{
			UserID:   userID,
			Username: username.String,
			Role:     role.String,
		}, nil
	}

	// Legacy fallback: only if s.user_id <= 1 (admin bootstrap / legacy session)
	if userID <= 1 {
		return &SessionUser{
			UserID:   1,
			Username: "admin",
			Role:     "admin",
		}, nil
	}

	// Missing / deleted non-admin user row; do not grant admin role
	return nil, nil
}

// InvalidateUserSessions deletes all active sessions for a specific user.
// When userID <= 1 (administrator), both user_id 1 and legacy user_id 0 sessions are removed.
func (s *Store) InvalidateUserSessions(ctx context.Context, userID int64) error {
	if userID <= 1 {
		_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = 1 OR user_id = 0`)
		return err
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ?`, userID)
	return err
}

// ChangeUserPassword verifies current password and updates to a new password in a single atomic transaction.
func (s *Store) ChangeUserPassword(ctx context.Context, userID int64, currentPassword, newPassword string) (string, error) {
	if err := auth.ValidateNewPassphrase(newPassword); err != nil {
		return "", err
	}

	newHash, newSalt, err := auth.HashPassphrase(newPassword)
	if err != nil {
		return "", fmt.Errorf("hash new password: %w", err)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback() }()

	var u User
	var hash, salt, role sql.NullString
	err = tx.QueryRowContext(ctx, `
		SELECT id, username, password_hash, password_salt, role, created_at, updated_at
		FROM users
		WHERE id = ?
	`, userID).Scan(&u.ID, &u.Username, &hash, &salt, &role, &u.CreatedAt, &u.UpdatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) && userID == 1 {
			u = User{ID: 1, Username: "admin", Role: "admin"}
		} else {
			return "", err
		}
	} else {
		u.PasswordHash = hash.String
		u.PasswordSalt = salt.String
		u.Role = role.String
	}

	currValid := auth.VerifyPassphrase(currentPassword, u.PasswordHash, u.PasswordSalt)
	if !currValid && (u.Role == "admin" || userID == 1) {
		// Fallback check against legacy master passphrase in settings if user row hash was empty or unset
		var mHash, mSalt string
		_ = tx.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = 'auth_passphrase_hash'`).Scan(&mHash)
		_ = tx.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = 'auth_passphrase_salt'`).Scan(&mSalt)
		if mHash != "" && mSalt != "" {
			currValid = auth.VerifyPassphrase(currentPassword, mHash, mSalt)
		}
	}
	if !currValid {
		return "", errors.New("current password incorrect")
	}

	now := time.Now().UTC()
	if u.PasswordHash != "" {
		res, err := tx.ExecContext(ctx, `
			UPDATE users SET password_hash = ?, password_salt = ?, updated_at = ?
			WHERE id = ? AND password_hash = ?
		`, newHash, newSalt, now, u.ID, u.PasswordHash)
		if err != nil {
			return "", err
		}
		rows, _ := res.RowsAffected()
		if rows == 0 {
			return "", errors.New("password update conflict: credentials changed concurrently")
		}
	} else {
		_, err = tx.ExecContext(ctx, `
			INSERT INTO users(id, username, password_hash, password_salt, role, created_at, updated_at)
			VALUES(?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(id) DO UPDATE SET password_hash = excluded.password_hash, password_salt = excluded.password_salt, updated_at = excluded.updated_at
			WHERE users.password_hash = '' OR users.password_hash IS NULL
		`, u.ID, u.Username, newHash, newSalt, u.Role, now, now)
		if err != nil {
			return "", err
		}
	}

	// If updating admin, synchronize legacy master passphrase settings in the same transaction
	if u.Role == "admin" || userID == 1 {
		_, err = tx.ExecContext(ctx, `
			INSERT INTO settings(key, value, updated_at) VALUES('auth_passphrase_hash', ?, ?)
			ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at
		`, newHash, now)
		if err != nil {
			return "", err
		}
		_, err = tx.ExecContext(ctx, `
			INSERT INTO settings(key, value, updated_at) VALUES('auth_passphrase_salt', ?, ?)
			ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at
		`, newSalt, now)
		if err != nil {
			return "", err
		}
		// Invalidate both user_id 1 and legacy user_id 0 sessions for admin
		if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = 1 OR user_id = 0`); err != nil {
			return "", err
		}
	} else {
		// Invalidate regular user sessions
		if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ?`, userID); err != nil {
			return "", err
		}
	}

	return newHash, tx.Commit()
}

// GetUserSetting loads a user-scoped setting key.
func (s *Store) GetUserSetting(ctx context.Context, userID int64, key string) (string, error) {
	var val string
	err := s.db.QueryRowContext(ctx, `
		SELECT value FROM user_settings WHERE user_id = ? AND key = ?
	`, userID, key).Scan(&val)
	if err != nil {
		return "", err
	}
	return val, nil
}

// SetUserSetting saves or updates a user-scoped setting key.
func (s *Store) SetUserSetting(ctx context.Context, userID int64, key, val string) error {
	now := time.Now().UTC()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO user_settings(user_id, key, value, updated_at)
		VALUES(?, ?, ?, ?)
		ON CONFLICT(user_id, key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at
	`, userID, key, val, now)
	return err
}

// DeleteUserSetting removes a user-scoped setting key.
func (s *Store) DeleteUserSetting(ctx context.Context, userID int64, key string) error {
	_, err := s.db.ExecContext(ctx, `
		DELETE FROM user_settings WHERE user_id = ? AND key = ?
	`, userID, key)
	return err
}

// ListUserFeeds returns all RSS/Atom sources configured by a specific user.
func (s *Store) ListUserFeeds(ctx context.Context, userID int64) ([]UserFeed, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, user_id, name, url, poll_minutes, created_at
		FROM user_feeds
		WHERE user_id = ?
		ORDER BY id ASC
	`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var feeds []UserFeed
	for rows.Next() {
		var f UserFeed
		if err := rows.Scan(&f.ID, &f.UserID, &f.Name, &f.URL, &f.PollMinutes, &f.CreatedAt); err != nil {
			return nil, err
		}
		feeds = append(feeds, f)
	}
	return feeds, rows.Err()
}

// ListDueUserFeeds returns a bounded batch ordered by when each feed became due.
func (s *Store) ListDueUserFeeds(ctx context.Context, now time.Time, limit int) ([]UserFeed, error) {
	if limit < 0 {
		return nil, errors.New("feed batch limit must not be negative")
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, user_id, name, url, poll_minutes, created_at
		FROM user_feeds WHERE next_poll_at <= ?
		ORDER BY next_poll_at, id LIMIT ?
	`, now.Unix(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var feeds []UserFeed
	for rows.Next() {
		var f UserFeed
		if err := rows.Scan(&f.ID, &f.UserID, &f.Name, &f.URL, &f.PollMinutes, &f.CreatedAt); err != nil {
			return nil, err
		}
		feeds = append(feeds, f)
	}
	return feeds, rows.Err()
}

// ScheduleUserFeed advances a due feed once, including across daemon processes.
func (s *Store) ScheduleUserFeed(ctx context.Context, feed UserFeed, now time.Time) (bool, error) {
	interval := min(1440, max(15, feed.PollMinutes))
	next := now.Add(time.Duration(interval) * time.Minute)
	res, err := s.db.ExecContext(ctx, `
		UPDATE user_feeds SET next_poll_at = ?
		WHERE id = ? AND user_id = ? AND next_poll_at <= ?
	`, next.Unix(), feed.ID, feed.UserID, now.Unix())
	if err != nil {
		return false, err
	}
	count, err := res.RowsAffected()
	return count == 1, err
}

// HasUserFeed checks whether a poll's subscription still belongs to its owner.
func (s *Store) HasUserFeed(ctx context.Context, userID, feedID int64) (bool, error) {
	var exists bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM user_feeds WHERE id = ? AND user_id = ?)`, feedID, userID).Scan(&exists)
	return exists, err
}

// AddUserFeed subscribes a user to an RSS/Atom source.
func (s *Store) AddUserFeed(ctx context.Context, userID int64, name, url string, pollMinutes int) (UserFeed, error) {
	name = strings.TrimSpace(name)
	url = strings.TrimSpace(url)
	if name == "" || len(name) > 100 || url == "" || len(url) > 2048 {
		return UserFeed{}, fmt.Errorf("%w: name and URL must be within 100 and 2048 bytes", ErrInvalidFeed)
	}
	if err := ingest.ValidateFeedURLContext(ctx, url); err != nil {
		return UserFeed{}, fmt.Errorf("%w: feed must be a public HTTP(S) URL", ErrInvalidFeed)
	}
	if pollMinutes <= 0 {
		pollMinutes = 30
	}
	if pollMinutes < 15 || pollMinutes > 1440 {
		return UserFeed{}, fmt.Errorf("%w: poll interval must be 15 to 1440 minutes", ErrInvalidFeed)
	}
	now := time.Now().UTC()
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO user_feeds (user_id, name, url, poll_minutes, created_at)
		SELECT ?, ?, ?, ?, ?
		WHERE (SELECT COUNT(*) FROM user_feeds WHERE user_id = ?) < ?
		ON CONFLICT(user_id, url) DO NOTHING
	`, userID, name, url, pollMinutes, now, userID, MaxUserFeeds)
	if err != nil {
		return UserFeed{}, fmt.Errorf("add user feed: %w", err)
	}
	if count, err := res.RowsAffected(); err != nil {
		return UserFeed{}, err
	} else if count == 0 {
		var exists bool
		if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM user_feeds WHERE user_id = ? AND url = ?)`, userID, url).Scan(&exists); err != nil {
			return UserFeed{}, err
		}
		if exists {
			return UserFeed{}, ErrFeedAlreadyExists
		}
		return UserFeed{}, ErrFeedLimit
	}
	id, _ := res.LastInsertId()
	return UserFeed{
		ID:          id,
		UserID:      userID,
		Name:        name,
		URL:         url,
		PollMinutes: pollMinutes,
		CreatedAt:   now,
	}, nil
}

// DeleteUserFeed removes a feed for a user.
func (s *Store) DeleteUserFeed(ctx context.Context, userID, feedID int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, `
		DELETE FROM user_feeds WHERE id = ? AND user_id = ?
	`, feedID, userID)
	if err != nil {
		return err
	}
	if rows, err := res.RowsAffected(); err != nil {
		return err
	} else if rows == 0 {
		return ErrFeedNotFound
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM feed_state WHERE source_key LIKE ?`, fmt.Sprintf("uf:%d:%%", feedID)); err != nil {
		return err
	}
	return tx.Commit()
}

// EnsureAdminUser ensures User 1 ("admin") is persisted in the users table and returns it.
// If setup has not yet occurred (no master passphrase or admin credentials), it returns an error
// without creating an empty-credential user row.
func (s *Store) EnsureAdminUser(ctx context.Context) (*User, error) {
	u, err := s.GetUserByID(ctx, 1)
	if err == nil && u != nil {
		return u, nil
	}

	hashB64, saltB64, _ := s.GetMasterPassphrase(ctx)
	if hashB64 == "" || saltB64 == "" {
		return nil, errors.New("admin setup incomplete")
	}

	now := time.Now().UTC()
	_, err = s.db.ExecContext(ctx, `
		INSERT OR IGNORE INTO users(id, username, password_hash, password_salt, role, created_at, updated_at)
		VALUES(1, 'admin', ?, ?, 'admin', ?, ?)
	`, hashB64, saltB64, now, now)
	if err != nil {
		return nil, err
	}
	return s.GetUserByID(ctx, 1)
}
