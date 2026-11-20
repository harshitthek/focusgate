package store

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
)

func sqliteName(name string) string { return `"` + strings.ReplaceAll(name, `"`, `""`) + `"` }

func itemsNeedRebuild(ctx context.Context, tx *sql.Tx) (bool, error) {
	rows, err := tx.QueryContext(ctx, `PRAGMA index_list(items)`)
	if err != nil {
		return false, err
	}
	var names []string
	for rows.Next() {
		var seq, unique, partial int
		var name, origin string
		if err := rows.Scan(&seq, &name, &unique, &origin, &partial); err != nil {
			rows.Close()
			return false, err
		}
		if unique != 0 && partial == 0 {
			names = append(names, name)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return false, err
	}
	hasTenantKey, hasLegacyKey := false, false
	for _, name := range names {
		info, err := tx.QueryContext(ctx, `PRAGMA index_info(`+sqliteName(name)+`)`)
		if err != nil {
			return false, err
		}
		var columns []string
		for info.Next() {
			var seq, cid int
			var column sql.NullString
			if err := info.Scan(&seq, &cid, &column); err != nil {
				info.Close()
				return false, err
			}
			columns = append(columns, strings.ToLower(column.String))
		}
		err = info.Err()
		info.Close()
		if err != nil {
			return false, err
		}
		sort.Strings(columns)
		joined := strings.Join(columns, ",")
		hasTenantKey = hasTenantKey || joined == "external_id,source_type,user_id"
		hasLegacyKey = hasLegacyKey || joined == "external_id,source_type"
	}
	return !hasTenantKey || hasLegacyKey, nil
}

type itemDependent struct {
	name, ddl, backup string
	indexes           []string
}

// Remove known child tables before replacing their parent, preserving their
// contents in SQLite's file-backed TEMP store instead of invoking ON DELETE CASCADE.
func backupItemDependents(ctx context.Context, tx *sql.Tx) ([]itemDependent, error) {
	var saved []itemDependent
	for _, name := range []string{"notification_deliveries", "mobile_item_links"} {
		entry := itemDependent{name: name, backup: "focusgate_migration_" + name}
		err := tx.QueryRowContext(ctx, `SELECT sql FROM sqlite_master WHERE type='table' AND name=?`, name).Scan(&entry.ddl)
		if err == sql.ErrNoRows {
			continue
		}
		if err != nil {
			return nil, err
		}
		indexes, err := tx.QueryContext(ctx, `SELECT sql FROM sqlite_master WHERE type='index' AND tbl_name=? AND sql IS NOT NULL`, name)
		if err != nil {
			return nil, err
		}
		for indexes.Next() {
			var ddl string
			if err := indexes.Scan(&ddl); err != nil {
				indexes.Close()
				return nil, err
			}
			entry.indexes = append(entry.indexes, ddl)
		}
		err = indexes.Err()
		indexes.Close()
		if err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `CREATE TEMP TABLE `+sqliteName(entry.backup)+` AS SELECT * FROM `+sqliteName(name)); err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `DROP TABLE `+sqliteName(name)); err != nil {
			return nil, err
		}
		saved = append(saved, entry)
	}
	return saved, nil
}

func restoreItemDependents(ctx context.Context, tx *sql.Tx, saved []itemDependent) error {
	for _, entry := range saved {
		statements := []string{entry.ddl, fmt.Sprintf(`INSERT INTO %s SELECT * FROM temp.%s`, sqliteName(entry.name), sqliteName(entry.backup))}
		statements = append(statements, entry.indexes...)
		statements = append(statements, `DROP TABLE temp.`+sqliteName(entry.backup))
		for _, statement := range statements {
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				return err
			}
		}
	}
	return nil
}
