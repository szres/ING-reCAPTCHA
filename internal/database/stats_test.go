package database

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func recordEvent(t *testing.T, db *DB, e VerificationEvent) {
	t.Helper()
	require.NoError(t, db.RecordVerificationEvent(e))
}

func msVal(ms int64) sql.NullInt64 { return sql.NullInt64{Int64: ms, Valid: true} }
func stepVal(s int64) sql.NullInt64 { return sql.NullInt64{Int64: s, Valid: true} }

func TestGetDailyStats_Empty(t *testing.T) {
	db := newTestDB(t)
	stats, err := db.GetDailyStats(0, 1_000_000_000_000)
	require.NoError(t, err)
	assert.Equal(t, 0, stats.Total)
	assert.Equal(t, 0, stats.Passed)
	assert.Equal(t, 0.0, stats.PassRate)
	assert.Equal(t, 0, stats.UniqueUsers)
}

func TestGetDailyStats_Aggregates(t *testing.T) {
	db := newTestDB(t)
	const start, end = int64(1_000_000), int64(2_000_000)
	mid := int64(1_500_000)

	// Inside window: 3 passed (5s, 10s, 15s), 1 failed at step 2, 1 expired
	recordEvent(t, db, VerificationEvent{ChatID: 1, UserID: 100, Outcome: "passed", CompletionTimeMs: msVal(5_000), UserLang: "en", OccurredAt: mid})
	recordEvent(t, db, VerificationEvent{ChatID: 1, UserID: 101, Outcome: "passed", CompletionTimeMs: msVal(10_000), UserLang: "zh", OccurredAt: mid + 10})
	recordEvent(t, db, VerificationEvent{ChatID: 1, UserID: 102, Outcome: "passed", CompletionTimeMs: msVal(15_000), UserLang: "zh", OccurredAt: mid + 20})
	recordEvent(t, db, VerificationEvent{ChatID: 1, UserID: 103, Outcome: "failed", FailedAtStep: stepVal(2), UserLang: "en", OccurredAt: mid + 30})
	recordEvent(t, db, VerificationEvent{ChatID: 1, UserID: 104, Outcome: "expired", UserLang: "zh", OccurredAt: mid + 40})

	// Outside window (older): should be ignored
	recordEvent(t, db, VerificationEvent{ChatID: 1, UserID: 999, Outcome: "passed", CompletionTimeMs: msVal(99_000), UserLang: "en", OccurredAt: start - 1})
	// Test event inside window: should be excluded from stats
	recordEvent(t, db, VerificationEvent{ChatID: 1, UserID: 555, Outcome: "passed", CompletionTimeMs: msVal(1_000), IsTest: true, UserLang: "en", OccurredAt: mid})

	stats, err := db.GetDailyStats(start, end)
	require.NoError(t, err)

	assert.Equal(t, 5, stats.Total)
	assert.Equal(t, 3, stats.Passed)
	assert.Equal(t, 1, stats.Failed)
	assert.Equal(t, 1, stats.Expired)
	assert.InDelta(t, 0.6, stats.PassRate, 0.0001) // 3/5
	assert.Equal(t, 5, stats.UniqueUsers)
	assert.Equal(t, int64(10_000), stats.AvgPassMs)
	assert.Equal(t, int64(5_000), stats.FastestPassMs)
	assert.Equal(t, int64(10_000), stats.MedianPassMs) // sorted [5,10,15] → idx round(0.5*2)=1 → 10
	assert.Equal(t, int64(15_000), stats.P95PassMs)    // idx round(0.95*2)=2 → 15
	assert.Equal(t, 1, stats.FailStepHist[2])
	assert.Equal(t, 3, stats.LangSplit["zh"]) // 2 passed + 1 expired
	assert.Equal(t, 2, stats.LangSplit["en"]) // 1 passed + 1 failed (test event excluded)
}

func TestGetDailyStats_ExcludesTestEventsByDefault(t *testing.T) {
	db := newTestDB(t)
	const start, end = int64(1_000_000), int64(2_000_000)
	mid := int64(1_500_000)

	recordEvent(t, db, VerificationEvent{ChatID: 1, UserID: 1, Outcome: "passed", CompletionTimeMs: msVal(5_000), IsTest: true, OccurredAt: mid})
	recordEvent(t, db, VerificationEvent{ChatID: 1, UserID: 2, Outcome: "failed", IsTest: true, OccurredAt: mid})

	stats, err := db.GetDailyStats(start, end)
	require.NoError(t, err)
	assert.Equal(t, 0, stats.Total)
}
