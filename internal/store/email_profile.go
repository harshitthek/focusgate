package store

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"strings"
	"time"
)

// EmailCredentials holds the stored IMAP credentials and preferences for the user's email account.
type EmailCredentials struct {
	Enabled     bool   `json:"enabled"`
	Provider    string `json:"provider"`
	Host        string `json:"host"`
	Port        int    `json:"port"`
	Username    string `json:"username"`
	Password    string `json:"-"`
	Mailbox     string `json:"mailbox"`
	PollMinutes int    `json:"poll_minutes"`
}

// GetEmailCredentials retrieves stored email credentials from SQLite settings.
func (s *Store) GetEmailCredentials(ctx context.Context) (EmailCredentials, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT key, value FROM settings WHERE key IN (
 'email_poll_minutes', 'email_enabled', 'email_provider', 'email_host', 'email_port',
 'email_username', 'email_password', 'email_mailbox')`)
	if err != nil {
		return EmailCredentials{}, err
	}
	defer rows.Close()
	values := make(map[string]string, 8)
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return EmailCredentials{}, err
		}
		values[key] = value
	}
	if err := rows.Err(); err != nil {
		return EmailCredentials{}, err
	}
	pollMin := 5
	if pm, err := strconv.Atoi(values["email_poll_minutes"]); err == nil && pm > 0 {
		pollMin = pm
	}
	if values["email_enabled"] != "true" {
		return EmailCredentials{PollMinutes: pollMin}, nil
	}
	provider, host, portStr := values["email_provider"], values["email_host"], values["email_port"]
	username, password, mailbox := values["email_username"], values["email_password"], values["email_mailbox"]

	port := 993
	if p, err := strconv.Atoi(portStr); err == nil && p > 0 {
		port = p
	}
	if mailbox == "" {
		mailbox = "INBOX"
	}

	return EmailCredentials{
		Enabled:     true,
		Provider:    provider,
		Host:        host,
		Port:        port,
		Username:    username,
		Password:    password,
		Mailbox:     mailbox,
		PollMinutes: pollMin,
	}, nil
}

// IsEmailDisabled returns whether email polling was explicitly disconnected by the user.
func (s *Store) IsEmailDisabled(ctx context.Context) (bool, error) {
	val, err := s.GetSetting(ctx, "email_disabled")
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return val == "true", nil
}

// SetEmailCredentials saves the email credentials and preferences to SQLite.
func (s *Store) SetEmailCredentials(ctx context.Context, creds EmailCredentials) error {
	return s.setEmailCredentials(ctx, creds, "")
}

func (s *Store) SetEmailCredentialsIfCurrent(ctx context.Context, creds EmailCredentials, generation string) error {
	if generation == "" {
		return ErrConnectionChanged
	}
	return s.setEmailCredentials(ctx, creds, generation)
}

func (s *Store) setEmailCredentials(ctx context.Context, creds EmailCredentials, generation string) error {
	now := time.Now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if generation != "" {
		if err := checkConnection(ctx, tx, "email", generation); err != nil {
			return err
		}
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM settings WHERE key = 'email_disabled'`); err != nil {
		return err
	}

	port := creds.Port
	if port <= 0 {
		port = 993
	}
	mailbox := strings.TrimSpace(creds.Mailbox)
	if mailbox == "" {
		mailbox = "INBOX"
	}
	pollMin := creds.PollMinutes
	if pollMin <= 0 {
		pollMin = 5
	}
	provider := strings.TrimSpace(creds.Provider)
	if provider == "" {
		provider = "gmail"
	}

	settings := map[string]string{
		"email_enabled":      "true",
		"email_provider":     provider,
		"email_host":         strings.TrimSpace(creds.Host),
		"email_port":         strconv.Itoa(port),
		"email_username":     strings.TrimSpace(creds.Username),
		"email_password":     creds.Password,
		"email_mailbox":      mailbox,
		"email_poll_minutes": strconv.Itoa(pollMin),
	}

	for k, v := range settings {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO settings(key, value, updated_at) VALUES(?, ?, ?)
			ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at
		`, k, v, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// SetEmailPollMinutes updates the email polling interval.
func (s *Store) SetEmailPollMinutes(ctx context.Context, minutes int) error {
	if minutes <= 0 {
		minutes = 5
	}
	return s.SetSetting(ctx, "email_poll_minutes", strconv.Itoa(minutes))
}

// ClearEmailCredentials removes stored email credentials and marks polling as explicitly disabled.
func (s *Store) ClearEmailCredentials(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := invalidateConnection(ctx, tx, "email"); err != nil {
		return err
	}

	now := time.Now().UTC()

	if _, err := tx.ExecContext(ctx, `
		DELETE FROM settings WHERE key IN (
			'email_enabled', 'email_provider', 'email_host', 'email_port',
			'email_username', 'email_password', 'email_mailbox', 'email_poll_minutes'
		)
	`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO settings(key, value, updated_at) VALUES('email_disabled', 'true', ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at
	`, now); err != nil {
		return err
	}
	return tx.Commit()
}
