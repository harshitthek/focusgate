package phone

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"time"

	"focusgate/internal/classify"
	"focusgate/internal/config"
	"focusgate/internal/ingest"
)

type Notice struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Body  string `json:"body"`
	Slot  int64  `json:"slot,omitempty"`
}

func (e *Engine) Refresh(operationID, assessmentURL, token string) error {
	e.mu.Lock()
	if e.refreshing {
		e.mu.Unlock()
		return ErrBusy
	}
	e.refreshing = true
	e.mu.Unlock()
	defer func() { e.mu.Lock(); e.refreshing = false; e.mu.Unlock() }()
	ctx, err := e.networkContext(operationID)
	if err != nil {
		return err
	}
	subscriptions, err := feeds(ctx, e.db, "AND paused=0")
	if err != nil {
		return err
	}
	attempts := 8
	var failures []error
	for _, f := range subscriptions {
		var generation int64
		if err := e.db.QueryRowContext(ctx, "SELECT generation,etag,modified FROM feeds WHERE id=? AND removed=0 AND paused=0 AND url=? AND name=? AND assess=?", f.ID, f.URL, f.Name, f.Assess).Scan(&generation, &f.etag, &f.modified); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			return err
		}
		raw, cache, changed, err := e.fetch(ctx, f)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			failures = append(failures, fmt.Errorf("%s: refresh failed", f.Name))
			continue
		}
		candidates, err := e.saveBatch(ctx, f, generation, raw, cache, changed)
		if err != nil {
			return err
		}
		if !e.account || !f.Assess || assessmentURL == "" || token == "" {
			continue
		}
		for _, item := range candidates {
			if attempts == 0 || ctx.Err() != nil {
				break
			}
			assessmentContext, finish, err := e.beginAssessment(ctx, item.ID, f.ID, generation)
			if err != nil {
				return err
			}
			if assessmentContext == nil {
				continue
			}
			attempts--
			var response json.RawMessage
			err = postJSON(assessmentContext, assessmentURL, "/v1/assessments", token, map[string]string{
				"id": item.ID, "title": item.Title, "snippet": item.Snippet,
			}, &response, 8<<10)
			finish()
			if err != nil {
				continue
			}
			sig, err := classify.DecodeAssessment(response, "")
			if err == nil && sig.UrgencyConf >= 0.55 && sig.CategoryConf >= 0.55 {
				if err := e.applyAssessment(ctx, item.ID, f.ID, generation, sig); err != nil {
					return err
				}
			}
		}
	}
	return errors.Join(failures...)
}

