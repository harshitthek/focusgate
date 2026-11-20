package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"time"
)

var ErrConnectionChanged = errors.New("connection changed; restart authorization")

// BeginConnection invalidates results from earlier attempts, including device polls.
func (s *Store) BeginConnection(ctx context.Context, kind string) (string, error) {
	if kind != "github" && kind != "email" {
		return "", errors.New("unsupported connection kind")
	}
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return "", err
	}
	generation := hex.EncodeToString(token[:])
	return generation, s.SetSetting(ctx, kind+"_generation", generation)
}

func invalidateConnection(ctx context.Context, tx *sql.Tx, kind string) error {
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO settings(key,value,updated_at) VALUES(?,?,?)
	 ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at`,
		kind+"_generation", hex.EncodeToString(token[:]), time.Now().UTC())
	return err
}

func checkConnection(ctx context.Context, tx *sql.Tx, kind, generation string) error {
	// A conditional write acquires the write lock before checking the generation.
	res, err := tx.ExecContext(ctx, `UPDATE settings SET value=value WHERE key=? AND value=?`, kind+"_generation", generation)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err == nil && n == 0 {
		return ErrConnectionChanged
	}
	return err
}
