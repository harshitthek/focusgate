package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"focusgate/internal/classify"
	"focusgate/internal/config"
	"focusgate/internal/ingest"
)

// StoredItem represents a persisted item alongside its focus routing metadata.
type StoredItem struct {
	ID             int64     `json:"id"`
	ExternalID     string    `json:"external_id"`
	SourceType     string    `json:"source_type"`
	SourceName     string    `json:"source_name"`
	Title          string    `json:"title"`
	Body           string    `json:"body"`
	Sender         string    `json:"sender"`
	URL            string    `json:"url"`
	Tag            string    `json:"tag"`
	Reason         string    `json:"reason"`
	Route          string    `json:"route"`
	Priority       int       `json:"priority"`
	Confidence     float64   `json:"confidence"`
	Uncertain      bool      `json:"uncertain"`
	DecisionSource string    `json:"decision_source"`
	DecisionReason string    `json:"decision_reason"`
	SignalsJSON    string    `json:"signals_json,omitempty"`
	Status         string    `json:"status"` // pushed | queued | digested | archived | read
	PublishedAt    time.Time `json:"published_at"`
	CreatedAt      time.Time `json:"created_at"`
}

// FeedSyncState stores conditional GET or IMAP UID watermarks.
type FeedSyncState struct {
	SourceKey    string
	ETag         string
	LastModified string
	LastUID      uint32
}

// Stats summarizes stored notification counts for telemetry and UI badges.
type Stats struct {
	Mode          config.FocusMode `json:"mode"`
	QueuedCount   int              `json:"queued_count"`
	PushedToday   int              `json:"pushed_today"`
	DigestedToday int              `json:"digested_today"`
	ArchivedToday int              `json:"archived_today"`
	TotalItems    int              `json:"total_items"`
}

// GetFocusMode reads the active FocusMode from SQLite, falling back to fallbackMode.
func (s *Store) GetFocusMode(ctx context.Context, fallbackMode config.FocusMode) (config.FocusMode, error) {
	var raw string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = 'focus_mode'`).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return fallbackMode, nil
	}
	if err != nil {
		return fallbackMode, err
	}
	mode := config.FocusMode(raw)
	if !config.ValidFocusMode(mode) {
		return fallbackMode, nil
	}
	return mode, nil
}

// SetFocusMode persists the active FocusMode across daemon restarts.
func (s *Store) SetFocusMode(ctx context.Context, mode config.FocusMode) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO settings(key, value, updated_at) VALUES('focus_mode', ?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at
	`, string(mode), time.Now().UTC())
	return err
}

// GetFeedState returns cached conditional HTTP headers or IMAP UID watermark.
func (s *Store) GetFeedState(ctx context.Context, sourceKey string) (FeedSyncState, error) {
	state := FeedSyncState{SourceKey: sourceKey}
	var uid int64
	err := s.db.QueryRowContext(ctx,
		`SELECT etag, last_modified, last_uid FROM feed_state WHERE source_key = ?`,
		sourceKey,
	).Scan(&state.ETag, &state.LastModified, &uid)
	if errors.Is(err, sql.ErrNoRows) {
		return state, nil
	}
	if err != nil {
		return state, err
	}
	if uid > 0 {
		state.LastUID = uint32(uid)
	}
	return state, nil
}

// SaveFeedState persists updated HTTP cache headers or IMAP UID watermark.
func (s *Store) SaveFeedState(ctx context.Context, st FeedSyncState) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO feed_state(source_key, etag, last_modified, last_uid, updated_at)
		VALUES(?, ?, ?, ?, ?)
		ON CONFLICT(source_key) DO UPDATE SET
			etag = excluded.etag,
			last_modified = excluded.last_modified,
			last_uid = excluded.last_uid,
			updated_at = excluded.updated_at
	`, st.SourceKey, st.ETag, st.LastModified, int64(st.LastUID), time.Now().UTC())
	return err
}

// Exists reports whether an item with (source_type, external_id) was already ingested.
func (s *Store) Exists(ctx context.Context, sourceType, externalID string) (bool, error) {
	var one int
	err := s.db.QueryRowContext(ctx,
		`SELECT 1 FROM items WHERE source_type = ? AND external_id = ? LIMIT 1`,
		sourceType, externalID,
	).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// InsertItem saves a newly classified item, returning (storedItem, inserted, error).
// Duplicates on (source_type, external_id) return inserted=false with nil error.
func (s *Store) InsertItem(ctx context.Context, item ingest.IncomingItem, dec classify.Decision) (StoredItem, bool, error) {
	status := statusForRoute(dec.Route)
	var signalsJSON string
	if dec.Signals != nil {
		if raw, err := json.Marshal(dec.Signals); err == nil {
			signalsJSON = string(raw)
		}
	}
	now := time.Now().UTC()
	uncertainInt := 0
	if dec.Uncertain {
		uncertainInt = 1
	}

	res, err := s.db.ExecContext(ctx, `
		INSERT OR IGNORE INTO items (
			external_id, source_type, source_name, title, body, sender, url, tag, reason,
			route, priority, confidence, uncertain, decision_source, decision_reason, signals_json,
			status, published_at, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		item.ExternalID, item.SourceType, item.SourceName, item.Title, item.Body, item.Sender, item.URL, item.Tag, item.Reason,
		dec.Route, dec.Priority, dec.Confidence, uncertainInt, dec.Source, dec.Reason, signalsJSON,
		status, item.PublishedAt.UTC(), now,
	)
	if err != nil {
		return StoredItem{}, false, err
	}
	rows, _ := res.RowsAffected()
	if rows == 0 {
		return StoredItem{}, false, nil
	}
	id, _ := res.LastInsertId()

	return StoredItem{
		ID:             id,
		ExternalID:     item.ExternalID,
		SourceType:     item.SourceType,
		SourceName:     item.SourceName,
		Title:          item.Title,
		Body:           item.Body,
		Sender:         item.Sender,
		URL:            item.URL,
		Tag:            item.Tag,
		Reason:         item.Reason,
		Route:          dec.Route,
		Priority:       dec.Priority,
		Confidence:     dec.Confidence,
		Uncertain:      dec.Uncertain,
		DecisionSource: dec.Source,
		DecisionReason: dec.Reason,
		SignalsJSON:    signalsJSON,
		Status:         status,
		PublishedAt:    item.PublishedAt.UTC(),
		CreatedAt:      now,
	}, true, nil
}

