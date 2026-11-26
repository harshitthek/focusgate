package phone

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"

	"focusgate/internal/classify"
	"focusgate/internal/config"
	"focusgate/internal/ingest"
	"focusgate/internal/store"
)

const maxFeeds = 8
const snippetLimit = 900

var ErrOffline = errors.New("offline mode is enabled")
var ErrBusy = errors.New("refresh is already running")

type Feed struct {
	ID             string `json:"id"`
	URL            string `json:"url"`
	Name           string `json:"name"`
	Paused         bool   `json:"paused"`
	Assess         bool   `json:"assess"`
	Revision       int64  `json:"revision"`
	Refreshed      int64  `json:"refreshed"`
	Removed        bool   `json:"removed,omitempty"`
	etag, modified string
}

type Item struct {
	ID        string `json:"id"`
	FeedID    string `json:"feedId"`
	Title     string `json:"title"`
	URL       string `json:"url"`
	Snippet   string `json:"snippet"`
	Published int64  `json:"published"`
	Status    int    `json:"status"` // 0 unread, 1 read, 2 archived
	Priority  int    `json:"priority"`
}

type Settings struct {
	Offline bool             `json:"offline"`
	Alerts  bool             `json:"alerts"`
	Mode    config.FocusMode `json:"mode"`
}

type State struct {
	Settings  Settings `json:"settings"`
	Feeds     []Feed   `json:"feeds"`
	Items     []Item   `json:"items"`
	Conflicts []Feed   `json:"conflicts"`
}

type Engine struct {
	db         *sql.DB
	account    bool
	router     *classify.Router
	rules      []config.RuleConfig
	fetch      func(context.Context, Feed) ([]ingest.IncomingItem, ingest.FeedCache, bool, error)
	mu         sync.Mutex
	network    context.Context
	networkID  string
	cancel     context.CancelFunc
	refreshing bool
	syncing    bool
}

func Open(dir, profile string) (*Engine, error) {
	if dir == "" {
		return nil, errors.New("missing private storage directory")
	}
	db, err := store.OpenSQLite(ProfilePath(dir, profile))
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	cfg := config.Default()
	cfg.Rules = []config.RuleConfig{{
		Name: "Security or outage advisory", SourceType: "rss",
		Regex:  "(?i)^(?:security advisory[: ]|security alert[: ]|sev[01][: ]|outage[: ]|production (?:down|outage)[: ]|CVE-[0-9]{4}-[0-9]+\\b)",
		Action: "push_now", Priority: 5,
	}}
	if err := cfg.NormalizeAndValidate(); err != nil {
		db.Close()
		return nil, err
	}
	e := &Engine{db: db, account: profile != "", router: classify.NewRouter(cfg), rules: cfg.Rules, fetch: func(ctx context.Context, f Feed) ([]ingest.IncomingItem, ingest.FeedCache, bool, error) {
		return ingest.FetchFeedRestricted(ctx, nil, f.ID, f.URL, "", ingest.FeedCache{ETag: f.etag, LastModified: f.modified}, snippetLimit)
	}}
	return e, nil
}

func (e *Engine) Close() error {
	e.Suspend()
	return e.db.Close()
}

func (e *Engine) Suspend() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.cancel != nil {
		e.cancel()
		e.cancel = nil
	}
	e.networkID = ""
}

func (e *Engine) BeginNetwork(id string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if id == "" || e.networkID != "" {
		return ErrBusy
	}
	s, err := settings(context.Background(), e.db)
	if err != nil {
		return err
	}
	if s.Offline {
		return ErrOffline
	}
	e.network, e.cancel = context.WithCancel(context.Background())
	e.networkID = id
	return nil
}

func (e *Engine) EndNetwork(id string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if id != "" && e.networkID == id {
		e.cancel()
		e.cancel = nil
		e.networkID = ""
	}
}

func (e *Engine) networkContext(id string) (context.Context, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if id == "" || e.networkID != id {
		return nil, context.Canceled
	}
	return e.network, nil
}

func hash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func canonical(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil || len(raw) > 2048 || u.Hostname() == "" || u.User != nil || u.Opaque != "" {
		return "", errors.New("enter a valid feed URL")
	}
	u.Scheme = strings.ToLower(u.Scheme)
	if u.Scheme != "https" && u.Scheme != "http" {
		return "", errors.New("feeds need an HTTP or HTTPS URL")
	}
	u.Host = strings.ToLower(u.Host)
	if u.Scheme == "https" && u.Port() == "443" || u.Scheme == "http" && u.Port() == "80" {
		u.Host = strings.TrimSuffix(u.Host, ":"+u.Port())
	}
	u.Fragment = ""
	if u.Path == "" {
		u.Path = "/"
	}
	return u.String(), nil
}

