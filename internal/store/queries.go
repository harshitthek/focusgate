package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"focusgate/internal/classify"
	"focusgate/internal/config"
	"focusgate/internal/ingest"
)

// StoredItem represents a persisted item alongside its focus routing metadata.
type StoredItem struct {
	ID             int64     `json:"id"`
	UserID         int64     `json:"user_id"`
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
	UserFeedID   int64
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
	return getUserFocusMode(ctx, s.db, 1, fallbackMode)
}

type itemDatabase interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func getUserFocusMode(ctx context.Context, db itemDatabase, userID int64, fallbackMode config.FocusMode) (config.FocusMode, error) {
	query := "SELECT value FROM settings WHERE key = 'focus_mode'"
	var args []any
	if userID > 1 {
		query = "SELECT value FROM user_settings WHERE key = 'focus_mode' AND user_id = ?"
		args = []any{userID}
	}
	var raw string
	err := db.QueryRowContext(ctx, query, args...).Scan(&raw)
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

// GetDailyUsage returns the consumed API calls recorded for dayKey (formatted YYYYMMDD).
func (s *Store) GetDailyUsage(ctx context.Context, dayKey string) (int, error) {
	var raw string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = 'typesafe_daily_usage'`).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return dailyCount(raw, dayKey), nil
}

// GetUserDailyUsage returns an account's semantic API usage. User 1 sees the
// overall usage against the operator's provider limit.
func (s *Store) GetUserDailyUsage(ctx context.Context, dayKey string, userID int64) (int, error) {
	if userID <= 1 {
		return s.GetDailyUsage(ctx, dayKey)
	}
	raw, err := s.GetUserSetting(ctx, userID, "typesafe_daily_usage")
	return dailyCount(raw, dayKey), err
}

func dailyCount(raw, dayKey string) int {
	prefix := dayKey + ":"
	if !strings.HasPrefix(raw, prefix) {
		return 0
	}
	countStr := strings.TrimPrefix(raw, prefix)
	count, convErr := strconv.Atoi(countStr)
	if convErr != nil {
		return 0
	}
	return max(0, count)
}

// SetDailyUsage persists today's consumed API call count for dayKey across daemon restarts.
func (s *Store) SetDailyUsage(ctx context.Context, dayKey string, count int) error {
	val := fmt.Sprintf("%s:%d", dayKey, count)
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO settings(key, value, updated_at) VALUES('typesafe_daily_usage', ?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at
	`, val, time.Now().UTC())
	return err
}