// ListItems fetches up to limit items optionally filtered by status.
func (s *Store) ListItems(ctx context.Context, status string, limit int) ([]StoredItem, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	query := `
		SELECT id, external_id, source_type, source_name, title, body, sender, url, tag, reason,
		       route, priority, confidence, uncertain, decision_source, decision_reason, signals_json,
		       status, published_at, created_at
		FROM items
	`
	var args []any
	if status != "" && status != "all" {
		query += ` WHERE status = ?`
		args = append(args, status)
	}
	query += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []StoredItem
	for rows.Next() {
		var it StoredItem
		var unc int
		if err := rows.Scan(
			&it.ID, &it.ExternalID, &it.SourceType, &it.SourceName, &it.Title, &it.Body, &it.Sender, &it.URL, &it.Tag, &it.Reason,
			&it.Route, &it.Priority, &it.Confidence, &unc, &it.DecisionSource, &it.DecisionReason, &it.SignalsJSON,
			&it.Status, &it.PublishedAt, &it.CreatedAt,
		); err != nil {
			return nil, err
		}
		it.Uncertain = unc != 0
		out = append(out, it)
	}
	return out, rows.Err()
}

// ListQueuedForDigest returns queued items ordered by priority descending.
func (s *Store) ListQueuedForDigest(ctx context.Context, limit int) ([]StoredItem, error) {
	if limit <= 0 || limit > 100 {
		limit = 40
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, external_id, source_type, source_name, title, body, sender, url, tag, reason,
		       route, priority, confidence, uncertain, decision_source, decision_reason, signals_json,
		       status, published_at, created_at
		FROM items
		WHERE status = 'queued'
		ORDER BY priority DESC, id DESC
		LIMIT ?
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []StoredItem
	for rows.Next() {
		var it StoredItem
		var unc int
		if err := rows.Scan(
			&it.ID, &it.ExternalID, &it.SourceType, &it.SourceName, &it.Title, &it.Body, &it.Sender, &it.URL, &it.Tag, &it.Reason,
			&it.Route, &it.Priority, &it.Confidence, &unc, &it.DecisionSource, &it.DecisionReason, &it.SignalsJSON,
			&it.Status, &it.PublishedAt, &it.CreatedAt,
		); err != nil {
			return nil, err
		}
		it.Uncertain = unc != 0
		out = append(out, it)
	}
	return out, rows.Err()
}

// UpdateItemStatus sets status ('read', 'archived', 'digested', 'pushed') on a single item.
func (s *Store) UpdateItemStatus(ctx context.Context, id int64, status string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE items SET status = ? WHERE id = ?`, status, id)
	return err
}

// MarkDigested marks a batch of item IDs as digested after delivery.
func (s *Store) MarkDigested(ctx context.Context, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	stmt, err := tx.PrepareContext(ctx, `UPDATE items SET status = 'digested' WHERE id = ? AND status = 'queued'`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, id := range ids {
		if _, err := stmt.ExecContext(ctx, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// GetStats returns aggregate counters for the dashboard.
func (s *Store) GetStats(ctx context.Context, mode config.FocusMode) (Stats, error) {
	st := Stats{Mode: mode}
	now := time.Now()
	startOfDay := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).UTC()

	_ = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM items WHERE status = 'queued'`).Scan(&st.QueuedCount)
	_ = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM items WHERE status = 'pushed' AND created_at >= ?`, startOfDay).Scan(&st.PushedToday)
	_ = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM items WHERE status = 'digested' AND created_at >= ?`, startOfDay).Scan(&st.DigestedToday)
	_ = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM items WHERE status = 'archived' AND created_at >= ?`, startOfDay).Scan(&st.ArchivedToday)
	_ = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM items`).Scan(&st.TotalItems)
	return st, nil
}

func statusForRoute(route string) string {
	switch route {
	case "push_now":
		return "pushed"
	case "queue_digest":
		return "queued"
	default:
		return "archived"
	}
}
