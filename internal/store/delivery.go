package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"time"
)

// ClaimDelivery prevents simultaneous sends from separate daemon or CLI processes.
// A completed receipt is retained until its item is pruned.
func (s *Store) ClaimDelivery(ctx context.Context, itemID int64, channel string) (string, bool, error) {
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return "", false, err
	}
	lease := hex.EncodeToString(token[:])
	now := time.Now().Unix()
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO notification_deliveries(item_id, channel, lease, lease_until, delivered)
		VALUES(?, ?, ?, ?, 0)
		ON CONFLICT(item_id, channel) DO UPDATE SET lease = excluded.lease, lease_until = excluded.lease_until
		WHERE delivered = 0 AND lease_until <= ?
	`, itemID, channel, lease, now+120, now)
	if err != nil {
		return "", false, err
	}
	count, err := res.RowsAffected()
	if err != nil {
		return "", false, err
	}
	if count > 0 {
		return lease, false, nil
	}
	var delivered bool
	if err := s.db.QueryRowContext(ctx, `SELECT delivered FROM notification_deliveries WHERE item_id = ? AND channel = ?`, itemID, channel).Scan(&delivered); err != nil {
		return "", false, err
	}
	if delivered {
		return "", true, nil
	}
	return "", false, errors.New("notification is already being delivered")
}

func (s *Store) FinishDelivery(ctx context.Context, itemID int64, channel, lease string, success bool) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE notification_deliveries SET delivered = ?, lease_until = 0
		WHERE item_id = ? AND channel = ? AND lease = ? AND delivered = 0 AND lease_until > 0
	`, success, itemID, channel, lease)
	if err != nil {
		return err
	}
	count, err := res.RowsAffected()
	if err == nil && count == 0 {
		return errors.New("notification delivery lease expired")
	}
	return err
}

// FinishInitialPush releases the digest hold only after the immediate attempt ends.
// A crashed sender's hold expires after two minutes without a repair-on-read write.
func (s *Store) FinishInitialPush(ctx context.Context, itemID int64, success bool) (bool, error) {
	status := "queued"
	if success {
		status = "pushed"
	}
	res, err := s.db.ExecContext(ctx, `UPDATE items SET status=?, initial_push_until=0
  WHERE id=? AND user_id=1 AND status='queued'`, status, itemID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}
