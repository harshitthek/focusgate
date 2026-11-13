package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// NotifySettings represents user notification channel and mention filter preferences.
type NotifySettings struct {
	SlackEnabled     bool   `json:"slack_enabled"`
	SlackWebhookURL  string `json:"slack_webhook_url"`
	SlackMention     string `json:"slack_mention"`
	MatrixEnabled    bool   `json:"matrix_enabled"`
	MatrixWebhookURL string `json:"matrix_webhook_url"`
	MatrixMention    string `json:"matrix_mention"`
	OnlyMentions     bool   `json:"only_mentions"`
	MentionHandles   string `json:"mention_handles"`

	// HasKey records whether each setting key was explicitly found in the settings store.
	HasKey map[string]bool `json:"-"`
}

func (s *Store) getSettingPresence(ctx context.Context, key string) (string, bool, error) {
	val, err := s.GetSetting(ctx, key)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return val, true, nil
}

// GetNotifySettings loads notification channel settings from the SQLite settings table.
func (s *Store) GetNotifySettings(ctx context.Context) (NotifySettings, error) {
	ns := NotifySettings{
		HasKey: make(map[string]bool),
	}

	keys := []string{
		"notify_slack_enabled",
		"notify_slack_url",
		"notify_slack_mention",
		"notify_matrix_enabled",
		"notify_matrix_url",
		"notify_matrix_mention",
		"notify_only_mentions",
		"notify_mention_handles",
	}

	for _, k := range keys {
		val, ok, err := s.getSettingPresence(ctx, k)
		if err != nil {
			return ns, err
		}
		if !ok {
			continue
		}
		ns.HasKey[k] = true
		switch k {
		case "notify_slack_enabled":
			ns.SlackEnabled = strings.EqualFold(val, "true")
		case "notify_slack_url":
			ns.SlackWebhookURL = val
		case "notify_slack_mention":
			ns.SlackMention = val
		case "notify_matrix_enabled":
			ns.MatrixEnabled = strings.EqualFold(val, "true")
		case "notify_matrix_url":
			ns.MatrixWebhookURL = val
		case "notify_matrix_mention":
			ns.MatrixMention = val
		case "notify_only_mentions":
			ns.OnlyMentions = strings.EqualFold(val, "true")
		case "notify_mention_handles":
			ns.MentionHandles = val
		}
	}

	return ns, nil
}

// SetNotifySettings updates notification channel settings in the SQLite settings table.
func (s *Store) SetNotifySettings(ctx context.Context, ns NotifySettings) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	now := time.Now().UTC()
	settings := map[string]string{
		"notify_slack_enabled":   boolStr(ns.SlackEnabled),
		"notify_slack_url":       strings.TrimSpace(ns.SlackWebhookURL),
		"notify_slack_mention":   strings.TrimSpace(ns.SlackMention),
		"notify_matrix_enabled":  boolStr(ns.MatrixEnabled),
		"notify_matrix_url":      strings.TrimSpace(ns.MatrixWebhookURL),
		"notify_matrix_mention":  strings.TrimSpace(ns.MatrixMention),
		"notify_only_mentions":   boolStr(ns.OnlyMentions),
		"notify_mention_handles": strings.TrimSpace(ns.MentionHandles),
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

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