// ReserveDailyUsage increments provider and member quotas together. The first
// statement takes SQLite's write lock before consulting the current count.
func (s *Store) ReserveDailyUsage(ctx context.Context, dayKey string, userID int64, globalLimit, userLimit int) (bool, error) {
	if globalLimit <= 0 || (userID > 1 && userLimit <= 0) {
		return false, nil
	}
	if _, err := time.Parse("20060102", dayKey); err != nil {
		return false, fmt.Errorf("invalid quota day: %w", err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()

	now := time.Now().UTC()
	initial := dayKey + ":1"
	global, err := tx.ExecContext(ctx, `
		INSERT INTO settings(key, value, updated_at) VALUES('typesafe_daily_usage', ?, ?)
		ON CONFLICT(key) DO UPDATE SET
			value = CASE WHEN substr(value, 1, 8) = ? THEN ? || ':' || (max(0, CAST(substr(value, 10) AS INTEGER)) + 1)
				ELSE excluded.value END,
			updated_at = excluded.updated_at
		WHERE substr(value, 1, 8) < ?
			OR (substr(value, 1, 8) = ? AND CAST(substr(value, 10) AS INTEGER) < ?)
	`, initial, now, dayKey, dayKey, dayKey, dayKey, globalLimit)
	if err != nil {
		return false, err
	}
	if affected, err := global.RowsAffected(); err != nil || affected == 0 {
		return false, err
	}

	if userID > 1 {
		user, err := tx.ExecContext(ctx, `
			INSERT INTO user_settings(user_id, key, value, updated_at) VALUES(?, 'typesafe_daily_usage', ?, ?)
			ON CONFLICT(user_id, key) DO UPDATE SET
				value = CASE WHEN substr(value, 1, 8) = ? THEN ? || ':' || (max(0, CAST(substr(value, 10) AS INTEGER)) + 1)
					ELSE excluded.value END,
				updated_at = excluded.updated_at
			WHERE substr(value, 1, 8) < ?
				OR (substr(value, 1, 8) = ? AND CAST(substr(value, 10) AS INTEGER) < ?)
		`, userID, initial, now, dayKey, dayKey, dayKey, dayKey, userLimit)
		if err != nil {
			return false, err
		}
		if affected, err := user.RowsAffected(); err != nil || affected == 0 {
			return false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
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
		SELECT ?, ?, ?, ?, ?
		WHERE ? = 0 OR EXISTS(SELECT 1 FROM user_feeds WHERE id = ?)
		ON CONFLICT(source_key) DO UPDATE SET
			etag = excluded.etag,
			last_modified = excluded.last_modified,
			last_uid = excluded.last_uid,
			updated_at = excluded.updated_at
	`, st.SourceKey, st.ETag, st.LastModified, int64(st.LastUID), time.Now().UTC(), st.UserFeedID, st.UserFeedID)
	return err
}

// ExistsUser reports whether an item with (source_type, external_id) was already ingested for a specific user.
func (s *Store) ExistsUser(ctx context.Context, userID int64, sourceType, externalID string) (bool, error) {
	if userID <= 0 {
		userID = 1
	}
	var one int
	err := s.db.QueryRowContext(ctx,
		`SELECT 1 FROM items WHERE user_id = ? AND source_type = ? AND external_id = ? LIMIT 1`,
		userID, sourceType, externalID,
	).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// Exists reports whether an item with (source_type, external_id) was already ingested for default user (id 1).
func (s *Store) Exists(ctx context.Context, sourceType, externalID string) (bool, error) {
	return s.ExistsUser(ctx, 1, sourceType, externalID)
}

// InsertUserItem saves a newly classified item for a specific user, returning (storedItem, inserted, error).
func (s *Store) InsertUserItem(ctx context.Context, userID int64, item ingest.IncomingItem, dec classify.Decision) (StoredItem, bool, error) {
	return insertUserItem(ctx, s.db, userID, item, dec, false)
}

// InsertPendingItem reads the account mode and commits its routing decision under
// the same SQLite write lock as CLI and daemon mode changes. route must only
// apply local policy to signals already computed outside the transaction.
func (s *Store) InsertPendingItem(ctx context.Context, userID int64, item ingest.IncomingItem, fallback config.FocusMode, route func(config.FocusMode) classify.Decision) (StoredItem, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return StoredItem{}, false, err
	}
	defer tx.Rollback()
	mode, err := getUserFocusMode(ctx, tx, userID, fallback)
	if err != nil {
		return StoredItem{}, false, err
	}
	stored, inserted, err := insertUserItem(ctx, tx, userID, item, route(mode), true)
	if err != nil {
		return StoredItem{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return StoredItem{}, false, err
	}
	return stored, inserted, nil
}

func insertUserItem(ctx context.Context, db itemDatabase, userID int64, item ingest.IncomingItem, dec classify.Decision, pending bool) (StoredItem, bool, error) {
	if userID <= 0 {
		userID = 1
	}
	item.Normalize(4096)
	status := statusForRoute(dec.Route)
	if (userID > 1 || pending) && status == "pushed" {
		// Non-administrator users have no server-level push webhook dispatcher.
		// Retain deliverable state by marking the item as "queued" for user inbox triage.
		status = "queued"
	}
	var signalsJSON string
	if dec.Signals != nil {
		if raw, err := json.Marshal(dec.Signals); err == nil {
			signalsJSON = string(raw)
		}
	}
	now := time.Now().UTC()
	var initialPushUntil int64
	if pending && userID == 1 && dec.Route == "push_now" {
		initialPushUntil = now.Add(2 * time.Minute).Unix()
	}
	uncertainInt := 0
	if dec.Uncertain {
		uncertainInt = 1
	}

	res, err := db.ExecContext(ctx, `
		INSERT OR IGNORE INTO items (
			user_id, external_id, source_type, source_name, title, body, sender, url, tag, reason,
			route, priority, confidence, uncertain, decision_source, decision_reason, signals_json,
   status, published_at, created_at, initial_push_until
  ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		userID, item.ExternalID, item.SourceType, item.SourceName, item.Title, item.Body, item.Sender, item.URL, item.Tag, item.Reason,
		dec.Route, dec.Priority, dec.Confidence, uncertainInt, dec.Source, dec.Reason, signalsJSON,
		status, item.PublishedAt.UTC(), now, initialPushUntil,
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
		UserID:         userID,
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

// InsertItem saves a newly classified item for default user (id 1), or item.UserID if set.
func (s *Store) InsertItem(ctx context.Context, item ingest.IncomingItem, dec classify.Decision) (StoredItem, bool, error) {
	targetUID := item.UserID
	if targetUID <= 0 {
		targetUID = 1
	}
	return s.InsertUserItem(ctx, targetUID, item, dec)
}

// ItemFilter parameterizes listing stored items with optional filtering.
type ItemFilter struct {
	UserID int64
	Status string
	Source string
	Query  string
	Limit  int
	Offset int
}

// ListItems fetches up to limit items optionally filtered by user, status, source, and search keyword.
func (s *Store) ListItems(ctx context.Context, filter ItemFilter) ([]StoredItem, error) {
	limit := filter.Limit
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	offset := filter.Offset
	if offset < 0 {
		offset = 0
	}
	query := `
		SELECT id, user_id, substr(external_id, 1, 4096), substr(source_type, 1, 32),
		       substr(source_name, 1, 128), substr(title, 1, 512), substr(body, 1, 4096),
		       substr(sender, 1, 320), substr(url, 1, 2048), substr(tag, 1, 128), substr(reason, 1, 256),
		       route, priority, confidence, uncertain, substr(decision_source, 1, 64),
		       substr(decision_reason, 1, 1024), substr(signals_json, 1, 8192),
		       status, published_at, created_at
		FROM items
	`
	var whereClauses []string
	var args []any

	if filter.UserID > 0 {
		whereClauses = append(whereClauses, "user_id = ?")
		args = append(args, filter.UserID)
	}
	if filter.Status != "" && filter.Status != "all" {
		whereClauses = append(whereClauses, "status = ?")
		args = append(args, filter.Status)
	}
	if filter.Source != "" && filter.Source != "all" {
		whereClauses = append(whereClauses, "source_type = ?")
		args = append(args, filter.Source)
	}
	if q := strings.TrimSpace(filter.Query); q != "" {
		whereClauses = append(whereClauses, "(title LIKE ? OR body LIKE ? OR sender LIKE ? OR source_name LIKE ?)")
		pattern := "%" + q + "%"
		args = append(args, pattern, pattern, pattern, pattern)
	}

	if len(whereClauses) > 0 {
		query += " WHERE " + strings.Join(whereClauses, " AND ")
	}
	query += ` ORDER BY id DESC LIMIT ? OFFSET ?`
	args = append(args, limit, offset)

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
			&it.ID, &it.UserID, &it.ExternalID, &it.SourceType, &it.SourceName, &it.Title, &it.Body, &it.Sender, &it.URL, &it.Tag, &it.Reason,
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

// ListUserQueuedForDigest returns queued items ordered by priority descending for a user (or all users if userID=0).
func (s *Store) ListUserQueuedForDigest(ctx context.Context, userID int64, limit int) ([]StoredItem, error) {
	if limit <= 0 || limit > 100 {
		limit = 40
	}
	query := `
		SELECT id, user_id, substr(external_id, 1, 4096), substr(source_type, 1, 32),
		       substr(source_name, 1, 128), substr(title, 1, 512), substr(body, 1, 4096),
		       substr(sender, 1, 320), substr(url, 1, 2048), substr(tag, 1, 128), substr(reason, 1, 256),
		       route, priority, confidence, uncertain, substr(decision_source, 1, 64),
		       substr(decision_reason, 1, 1024), substr(signals_json, 1, 8192),
		       status, published_at, created_at
		FROM items
		WHERE status = 'queued' AND initial_push_until <= ?
	`
	args := []any{time.Now().Unix()}
	if userID > 0 {
		query += " AND user_id = ?"
		args = append(args, userID)
	}
	query += `
		ORDER BY (priority * 7200 + (strftime('%s', 'now') - strftime('%s', created_at))) DESC, id ASC
		LIMIT ?
	`
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
			&it.ID, &it.UserID, &it.ExternalID, &it.SourceType, &it.SourceName, &it.Title, &it.Body, &it.Sender, &it.URL, &it.Tag, &it.Reason,
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

// ListQueuedForDigest returns queued items ordered by priority descending for administrator (user 1).
// The scheduled background dispatcher uses this to deliver digests to the administrator's configured channels.
// Multi-tenant users consume queued items via ListItems (filtered by status=queued) or mobile sync.
func (s *Store) ListQueuedForDigest(ctx context.Context, limit int) ([]StoredItem, error) {
	return s.ListUserQueuedForDigest(ctx, 1, limit)
}

var ErrItemNotFound = errors.New("item not found")

// UpdateUserItemStatus sets status ('read', 'archived', 'digested', 'pushed', 'queued') on a single item, scoped to a specific user if userID > 0.
func (s *Store) UpdateUserItemStatus(ctx context.Context, userID int64, id int64, status string) error {
	var res sql.Result
	var err error
	if userID > 0 {
		res, err = s.db.ExecContext(ctx, `UPDATE items SET status = ? WHERE id = ? AND user_id = ?`, status, id, userID)
	} else {
		res, err = s.db.ExecContext(ctx, `UPDATE items SET status = ? WHERE id = ?`, status, id)
	}
	if err != nil {
		return err
	}
	count, err := res.RowsAffected()
	if err == nil && count == 0 {
		return ErrItemNotFound
	}
	return err
}

// UpdateItemStatus sets status ('read', 'archived', 'digested', 'pushed', 'queued') on a single item.
func (s *Store) UpdateItemStatus(ctx context.Context, id int64, status string) error {
	return s.UpdateUserItemStatus(ctx, 0, id, status)
}

// BulkUpdateUserStatus updates multiple item IDs (or all items matching fromStatus when ids is empty),
// scoped to a specific user if userID > 0 (unscoped across all items if userID <= 0).
func (s *Store) BulkUpdateUserStatus(ctx context.Context, userID int64, ids []int64, fromStatus, toStatus string) (int64, error) {
	if len(ids) == 0 {
		var res sql.Result
		var err error
		if fromStatus == "" || fromStatus == "all" {
			if toStatus != "read" && toStatus != "archived" {
				return 0, fmt.Errorf("wildcard bulk status update only permitted for terminal states (read, archived); got %q", toStatus)
			}
			if userID > 0 {
				res, err = s.db.ExecContext(ctx, `UPDATE items SET status = ? WHERE status != ? AND user_id = ?`, toStatus, toStatus, userID)
			} else {
				res, err = s.db.ExecContext(ctx, `UPDATE items SET status = ? WHERE status != ?`, toStatus, toStatus)
			}
		} else {
			if userID > 0 {
				res, err = s.db.ExecContext(ctx, `UPDATE items SET status = ? WHERE status = ? AND user_id = ?`, toStatus, fromStatus, userID)
			} else {
				res, err = s.db.ExecContext(ctx, `UPDATE items SET status = ? WHERE status = ?`, toStatus, fromStatus)
			}
		}
		if err != nil {
			return 0, err
		}
		return res.RowsAffected()
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	useFrom := fromStatus != "" && fromStatus != "all"
	query := `UPDATE items SET status = ? WHERE id = ?`
	if useFrom && userID > 0 {
		query = `UPDATE items SET status = ? WHERE id = ? AND status = ? AND user_id = ?`
	} else if useFrom {
		query = `UPDATE items SET status = ? WHERE id = ? AND status = ?`
	} else if userID > 0 {
		query = `UPDATE items SET status = ? WHERE id = ? AND user_id = ?`
	}
	stmt, err := tx.PrepareContext(ctx, query)
	if err != nil {
		return 0, err
	}
	defer stmt.Close()

	var total int64
	for _, id := range ids {
		var res sql.Result
		if useFrom && userID > 0 {
			res, err = stmt.ExecContext(ctx, toStatus, id, fromStatus, userID)
		} else if useFrom {
			res, err = stmt.ExecContext(ctx, toStatus, id, fromStatus)
		} else if userID > 0 {
			res, err = stmt.ExecContext(ctx, toStatus, id, userID)
		} else {
			res, err = stmt.ExecContext(ctx, toStatus, id)
		}
		if err != nil {
			return 0, err
		}
		n, _ := res.RowsAffected()
		total += n
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return total, nil
}

// BulkUpdateStatus updates multiple item IDs (or all items matching fromStatus when ids is empty)
// in one SQLite transaction and returns the number of updated rows.
func (s *Store) BulkUpdateStatus(ctx context.Context, ids []int64, fromStatus, toStatus string) (int64, error) {
	return s.BulkUpdateUserStatus(ctx, 0, ids, fromStatus, toStatus)
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

// GetUserStats returns aggregate counters for a specific user (or all users if userID=0).
func (s *Store) GetUserStats(ctx context.Context, userID int64, mode config.FocusMode) (Stats, error) {
	st := Stats{Mode: mode}
	now := time.Now()
	startOfDay := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).UTC()

	userClause := ""
	var userArg []any
	if userID > 0 {
		userClause = " AND user_id = ?"
		userArg = append(userArg, userID)
	}

	qQueued := "SELECT COUNT(*) FROM items WHERE status = 'queued'" + userClause
	if err := s.db.QueryRowContext(ctx, qQueued, userArg...).Scan(&st.QueuedCount); err != nil {
		return Stats{}, fmt.Errorf("count queued: %w", err)
	}

	argsDay := append([]any{startOfDay}, userArg...)
	qPushed := "SELECT COUNT(*) FROM items WHERE status = 'pushed' AND created_at >= ?" + userClause
	if err := s.db.QueryRowContext(ctx, qPushed, argsDay...).Scan(&st.PushedToday); err != nil {
		return Stats{}, fmt.Errorf("count pushed today: %w", err)
	}

	qDigested := "SELECT COUNT(*) FROM items WHERE status = 'digested' AND created_at >= ?" + userClause
	if err := s.db.QueryRowContext(ctx, qDigested, argsDay...).Scan(&st.DigestedToday); err != nil {
		return Stats{}, fmt.Errorf("count digested today: %w", err)
	}

	qArchived := "SELECT COUNT(*) FROM items WHERE status = 'archived' AND created_at >= ?" + userClause
	if err := s.db.QueryRowContext(ctx, qArchived, argsDay...).Scan(&st.ArchivedToday); err != nil {
		return Stats{}, fmt.Errorf("count archived today: %w", err)
	}

	qTotal := "SELECT COUNT(*) FROM items"
	if userID > 0 {
		qTotal += " WHERE user_id = ?"
	}
	if err := s.db.QueryRowContext(ctx, qTotal, userArg...).Scan(&st.TotalItems); err != nil {
		return Stats{}, fmt.Errorf("count total items: %w", err)
	}
	return st, nil
}

// GetStats returns aggregate counters for the dashboard across all users.
func (s *Store) GetStats(ctx context.Context, mode config.FocusMode) (Stats, error) {
	return s.GetUserStats(ctx, 0, mode)
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
