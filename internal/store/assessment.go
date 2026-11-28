package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

const assessmentSchema = `CREATE TABLE IF NOT EXISTS mobile_assessments(
    account TEXT NOT NULL, request TEXT NOT NULL, fingerprint TEXT NOT NULL,
    day TEXT NOT NULL, result TEXT NOT NULL DEFAULT '', cache_scope TEXT NOT NULL DEFAULT '',
    lease TEXT NOT NULL DEFAULT '', lease_until INTEGER NOT NULL DEFAULT 0, PRIMARY KEY(account,request)
)`

const assessmentUsageSchema = `CREATE TABLE IF NOT EXISTS mobile_assessment_usage(
    day TEXT NOT NULL, account TEXT NOT NULL, attempts INTEGER NOT NULL, PRIMARY KEY(day,account)
)`

var ErrAssessmentChanged = errors.New("assessment request content changed")

func migrateAssessment(ctx context.Context, tx *sql.Tx) error {
	if columnExistsTx(ctx, tx, "mobile_assessments", "cache_scope") {
		return nil
	}
	for _, stmt := range []string{
		"ALTER TABLE mobile_assessments ADD COLUMN cache_scope TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE mobile_assessments ADD COLUMN lease TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE mobile_assessments ADD COLUMN lease_until INTEGER NOT NULL DEFAULT 0",
		`INSERT INTO mobile_assessment_usage(day,account,attempts)
         SELECT day,account,count(*) FROM mobile_assessments GROUP BY day,account`,
	} {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("migrate mobile assessments: %w", err)
		}
	}
	return nil
}

// ReserveAssessment returns cached output or a lease for one charged provider attempt.
func (s *Store) ReserveAssessment(ctx context.Context, account, id, fingerprint, scope string, now time.Time, global, perAccount int, usesJev bool) (string, string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", "", err
	}
	defer tx.Rollback()
	now = now.UTC()
	day := now.Format("20060102")
	cutoff := now.AddDate(0, 0, -2).Format("20060102")
	if _, err := tx.ExecContext(ctx, "DELETE FROM mobile_assessments WHERE day<? AND lease_until<=?", cutoff, now.Unix()); err != nil {
		return "", "", err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM mobile_assessment_usage WHERE day<?", cutoff); err != nil {
		return "", "", err
	}
	var old, result, oldScope string
	var leaseUntil int64
	err = tx.QueryRowContext(ctx, `SELECT fingerprint,result,cache_scope,lease_until
        FROM mobile_assessments WHERE account=? AND request=?`, account, id).Scan(&old, &result, &oldScope, &leaseUntil)
	switch {
	case err == nil:
		if old != fingerprint {
			return "", "", ErrAssessmentChanged
		}
		if oldScope == scope && result != "" {
			return result, "", tx.Commit()
		}
		if leaseUntil > now.Unix() {
			return "", "", errors.New("assessment already in progress")
		}
	case !errors.Is(err, sql.ErrNoRows):
		return "", "", err
	}

	var globalUsed, userUsed int
	if err := tx.QueryRowContext(ctx, `SELECT coalesce(sum(attempts),0),
        coalesce(sum(CASE WHEN account=? THEN attempts END),0) FROM mobile_assessment_usage WHERE day=?`,
		account, day).Scan(&globalUsed, &userUsed); err != nil {
		return "", "", err
	}
	if globalUsed >= global || userUsed >= perAccount {
		return "", "", tx.Commit()
	}
	if usesJev {
		ok, err := reserveProviderUsage(ctx, tx, now.In(time.Local).Format("20060102"), now, global)
		if err != nil || !ok {
			return "", "", err
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO mobile_assessment_usage(day,account,attempts) VALUES(?,?,1)
        ON CONFLICT(day,account) DO UPDATE SET attempts=attempts+1`, day, account)
	if err != nil {
		return "", "", err
	}
	// The handler limits the provider call to three seconds. The lease also
	// allows retry after process termination, while stale completions are fenced.
	lease := rand.Text()
	_, err = tx.ExecContext(ctx, `INSERT INTO mobile_assessments(account,request,fingerprint,day,cache_scope,lease,lease_until)
        VALUES(?,?,?,?,?,?,?)
        ON CONFLICT(account,request) DO UPDATE SET day=excluded.day,result='',cache_scope=excluded.cache_scope,
        lease=excluded.lease,lease_until=excluded.lease_until`, account, id, fingerprint, day, scope, lease, now.Add(15*time.Second).Unix())
	if err != nil {
		return "", "", err
	}
	return "", lease, tx.Commit()
}

// FinishAssessment saves the result, or clears a failed lease for retry.
func (s *Store) FinishAssessment(ctx context.Context, account, id, lease, result string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE mobile_assessments SET result=?,lease='',lease_until=0
        WHERE account=? AND request=? AND lease=?`, result, account, id, lease)
	if err != nil {
		return err
	}
	updated, err := res.RowsAffected()
	if err == nil && updated != 1 {
		return errors.New("assessment lease changed")
	}
	return err
}
