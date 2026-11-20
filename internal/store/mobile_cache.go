package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"focusgate/internal/ingest"
)

// InitMobileCache adds server ID mappings to the device database only.
func (s *Store) InitMobileCache(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS mobile_item_links (
			item_id INTEGER PRIMARY KEY REFERENCES items(id) ON DELETE CASCADE,
			server_id INTEGER NOT NULL
		)
	`)
	return err
}

// ActivateMobileAccount makes a configuration/cache transition recoverable on restart.
func (s *Store) ActivateMobileAccount(ctx context.Context, owner string, equivalentOwners ...string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var previous string
	err = tx.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = 'mobile_cache_owner'`).Scan(&previous)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	sameOwner := previous == owner
	for _, equivalent := range equivalentOwners {
		if equivalent != "" && previous == equivalent {
			sameOwner = true
		}
	}
	if err == nil && !sameOwner {
		if _, err := tx.ExecContext(ctx, `DELETE FROM items`); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM settings WHERE key = 'focus_mode'`); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO settings(key, value, updated_at) VALUES('mobile_cache_owner', ?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at
	`, owner, time.Now().UTC()); err != nil {
		return err
	}
	return tx.Commit()
}

// ImportMobileItems applies one remote page atomically and preserves local row IDs.
func (s *Store) ImportMobileItems(ctx context.Context, items []StoredItem) error {
	if len(items) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	upsert, err := tx.PrepareContext(ctx, `
		INSERT INTO items (
			user_id, external_id, source_type, source_name, title, body, sender, url, tag, reason,
			route, priority, confidence, uncertain, decision_source, decision_reason, signals_json,
			status, published_at, created_at
		) VALUES (1, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(user_id, source_type, external_id) DO UPDATE SET
			source_name = excluded.source_name, title = excluded.title, body = excluded.body,
			sender = excluded.sender, url = excluded.url, tag = excluded.tag, reason = excluded.reason,
			route = excluded.route, priority = excluded.priority, confidence = excluded.confidence,
			uncertain = excluded.uncertain, decision_source = excluded.decision_source,
			decision_reason = excluded.decision_reason, signals_json = excluded.signals_json,
			status = excluded.status, published_at = excluded.published_at, created_at = excluded.created_at
		RETURNING id
	`)
	if err != nil {
		return err
	}
	defer upsert.Close()
	for _, item := range items {
		if item.ID <= 0 || item.ExternalID == "" || item.SourceType == "" {
			return errors.New("server response contains an item without an identity")
		}
		if item.CreatedAt.IsZero() {
			item.CreatedAt = time.Now().UTC()
		}
		if item.PublishedAt.IsZero() {
			item.PublishedAt = item.CreatedAt
		}
		if item.Status == "" {
			item.Status = statusForRoute(item.Route)
		}
		var localID int64
		err := upsert.QueryRowContext(ctx,
			item.ExternalID, item.SourceType, item.SourceName, item.Title, ingest.TruncateRunes(item.Body, 1200),
			item.Sender, item.URL, item.Tag, item.Reason, item.Route, item.Priority, item.Confidence,
			item.Uncertain, item.DecisionSource, item.DecisionReason, item.SignalsJSON, item.Status,
			item.PublishedAt.UTC(), item.CreatedAt.UTC(),
		).Scan(&localID)
		if err != nil {
			return fmt.Errorf("save remote item %d: %w", item.ID, err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO mobile_item_links(item_id, server_id) VALUES(?, ?)
			ON CONFLICT(item_id) DO UPDATE SET server_id = excluded.server_id
		`, localID, item.ID); err != nil {
			return fmt.Errorf("map remote item %d: %w", item.ID, err)
		}
	}
	return tx.Commit()
}

// MobileServerID returns zero for local-only items.
func (s *Store) MobileServerID(ctx context.Context, localID int64) (int64, error) {
	var serverID int64
	err := s.db.QueryRowContext(ctx, `SELECT server_id FROM mobile_item_links WHERE item_id = ?`, localID).Scan(&serverID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return serverID, err
}
