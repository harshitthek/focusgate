package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

const assessmentSchema = `CREATE TABLE IF NOT EXISTS mobile_assessments(
	account TEXT NOT NULL, request TEXT NOT NULL, fingerprint TEXT NOT NULL,
	day TEXT NOT NULL, result TEXT NOT NULL DEFAULT '', PRIMARY KEY(account,request)
)`

var ErrAssessmentChanged = errors.New("assessment request content changed")

// ReserveAssessment checks deduplication and reserves both daily quotas in the same transaction.
func (s *Store) ReserveAssessment(ctx context.Context, account, id, fingerprint string, now time.Time, global, perAccount int, usesJev bool) (string, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", false, err
	}
	defer tx.Rollback()
	day := now.UTC().Format("20060102")
	if _, err := tx.ExecContext(ctx, "DELETE FROM mobile_assessments WHERE day<?", now.AddDate(0, 0, -2).Format("20060102")); err != nil {
		return "", false, err
	}
	var old, result string
	err = tx.QueryRowContext(ctx, "SELECT fingerprint,result FROM mobile_assessments WHERE account=? AND request=?", account, id).Scan(&old, &result)
	if err == nil {
		if old != fingerprint {
			return "", false, ErrAssessmentChanged
		}
		return result, false, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", false, err
	}
	var globalUsed, userUsed int
	if err := tx.QueryRowContext(ctx, "SELECT count(*),count(CASE WHEN account=? THEN 1 END) FROM mobile_assessments WHERE day=?", account, day).Scan(&globalUsed, &userUsed); err != nil {
		return "", false, err
	}
	if globalUsed >= global || userUsed >= perAccount {
		return "", false, tx.Commit()
	}
	if usesJev {
		ok, err := reserveProviderUsage(ctx, tx, now.In(time.Local).Format("20060102"), now.UTC(), global)
		if err != nil || !ok {
			return "", false, err
		}
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO mobile_assessments(account,request,fingerprint,day) VALUES(?,?,?,?)", account, id, fingerprint, day)
	if err != nil {
		return "", false, err
	}
	return "", true, tx.Commit()
}

func (s *Store) FinishAssessment(ctx context.Context, account, id, result string) error {
	_, err := s.db.ExecContext(ctx, "UPDATE mobile_assessments SET result=? WHERE account=? AND request=?", result, account, id)
	return err
}
