package store

import (
	"context"
	"testing"
	"time"
)

func TestAssessmentChargesTheDayAfterWriteLock(t *testing.T) {
	st, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	midnight := time.Date(2026, 11, 28, 0, 0, 0, 0, time.UTC)
	clock := func() time.Time {
		if st.db.Stats().InUse == 0 {
			return midnight.Add(-time.Second)
		}
		return midnight
	}
	_, lease, err := st.ReserveAssessment(ctx, "account", "first", "hash1", "scope", clock, 2, 1, false)
	if err != nil || lease == "" {
		t.Fatalf("first reservation failed: %v", err)
	}
	_, lease, err = st.ReserveAssessment(ctx, "account", "second", "hash2", "scope",
		func() time.Time { return midnight }, 2, 1, false)
	if err != nil || lease != "" {
		t.Fatalf("new-day quota failed to count the first reservation: lease present %t, error %v", lease != "", err)
	}
}
