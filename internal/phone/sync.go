package phone

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"focusgate/internal/ingest"
)

type operation struct {
	ID     string          `json:"id"`
	Kind   string          `json:"kind"`
	Entity string          `json:"entity"`
	Base   int64           `json:"base"`
	Data   json.RawMessage `json:"data"`
}

type change struct {
	Kind string          `json:"kind"`
	Data json.RawMessage `json:"data"`
}

type acknowledgement struct {
	ID       string `json:"id"`
	Conflict bool   `json:"conflict"`
	Feed     *Feed  `json:"feed,omitempty"`
}

type syncResponse struct {
	Cursor  string            `json:"cursor"`
	More    bool              `json:"more"`
	Reset   bool              `json:"reset"`
	Acks    []acknowledgement `json:"acks"`
	Changes []change          `json:"changes"`
}

func enqueue(tx *sql.Tx, kind, id string, base int64, value any) error {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return err
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = tx.Exec("INSERT INTO outbox(op,kind,entity,base,data) VALUES(?,?,?,?,?)", hex.EncodeToString(b[:]), kind, id, base, string(raw))
	return err
}

func enqueueFeed(tx *sql.Tx, f Feed) error {
	var revision int64
	if err := tx.QueryRow("SELECT revision FROM feeds WHERE id=?", f.ID).Scan(&revision); err != nil {
		return err
	}
	return enqueue(tx, "feed", f.ID, revision, Feed{ID: f.ID, URL: f.URL, Name: f.Name, Paused: f.Paused, Removed: f.Removed})
}

var remoteClient = &http.Client{
	Timeout:       15 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

func postJSON(ctx context.Context, origin, path, token string, value, dst any, maxBody int64) error {
	u, err := url.Parse(origin)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.ForceQuery {
		return errors.New("remote service requires a valid HTTPS URL")
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.JoinPath(path).String(), bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := remoteClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("remote service returned HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return err
	}
	if int64(len(data)) > maxBody {
		return errors.New("remote response too large")
	}
	return json.Unmarshal(data, dst)
}

func (e *Engine) Sync(operationID, syncURL, token string) error {
	if !e.account || syncURL == "" || token == "" {
		return errors.New("sign in to sync")
	}
	e.mu.Lock()
	if e.syncing {
		e.mu.Unlock()
		return ErrBusy
	}
	e.syncing = true
	e.mu.Unlock()
	defer func() { e.mu.Lock(); e.syncing = false; e.mu.Unlock() }()
	ctx, err := e.networkContext(operationID)
	if err != nil {
		return err
	}
	for range 30 {
		cursor, ops, err := e.pending(ctx)
		if err != nil {
			return err
		}
		var result syncResponse
		request := struct {
			Cursor     string      `json:"cursor"`
			Operations []operation `json:"operations"`
		}{cursor, ops}
		// A page may contain 50 changes and 50 feed acknowledgements. At six
		// bytes per escaped character, even all maximum-length fields fit in 2 MiB.
		if err := postJSON(ctx, syncURL, "/v1/sync", token, request, &result, 2<<20); err != nil {
			return err
		}
		if err := e.applySync(ctx, ops, result); err != nil {
			return err
		}
		if !result.More {
			var pending bool
			if err := e.db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM outbox)").Scan(&pending); err != nil {
				return err
			}
			if !pending {
				return nil
			}
		}
	}
	return errors.New("sync will continue on the next refresh")
}

func (e *Engine) pending(ctx context.Context) (string, []operation, error) {
	var cursor string
	if err := e.db.QueryRowContext(ctx, "SELECT cursor FROM preferences").Scan(&cursor); err != nil {
		return "", nil, err
	}
	rows, err := e.db.QueryContext(ctx, `SELECT op,kind,entity,base,data FROM outbox o
		WHERE kind!='feed' OR seq=(SELECT min(seq) FROM outbox WHERE kind='feed' AND entity=o.entity)
		ORDER BY seq LIMIT 50`)
	if err != nil {
		return "", nil, err
	}
	defer rows.Close()
	ops := []operation{}
	wireBytes := 0
	for rows.Next() {
		var op operation
		var raw string
		if err := rows.Scan(&op.ID, &op.Kind, &op.Entity, &op.Base, &raw); err != nil {
			return "", nil, err
		}
		// Escaped snippets and four-byte runes can fill a page before 50 rows.
		wireBytes += len(raw) + 256
		if wireBytes > 384<<10 && len(ops) > 0 {
			break
		}
		op.Data = json.RawMessage(raw)
		ops = append(ops, op)
	}
	return cursor, ops, rows.Err()
}