func (e *Engine) saveBatch(ctx context.Context, f Feed, generation int64, raw []ingest.IncomingItem, cache ingest.FeedCache, changed bool) ([]Item, error) {
	tx, err := e.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var current, refreshed int64
	err = tx.QueryRowContext(ctx, "SELECT generation,refreshed FROM feeds WHERE id=? AND removed=0 AND paused=0", f.ID).Scan(&current, &refreshed)
	if errors.Is(err, sql.ErrNoRows) || err == nil && current != generation {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	s, err := settings(ctx, tx)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	var candidates []Item
	for _, incoming := range raw {
		incoming.SourceName = f.Name
		id := hash(f.ID + "\x00" + incoming.ExternalID)
		res, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO seen VALUES(?,?)", id, now.Unix())
		if err != nil {
			return nil, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return nil, err
		}
		if n == 0 {
			// Keep entries from a publisher with a long rolling backlog deduplicated.
			if _, err := tx.ExecContext(ctx, "UPDATE seen SET seen_at=? WHERE id=?", now.Unix(), id); err != nil {
				return nil, err
			}
			continue
		}
		var exists bool
		if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM inbox WHERE id=?)", id).Scan(&exists); err != nil {
			return nil, err
		}
		if exists {
			continue
		}
		match := classify.MatchRules(e.rules, incoming)
		sig := classify.HeuristicEvaluate(incoming)
		d := e.router.Route(s.Mode, match, sig)
		publish := min(incoming.PublishedAt.Unix(), now.Unix())
		item := Item{ID: id, FeedID: f.ID, Title: incoming.Title, URL: articleURL(f.URL, incoming.URL), Snippet: incoming.Body, Published: publish, Priority: e.router.Route(config.ModeNormal, match, sig).Priority}
		fresh := refreshed != 0 && publish >= now.Add(-48*time.Hour).Unix()
		delivery, digest := 0, 0
		if fresh {
			if s.Alerts && d.Route == "push_now" {
				delivery = 1
			} else if d.Route == "queue_digest" {
				digest = 1
			}
		}
		if match.Matched && d.Route == "archive" {
			item.Status = 2
		}
		result, err := json.Marshal(routeInput{Match: match, Signals: sig})
		if err != nil {
			return nil, err
		}
		if err := putItem(ctx, tx, item, delivery, digest, string(result), now.Unix()); err != nil {
			return nil, err
		}
		if e.account {
			if err := enqueue(tx, "item", id, 0, item); err != nil {
				return nil, err
			}
		}
		if fresh && !match.Matched && item.Status == 0 {
			candidates = append(candidates, item)
		}
	}
	if changed {
		_, err = tx.ExecContext(ctx, "UPDATE feeds SET refreshed=?,etag=?,modified=? WHERE id=?", now.Unix(), ingest.TruncateRunes(cache.ETag, 1024), ingest.TruncateRunes(cache.LastModified, 256), f.ID)
	} else {
		_, err = tx.ExecContext(ctx, "UPDATE feeds SET refreshed=? WHERE id=?", now.Unix(), f.ID)
	}
	if err != nil {
		return nil, err
	}
	if err := prune(ctx, tx, now); err != nil {
		return nil, err
	}
	return candidates, tx.Commit()
}

type routeInput struct {
	Match   classify.RuleMatch
	Signals classify.SmartSignals
}

func putItem(ctx context.Context, tx *sql.Tx, i Item, delivery, digest int, decision string, received int64) error {
	_, err := tx.ExecContext(ctx, "INSERT INTO inbox("+itemColumns+",delivery,digest,decision,received) VALUES(?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET status=max(status,excluded.status)",
		i.ID, i.FeedID, i.Title, i.URL, i.Snippet, i.Published, i.Status, i.Priority, delivery, digest, decision, received)
	return err
}

func articleURL(feed, raw string) string {
	if raw == "" {
		return ""
	}
	base, _ := url.Parse(feed)
	u, err := base.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" || u.User != nil || len(u.String()) > 2048 {
		return ""
	}
	return u.String()
}

func (e *Engine) beginAssessment(ctx context.Context, item, feed string, generation int64) (context.Context, func(), error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	allowed, err := assessmentEligible(ctx, e.db, item, feed, generation)
	if err != nil || !allowed {
		return nil, nil, err
	}
	assessmentCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	pending := &feedAssessment{feedID: feed, cancel: cancel}
	e.assessment = pending
	return assessmentCtx, func() {
		cancel()
		e.mu.Lock()
		defer e.mu.Unlock()
		if e.assessment == pending {
			e.assessment = nil
		}
	}, nil
}

