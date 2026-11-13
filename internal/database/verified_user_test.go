package database

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func verifiedRow(t *testing.T, db *DB, userID int64) (chatID int64, source string, verifiedAt int64) {
	t.Helper()
	require.NoError(t, db.QueryRow(
		"SELECT chat_id, source, verified_at FROM verified_users WHERE user_id = ?", userID,
	).Scan(&chatID, &source, &verifiedAt))
	return chatID, source, verifiedAt
}

func TestIsUserVerified_FalseUntilMarked(t *testing.T) {
	db := newTestDB(t)

	verified, err := db.IsUserVerified(42)
	require.NoError(t, err)
	assert.False(t, verified)

	require.NoError(t, db.MarkUserVerified(42, -100, VerifiedSourceCaptcha))

	verified, err = db.IsUserVerified(42)
	require.NoError(t, err)
	assert.True(t, verified)

	other, err := db.IsUserVerified(43)
	require.NoError(t, err)
	assert.False(t, other, "the pass belongs to one user only")
}

func TestMarkUserVerified_KeepsFirstRecord(t *testing.T) {
	db := newTestDB(t)

	require.NoError(t, db.MarkUserVerified(42, -100, VerifiedSourceCaptcha))
	require.NoError(t, db.MarkUserVerified(42, -200, VerifiedSourceMiniApp))

	chatID, source, verifiedAt := verifiedRow(t, db, 42)
	assert.Equal(t, int64(-100), chatID)
	assert.Equal(t, VerifiedSourceCaptcha, source)
	assert.Positive(t, verifiedAt)
}

// recreatePassList drops the pass list and migrates again, which is what an
// existing deployment goes through the first time it runs this version.
func recreatePassList(t *testing.T, db *DB) {
	t.Helper()
	_, err := db.Exec("DROP TABLE verified_users")
	require.NoError(t, err)
	require.NoError(t, db.Migrate(""))
}

func TestMigrate_SeedsPassListFromSelfSolvedPasses(t *testing.T) {
	db := newTestDB(t)

	const (
		selfSolved    int64 = 1
		adminApproved int64 = 2
		testRun       int64 = 3
		failed        int64 = 4
		expired       int64 = 5
		solvedTwice   int64 = 6
		adminThenSelf int64 = 7
	)
	admin := sql.NullInt64{Int64: 900, Valid: true}
	events := []VerificationEvent{
		{ChatID: -100, UserID: selfSolved, Outcome: "passed", OccurredAt: 1000},
		{ChatID: -100, UserID: adminApproved, Outcome: "passed", AdminUserID: admin, OccurredAt: 1000},
		{ChatID: testRun, UserID: testRun, Outcome: "passed", IsTest: true, OccurredAt: 1000},
		{ChatID: -100, UserID: failed, Outcome: "failed", OccurredAt: 1000},
		{ChatID: -100, UserID: expired, Outcome: "expired", OccurredAt: 1000},
		{ChatID: -200, UserID: solvedTwice, Outcome: "passed", OccurredAt: 3000},
		{ChatID: -100, UserID: solvedTwice, Outcome: "passed", OccurredAt: 2000},
		{ChatID: -100, UserID: adminThenSelf, Outcome: "passed", AdminUserID: admin, OccurredAt: 1000},
		{ChatID: -200, UserID: adminThenSelf, Outcome: "passed", OccurredAt: 4000},
	}
	for _, e := range events {
		require.NoError(t, db.RecordVerificationEvent(e))
	}

	recreatePassList(t, db)

	want := map[int64]bool{
		selfSolved:    true,
		adminApproved: false,
		testRun:       false,
		failed:        false,
		expired:       false,
		solvedTwice:   true,
		adminThenSelf: true,
	}
	for userID, expected := range want {
		verified, err := db.IsUserVerified(userID)
		require.NoError(t, err)
		assert.Equal(t, expected, verified, "user %d", userID)
	}

	chatID, source, verifiedAt := verifiedRow(t, db, solvedTwice)
	assert.Equal(t, int64(-100), chatID, "the earliest pass is the one recorded")
	assert.Equal(t, VerifiedSourceCaptcha, source)
	assert.Equal(t, int64(2000), verifiedAt)

	chatID, _, verifiedAt = verifiedRow(t, db, adminThenSelf)
	assert.Equal(t, int64(-200), chatID, "the admin approval is not what qualified them")
	assert.Equal(t, int64(4000), verifiedAt)
}

func TestMigrate_SeedsPassListOnlyWhenCreatingIt(t *testing.T) {
	db := newTestDB(t)
	require.NoError(t, db.RecordVerificationEvent(VerificationEvent{
		ChatID: -100, UserID: 1, Outcome: "passed", OccurredAt: 1000,
	}))
	recreatePassList(t, db)

	_, err := db.Exec("DELETE FROM verified_users WHERE user_id = 1")
	require.NoError(t, err)
	require.NoError(t, db.Migrate(""))

	verified, err := db.IsUserVerified(1)
	require.NoError(t, err)
	assert.False(t, verified, "a later migration must not bring back a removed entry")
}