func (e *Engine) applySync(ctx context.Context, ops []operation, result syncResponse) error {
	if result.Cursor == "" || len(result.Cursor) > 32 || len(result.Acks) != len(ops) || len(result.Changes) > 50 {
		return errors.New("invalid sync page")
	}
	tx, err := e.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var priorCursor string
	if err := tx.QueryRow("SELECT cursor FROM preferences").Scan(&priorCursor); err != nil {
		return err
	}
	if result.Reset {
		if _, err := tx.Exec("UPDATE inbox SET snapshot_seen=0 WHERE received=0"); err != nil {
			return err
		}
	}
	snapshot := result.Reset || strings.HasPrefix(priorCursor, "s:")
	for i, ack := range result.Acks {
		op := ops[i]
		if ack.ID != op.ID {
			return errors.New("invalid sync acknowledgement")
		}
		if op.Kind == "feed" {
			if ack.Feed != nil && ack.Feed.ID != op.Entity {
				return errors.New("sync feed identity mismatch")
			}
			if ack.Conflict {
				f, err := scanFeed(tx.QueryRow("SELECT "+feedColumns+" FROM feeds WHERE id=?", op.Entity))
				if err != nil {
					return err
				}
				raw, err := json.Marshal(f)
				if err != nil {
					return err
				}
				if _, err := tx.Exec("INSERT OR REPLACE INTO feed_conflicts VALUES(?,?)", f.ID, raw); err != nil {
					return err
				}
				if _, err := tx.Exec("DELETE FROM outbox WHERE kind='feed' AND entity=?", f.ID); err != nil {
					return err
				}
				if ack.Feed == nil {
					if _, err := tx.Exec("DELETE FROM feeds WHERE id=?", f.ID); err != nil {
						return err
					}
				}
			} else {
				if ack.Feed == nil {
					return errors.New("missing feed revision")
				}
				if _, err := tx.Exec("UPDATE outbox SET base=? WHERE kind='feed' AND entity=? AND base=?", ack.Feed.Revision, op.Entity, op.Base); err != nil {
					return err
				}
			}
		}
		if _, err := tx.Exec("DELETE FROM outbox WHERE op=?", ack.ID); err != nil {
			return err
		}
		if ack.Feed != nil {
			if err := applyFeed(tx, *ack.Feed, snapshot); err != nil {
				return err
			}
		}
	}
	for _, c := range result.Changes {
		switch c.Kind {
		case "feed":
			var f Feed
			if err := json.Unmarshal(c.Data, &f); err != nil {
				return err
			}
			if err := applyFeed(tx, f, snapshot); err != nil {
				return err
			}
		case "item":
			var item Item
			if err := json.Unmarshal(c.Data, &item); err != nil {
				return err
			}
			if err := validateRemoteItem(&item); err != nil {
				return err
			}
			if err := putItem(ctx, tx, item, 0, 0, "", 0); err != nil {
				return err
			}
			if _, err := tx.Exec("UPDATE inbox SET snapshot_seen=1 WHERE id=? AND received=0", item.ID); err != nil {
				return err
			}
		default:
			return errors.New("unknown sync change type")
		}
	}
	if snapshot && !result.More {
		if _, err := tx.Exec(`DELETE FROM inbox WHERE received=0 AND snapshot_seen=0
			AND NOT EXISTS(SELECT 1 FROM outbox WHERE entity=inbox.id)`); err != nil {
			return err
		}
	}
	if _, err := tx.Exec("UPDATE preferences SET cursor=?", result.Cursor); err != nil {
		return err
	}
	if err := prune(ctx, tx, time.Now()); err != nil {
		return err
	}
	return tx.Commit()
}

func validateRemoteItem(i *Item) error {
	if len(i.ID) != 64 || len(i.FeedID) != 64 || i.Status < 0 || i.Status > 2 || i.Priority < 1 || i.Priority > 5 {
		return errors.New("invalid synced item")
	}
	i.Title = ingest.TruncateRunes(i.Title, 512)
	i.Snippet = ingest.TruncateRunes(i.Snippet, snippetLimit)
	i.URL = articleURL("https://invalid.invalid/", i.URL)
	return nil
}

