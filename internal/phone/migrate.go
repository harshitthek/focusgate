package phone

import (
	"database/sql"
	"net/url"
)

// migratePhoneURLs discards legacy feed edits that the server cannot apply,
// and clears invalid optional links without dropping their queued items.
func migratePhoneURLs(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var version int
	if err := tx.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version >= 1 {
		return err
	}

	repairs := []struct {
		selectURLs string
		update     []string
	}{
		{
			selectURLs: "SELECT id,url FROM feeds",
			update: []string{
				"DELETE FROM outbox WHERE kind='feed' AND entity=?",
				"DELETE FROM feed_conflicts WHERE id=?",
				"DELETE FROM feeds WHERE id=?",
			},
		},
		{
			selectURLs: "SELECT op,json_extract(data,'$.url') FROM outbox WHERE kind='item'",
			update:     []string{"UPDATE outbox SET data=json_set(data,'$.url','') WHERE op=?"},
		},
		{
			selectURLs: "SELECT id,url FROM inbox WHERE url<>''",
			update:     []string{"UPDATE inbox SET url='' WHERE id=?"},
		},
	}
	for _, repair := range repairs {
		rows, err := tx.Query(repair.selectURLs)
		if err != nil {
			return err
		}
		invalid, err := invalidPortIDs(rows)
		if err != nil {
			return err
		}
		for _, id := range invalid {
			for _, query := range repair.update {
				if _, err := tx.Exec(query, id); err != nil {
					return err
				}
			}
		}
	}
	// Earlier builds did not record feed membership on snapshot pages.
	if _, err := tx.Exec("UPDATE preferences SET cursor='0' WHERE cursor LIKE 's:%'"); err != nil {
		return err
	}
	if _, err := tx.Exec("PRAGMA user_version=1"); err != nil {
		return err
	}
	return tx.Commit()
}

func invalidPortIDs(rows *sql.Rows) ([]string, error) {
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		var raw sql.NullString
		if err := rows.Scan(&id, &raw); err != nil {
			return nil, err
		}
		if parsed, err := url.Parse(raw.String); err == nil && !validHTTPPort(parsed) {
			ids = append(ids, id)
		}
	}
	return ids, rows.Err()
}
