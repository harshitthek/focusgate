package phone

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"focusgate/internal/ingest"
)

// ImportLegacy copies a previous FocusGate APK's cache into an explicitly local profile.
// Server session tokens and ownership claims do not transfer into Neon.
func (e *Engine) ImportLegacy(path string) error {
	if e.account {
		return errors.New("legacy items belong to the local inbox")
	}
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	ctx := context.Background()
	absolute, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(absolute), RawQuery: "mode=ro"}
	if !strings.HasPrefix(u.Path, "/") {
		u.Path = "/" + u.Path
	}
	legacy, err := sql.Open("sqlite", u.String())
	if err != nil {
		return err
	}
	defer legacy.Close()
	tx, err := e.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var done bool
	if err := tx.QueryRow("SELECT legacy_imported FROM preferences").Scan(&done); err != nil || done {
		return err
	}
	rows, err := legacy.QueryContext(ctx, `SELECT source_type,external_id,title,body,url,
		COALESCE(unixepoch(published_at),unixepoch(created_at),0),status,priority
		FROM items WHERE user_id=1 ORDER BY created_at DESC LIMIT 1000`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var item Item
		var kind, external, status string
		if err := rows.Scan(&kind, &external, &item.Title, &item.Snippet, &item.URL, &item.Published, &status, &item.Priority); err != nil {
			return err
		}
		item.ID = hash("legacy\x00" + kind + "\x00" + external)
		item.FeedID = hash("legacy")
		item.Title = ingest.TruncateRunes(item.Title, 512)
		item.Snippet = ingest.TruncateRunes(item.Snippet, snippetLimit)
		if strings.HasPrefix(item.URL, "https://") || strings.HasPrefix(item.URL, "http://") {
			item.URL = articleURL("https://invalid.invalid/", item.URL)
		} else {
			item.URL = ""
		}
		item.Priority = min(max(item.Priority, 1), 5)
		if status == "archived" {
			item.Status = 2
		} else if status == "read" {
			item.Status = 1
		}
		if item.Published == 0 {
			item.Published = time.Now().Unix()
		}
		if err := putItem(ctx, tx, item, 0, 0, "", 0); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if _, err := tx.Exec("UPDATE preferences SET legacy_imported=1"); err != nil {
		return err
	}
	return tx.Commit()
}