func applyFeed(tx *sql.Tx, f Feed, snapshot bool) error {
	raw, err := canonical(f.URL)
	if err != nil || raw != f.URL || hash(raw) != f.ID || f.Revision <= 0 {
		return errors.New("invalid synced feed")
	}
	f.Name = ingest.TruncateRunes(f.Name, 128)
	var pending bool
	if err := tx.QueryRow("SELECT EXISTS(SELECT 1 FROM outbox WHERE kind='feed' AND entity=?)", f.ID).Scan(&pending); err != nil {
		return err
	}
	if pending {
		_, err = tx.Exec("UPDATE feeds SET revision=max(revision,?) WHERE id=?", f.Revision, f.ID)
		return err
	}
	_, err = tx.Exec(`INSERT INTO feeds(id,url,name,paused,removed,revision) VALUES(?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET name=excluded.name,paused=excluded.paused,removed=excluded.removed,
		revision=excluded.revision,
		generation=generation+(removed!=excluded.removed OR paused!=excluded.paused OR name!=excluded.name),
		refreshed=CASE WHEN removed!=excluded.removed THEN 0 ELSE refreshed END,
		etag=CASE WHEN removed!=excluded.removed THEN '' ELSE etag END,
		modified=CASE WHEN removed!=excluded.removed THEN '' ELSE modified END,
		assess=CASE WHEN excluded.removed THEN 0 ELSE assess END
		WHERE ? OR excluded.revision>=revision`, f.ID, f.URL, f.Name, f.Paused, f.Removed, f.Revision, snapshot)
	return err
}

func (e *Engine) conflicts(ctx context.Context) ([]Feed, error) {
	rows, err := e.db.QueryContext(ctx, "SELECT data FROM feed_conflicts ORDER BY id LIMIT 50")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []Feed{}
	for rows.Next() {
		var raw string
		var f Feed
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(raw), &f); err != nil {
			return nil, err
		}
		result = append(result, f)
	}
	return result, rows.Err()
}

func (e *Engine) DismissConflict(id string) error {
	_, err := e.db.Exec("DELETE FROM feed_conflicts WHERE id=?", id)
	return err
}

func (e *Engine) ImportLocal(dir string) error {
	if !e.account {
		return errors.New("sign in to import")
	}
	source, err := Open(dir, "")
	if err != nil {
		return err
	}
	defer source.Close()
	ctx := context.Background()
	subscriptions, err := feeds(ctx, source.db, "")
	if err != nil {
		return err
	}
	tx, err := e.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, f := range subscriptions {
		added, err := tx.Exec(`INSERT OR IGNORE INTO feeds(id,url,name,paused)
			SELECT ?,?,?,? WHERE (SELECT count(*) FROM feeds WHERE removed=0)<8`, f.ID, f.URL, f.Name, f.Paused)
		if err != nil {
			return err
		}
		n, err := added.RowsAffected()
		if err != nil {
			return err
		}
		if n > 0 {
			if err := enqueueFeed(tx, f); err != nil {
				return err
			}
		}
	}
	rows, err := source.db.QueryContext(ctx, "SELECT "+itemColumns+" FROM inbox WHERE feed_id!=? ORDER BY published DESC LIMIT 1000", hash("legacy"))
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var i Item
		if err := rows.Scan(&i.ID, &i.FeedID, &i.Title, &i.URL, &i.Snippet, &i.Published, &i.Status, &i.Priority); err != nil {
			return err
		}
		var exists bool
		if err := tx.QueryRow("SELECT EXISTS(SELECT 1 FROM inbox WHERE id=?)", i.ID).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			if err := putItem(ctx, tx, i, 0, 0, "", 0); err != nil {
				return err
			}
			if err := enqueue(tx, "item", i.ID, 0, i); err != nil {
				return err
			}
		} else if i.Status > 0 {
			res, err := tx.Exec("UPDATE inbox SET status=? WHERE id=? AND status<?", i.Status, i.ID, i.Status)
			if err != nil {
				return err
			}
			n, err := res.RowsAffected()
			if err != nil {
				return err
			}
			if n > 0 {
				if err := enqueue(tx, "status", i.ID, 0, map[string]int{"status": i.Status}); err != nil {
					return err
				}
			}
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if err := prune(ctx, tx, time.Now()); err != nil {
		return err
	}
	return tx.Commit()
}

// AccountKey uses the verified identity from the sync service, never an email.
func AccountKey(issuer, subject string) (string, error) {
	u, err := url.Parse(issuer)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil ||
		u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") || subject == "" || len(subject) > 256 || strings.ContainsRune(subject, 0) {
		return "", errors.New("invalid account identity")
	}
	return u.Scheme + "://" + u.Host + "\x00" + subject, nil
}

func ProfilePath(dir, profile string) string {
	return filepath.Join(dir, "phone-"+hash(profile)+".db")
}