func assessmentEligible(ctx context.Context, q queryer, id, feed string, generation int64) (bool, error) {
	var ok bool
	err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM inbox i JOIN feeds f ON f.id=i.feed_id
		WHERE i.id=? AND f.id=? AND i.status=0 AND f.generation=? AND f.assess=1 AND f.paused=0 AND f.removed=0)`, id, feed, generation).Scan(&ok)
	return ok, err
}

func (e *Engine) applyAssessment(ctx context.Context, id, feed string, generation int64, sig classify.SmartSignals) error {
	tx, err := e.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	allowed, err := assessmentEligible(ctx, tx, id, feed, generation)
	if err != nil || !allowed {
		return err
	}
	s, err := settings(ctx, tx)
	if err != nil {
		return err
	}
	d := e.router.Route(s.Mode, classify.RuleMatch{}, sig)
	p := e.router.Route(config.ModeNormal, classify.RuleMatch{}, sig).Priority
	raw, err := json.Marshal(routeInput{Signals: sig})
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE inbox SET priority=?, decision=?,
		delivery=CASE WHEN delivery=2 THEN 2 ELSE ? END, digest=? WHERE id=?`,
		p, string(raw), s.Alerts && d.Route == "push_now", d.Route == "queue_digest", id)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func prune(ctx context.Context, tx *sql.Tx, now time.Time) error {
	_, err := tx.ExecContext(ctx, `DELETE FROM inbox WHERE published < ? OR id IN
		(SELECT id FROM inbox ORDER BY published DESC,id LIMIT -1 OFFSET 1000)`, now.AddDate(0, 0, -30).Unix())
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM seen WHERE seen_at<?", now.AddDate(0, 0, -45).Unix()); err != nil {
		return err
	}
	// An evicted item is still eligible for sync if the user acted on it.
	// Keep its creation ahead of pending status updates until acknowledged.
	_, err = tx.ExecContext(ctx, `DELETE FROM outbox WHERE kind='item'
        AND NOT EXISTS(SELECT 1 FROM inbox WHERE inbox.id=outbox.entity)
        AND NOT EXISTS(SELECT 1 FROM outbox pending WHERE pending.entity=outbox.entity AND pending.kind='status')`)
	return err
}

func (e *Engine) Notices(zone string, offsetSeconds int) ([]Notice, error) {
	location, err := time.LoadLocation(zone)
	if err != nil {
		location = time.FixedZone("phone", offsetSeconds)
	}
	return e.noticesAt(time.Now().In(location))
}

func (e *Engine) noticesAt(now time.Time) ([]Notice, error) {
	ctx := context.Background()
	result := []Notice{}
	s, err := settings(ctx, e.db)
	if err != nil || !s.Alerts {
		return result, err
	}
	rows, err := e.db.QueryContext(ctx, `SELECT i.id,i.title,i.snippet,i.decision FROM inbox i JOIN feeds f ON f.id=i.feed_id
		WHERE i.status=0 AND i.delivery=1 AND i.received>=? AND f.removed=0 AND f.paused=0
		ORDER BY i.received DESC LIMIT 100`, now.Add(-2*time.Hour).Unix())
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var n Notice
		var raw string
		if err := rows.Scan(&n.ID, &n.Title, &n.Body, &raw); err != nil {
			rows.Close()
			return nil, err
		}
		var input routeInput
		if json.Unmarshal([]byte(raw), &input) == nil && e.router.Route(s.Mode, input.Match, input.Signals).Route == "push_now" {
			result = append(result, n)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	if s.Mode != config.ModeNormal {
		return result, nil
	}
	slot := latestDigest(now)
	var last int64
	var count int
	if err := e.db.QueryRow(`SELECT last_digest,(SELECT count(*) FROM inbox WHERE status=0 AND digest=1 AND received<? AND received>=preferences.last_digest
		AND feed_id IN (SELECT id FROM feeds WHERE paused=0 AND removed=0)) FROM preferences`, slot.Unix()).Scan(&last, &count); err != nil {
		return nil, err
	}
	if last < slot.Unix() && count > 0 {
		result = append(result, Notice{ID: "digest", Title: "FocusGate digest", Body: fmt.Sprintf("%d unread feed items", count), Slot: slot.Unix()})
	}
	return result, nil
}

func latestDigest(now time.Time) time.Time {
	year, month, day := now.Date()
	for _, t := range [][2]int{{19, 30}, {13, 30}, {9, 0}} {
		slot := time.Date(year, month, day, t[0], t[1], 0, 0, now.Location())
		if !slot.After(now) {
			return slot
		}
	}
	previous := now.AddDate(0, 0, -1)
	return time.Date(previous.Year(), previous.Month(), previous.Day(), 19, 30, 0, 0, now.Location())
}

func (e *Engine) Acknowledge(n Notice) error {
	if n.ID != "digest" {
		_, err := e.db.Exec("UPDATE inbox SET delivery=2 WHERE id=? AND delivery=1", n.ID)
		return err
	}
	_, err := e.db.Exec("UPDATE preferences SET last_digest=max(last_digest,?)", n.Slot)
	return err
}