func (e *Engine) SetSettings(s Settings) error {
	if !config.ValidFocusMode(s.Mode) {
		return errors.New("invalid focus mode")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if s.Offline && e.cancel != nil {
		e.cancel()
		e.cancel = nil
		e.networkID = ""
	}
	_, err := e.db.Exec(`UPDATE preferences SET offline=?,alerts=?,mode=?,
		last_digest=CASE WHEN alerts=0 AND ? THEN unixepoch() ELSE last_digest END`, s.Offline, s.Alerts, s.Mode, s.Alerts)
	return err
}

type queryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func settings(ctx context.Context, q queryer) (Settings, error) {
	var s Settings
	err := q.QueryRowContext(ctx, "SELECT offline,alerts,mode FROM preferences").Scan(&s.Offline, &s.Alerts, &s.Mode)
	return s, err
}

func (e *Engine) Settings() (Settings, error) {
	return settings(context.Background(), e.db)
}

const feedColumns = "id,url,name,paused,assess,revision,refreshed,removed,etag,modified"
const itemColumns = "id,feed_id,title,url,snippet,published,status,priority"

type scanner interface{ Scan(...any) error }

func scanFeed(row scanner) (Feed, error) {
	var f Feed
	err := row.Scan(&f.ID, &f.URL, &f.Name, &f.Paused, &f.Assess, &f.Revision, &f.Refreshed, &f.Removed, &f.etag, &f.modified)
	return f, err
}

func feeds(ctx context.Context, q queryer, where string) ([]Feed, error) {
	rows, err := q.QueryContext(ctx, "SELECT "+feedColumns+" FROM feeds WHERE removed=0 "+where+" ORDER BY name,id LIMIT 8")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []Feed{}
	for rows.Next() {
		f, err := scanFeed(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, f)
	}
	return result, rows.Err()
}

func (e *Engine) State(archive bool, offset int) (State, error) {
	ctx := context.Background()
	var s State
	var err error
	if s.Settings, err = settings(ctx, e.db); err != nil {
		return s, err
	}
	if s.Feeds, err = feeds(ctx, e.db, ""); err != nil {
		return s, err
	}
	if s.Conflicts, err = e.conflicts(ctx); err != nil {
		return s, err
	}
	s.Items, err = e.listItems("(status=2)=?", archive, offset)
	return s, err
}

func (e *Engine) Digest(offset int) ([]Item, error) {
	return e.listItems("status=0 AND digest=? AND feed_id IN (SELECT id FROM feeds WHERE removed=0 AND paused=0)", 1, offset)
}

func (e *Engine) listItems(where string, filter any, offset int) ([]Item, error) {
	rows, err := e.db.Query("SELECT "+itemColumns+" FROM inbox WHERE "+where+" ORDER BY priority DESC,published DESC,id LIMIT 100 OFFSET ?", filter, max(offset, 0))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []Item{}
	for rows.Next() {
		var i Item
		if err = rows.Scan(&i.ID, &i.FeedID, &i.Title, &i.URL, &i.Snippet, &i.Published, &i.Status, &i.Priority); err != nil {
			return nil, err
		}
		items = append(items, i)
	}
	return items, rows.Err()
}

func (e *Engine) SaveFeed(f Feed) error {
	raw, err := canonical(f.URL)
	if err != nil {
		return err
	}
	f.URL = raw
	id := hash(raw)
	if f.ID != "" && f.ID != id {
		return errors.New("remove and re-add a feed to change its URL")
	}
	f.ID = id
	f.Name = ingest.TruncateRunes(strings.TrimSpace(f.Name), 128)
	if f.Name == "" {
		f.Name = ingest.TruncateRunes(raw, 128)
	}
	ctx := context.Background()
	tx, err := e.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	previous, err := scanFeed(tx.QueryRow("SELECT "+feedColumns+" FROM feeds WHERE id=?", id))
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	existing := err == nil && !previous.Removed
	if !f.Removed && !existing {
		var count int
		if err := tx.QueryRow("SELECT count(*) FROM feeds WHERE removed=0").Scan(&count); err != nil {
			return err
		}
		if count >= maxFeeds {
			return errors.New("you can add up to eight feeds")
		}
	}
	_, err = tx.Exec(`INSERT INTO feeds(id,url,name,paused,assess,removed) VALUES(?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET name=excluded.name, paused=excluded.paused, assess=excluded.assess, removed=excluded.removed,
		generation=generation+1, refreshed=CASE WHEN removed=1 THEN 0 ELSE refreshed END,
		etag=CASE WHEN removed=1 THEN '' ELSE etag END, modified=CASE WHEN removed=1 THEN '' ELSE modified END`, id, raw, f.Name, f.Paused, f.Assess && !f.Removed, f.Removed)
	if err != nil {
		return err
	}
	if e.account && (previous.ID == "" || previous.Name != f.Name || previous.Paused != f.Paused || previous.Removed != f.Removed) {
		if err := enqueueFeed(tx, f); err != nil {
			return err
		}
	}
	if _, err := tx.Exec("DELETE FROM feed_conflicts WHERE id=?", id); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if previous.Assess && (!f.Assess || f.Removed || f.Paused) {
		e.Suspend()
	}
	return nil
}

func (e *Engine) Mark(id string, status int) error {
	if status != 1 && status != 2 {
		return errors.New("invalid inbox status")
	}
	tx, err := e.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.Exec("UPDATE inbox SET status=? WHERE id=? AND status<?", status, id, status)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if e.account && n != 0 {
		if err := enqueue(tx, "status", id, 0, struct {
			Status int `json:"status"`
		}{status}); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func Decode(raw string, dst any) error {
	if len(raw) > 2<<20 {
		return fmt.Errorf("request too large")
	}
	return json.Unmarshal([]byte(raw), dst)
}
