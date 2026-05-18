package database

import (
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- CreatePendingVerification / GetPendingVerification ---

func TestCreatePendingVerification_CreateAndRetrieve(t *testing.T) {
	db := newTestDB(t)
	expiresAt := time.Now().Add(5 * time.Minute).Truncate(time.Second)

	err := db.CreatePendingVerification(100, 200, []string{"cat", "dog"}, expiresAt, false, "en")
	require.NoError(t, err)

	pv, err := db.GetPendingVerification(100, 200)
	require.NoError(t, err)
	require.NotNil(t, pv)

	assert.Equal(t, int64(100), pv.ChatID)
	assert.Equal(t, int64(200), pv.UserID)
	assert.Equal(t, []string{"cat", "dog"}, pv.CorrectLabels)
	assert.Equal(t, 0, pv.CurrentStep)
	assert.Equal(t, []string{}, pv.UserAnswers)
	assert.Equal(t, 0, pv.RetryCount)
	assert.Equal(t, expiresAt.Unix(), pv.ExpiresAt.Unix())
}

func TestCreatePendingVerification_UpsertResetsStepAndAnswers(t *testing.T) {
	db := newTestDB(t)
	expiresAt := time.Now().Add(5 * time.Minute).Truncate(time.Second)

	require.NoError(t, db.CreatePendingVerification(100, 200, []string{"cat"}, expiresAt, false, "en"))

	// Advance step and answers, then increment retry count
	ok, err := db.UpdateVerificationStep(100, 200, 2, []string{"cat"}, 0)
	require.NoError(t, err)
	require.True(t, ok)
	_, err = db.IncrementRetryCount(100, 200)
	require.NoError(t, err)

	// Upsert with new labels
	newExpires := time.Now().Add(10 * time.Minute).Truncate(time.Second)
	require.NoError(t, db.CreatePendingVerification(100, 200, []string{"bird"}, newExpires, false, "en"))

	pv, err := db.GetPendingVerification(100, 200)
	require.NoError(t, err)
	require.NotNil(t, pv)

	// step and answers reset, retry_count preserved
	assert.Equal(t, []string{"bird"}, pv.CorrectLabels)
	assert.Equal(t, 0, pv.CurrentStep)
	assert.Equal(t, []string{}, pv.UserAnswers)
	assert.Equal(t, 1, pv.RetryCount)
}

func TestGetPendingVerification_NilForMissing(t *testing.T) {
	db := newTestDB(t)
	pv, err := db.GetPendingVerification(999, 999)
	require.NoError(t, err)
	assert.Nil(t, pv)
}

func TestGetPendingVerification_JSONDeserialization(t *testing.T) {
	db := newTestDB(t)
	labels := []string{"alpha", "beta", "gamma"}
	expiresAt := time.Now().Add(time.Minute).Truncate(time.Second)

	require.NoError(t, db.CreatePendingVerification(1, 2, labels, expiresAt, false, "en"))
	ok2, err := db.UpdateVerificationStep(1, 2, 1, []string{"alpha"}, 0)
	require.NoError(t, err)
	require.True(t, ok2)

	pv, err := db.GetPendingVerification(1, 2)
	require.NoError(t, err)
	require.NotNil(t, pv)

	assert.Equal(t, labels, pv.CorrectLabels)
	assert.Equal(t, []string{"alpha"}, pv.UserAnswers)
}

func TestGetPendingVerification_ExpiresAtUnixRoundTrip(t *testing.T) {
	db := newTestDB(t)
	// Truncate to second precision since we store as UNIX timestamp
	expiresAt := time.Now().Add(30 * time.Minute).Truncate(time.Second)

	require.NoError(t, db.CreatePendingVerification(10, 20, []string{"x"}, expiresAt, false, "en"))

	pv, err := db.GetPendingVerification(10, 20)
	require.NoError(t, err)
	require.NotNil(t, pv)

	assert.Equal(t, expiresAt.Unix(), pv.ExpiresAt.Unix())
}

// --- UpdateVerificationMessageID ---

func TestUpdateVerificationMessageID(t *testing.T) {
	db := newTestDB(t)
	expiresAt := time.Now().Add(time.Minute)
	require.NoError(t, db.CreatePendingVerification(1, 1, []string{"a"}, expiresAt, false, "en"))

	require.NoError(t, db.UpdateVerificationMessageID(1, 1, 42))

	pv, err := db.GetPendingVerification(1, 1)
	require.NoError(t, err)
	require.NotNil(t, pv)
	assert.True(t, pv.MessageID.Valid)
	assert.Equal(t, int64(42), pv.MessageID.Int64)
}

// --- UpdateVerificationStep ---

func TestUpdateVerificationStep(t *testing.T) {
	db := newTestDB(t)
	expiresAt := time.Now().Add(time.Minute)
	require.NoError(t, db.CreatePendingVerification(1, 1, []string{"a", "b"}, expiresAt, false, "en"))

	ok, err := db.UpdateVerificationStep(1, 1, 1, []string{"a"}, 0)
	require.NoError(t, err)
	require.True(t, ok)

	pv, err := db.GetPendingVerification(1, 1)
	require.NoError(t, err)
	require.NotNil(t, pv)
	assert.Equal(t, 1, pv.CurrentStep)
	assert.Equal(t, []string{"a"}, pv.UserAnswers)
}

// --- IncrementRetryCount ---

func TestIncrementRetryCount(t *testing.T) {
	db := newTestDB(t)
	expiresAt := time.Now().Add(time.Minute)
	require.NoError(t, db.CreatePendingVerification(1, 1, []string{"a"}, expiresAt, false, "en"))

	count, err := db.IncrementRetryCount(1, 1)
	require.NoError(t, err)
	assert.Equal(t, 1, count)

	count, err = db.IncrementRetryCount(1, 1)
	require.NoError(t, err)
	assert.Equal(t, 2, count)

	// IncrementRetryCount also resets step and answers
	pv, err := db.GetPendingVerification(1, 1)
	require.NoError(t, err)
	require.NotNil(t, pv)
	assert.Equal(t, 0, pv.CurrentStep)
	assert.Equal(t, []string{}, pv.UserAnswers)
}

// --- DeletePendingVerification ---

func TestDeletePendingVerification_Deletes(t *testing.T) {
	db := newTestDB(t)
	expiresAt := time.Now().Add(time.Minute)
	require.NoError(t, db.CreatePendingVerification(1, 1, []string{"a"}, expiresAt, false, "en"))

	require.NoError(t, db.DeletePendingVerification(1, 1))

	pv, err := db.GetPendingVerification(1, 1)
	require.NoError(t, err)
	assert.Nil(t, pv)
}

func TestDeletePendingVerification_NoOpOnMissing(t *testing.T) {
	db := newTestDB(t)
	// Should not error when record does not exist
	require.NoError(t, db.DeletePendingVerification(999, 999))
}

// --- ClaimPendingVerification ---

func TestClaimPendingVerification_ReturnsTrueAndDeletes(t *testing.T) {
	db := newTestDB(t)
	expiresAt := time.Now().Add(time.Minute).Truncate(time.Second)
	require.NoError(t, db.CreatePendingVerification(1, 1, []string{"a"}, expiresAt, false, "en"))

	claimed, err := db.ClaimPendingVerification(1, 1, expiresAt)
	require.NoError(t, err)
	assert.True(t, claimed)

	pv, err := db.GetPendingVerification(1, 1)
	require.NoError(t, err)
	assert.Nil(t, pv)
}

func TestClaimPendingVerification_FalseWhenMismatched(t *testing.T) {
	db := newTestDB(t)
	expiresAt := time.Now().Add(time.Minute).Truncate(time.Second)
	require.NoError(t, db.CreatePendingVerification(1, 1, []string{"a"}, expiresAt, false, "en"))

	wrongExpires := expiresAt.Add(time.Second)
	claimed, err := db.ClaimPendingVerification(1, 1, wrongExpires)
	require.NoError(t, err)
	assert.False(t, claimed)
}

func TestClaimPendingVerification_FalseWhenMissing(t *testing.T) {
	db := newTestDB(t)
	claimed, err := db.ClaimPendingVerification(999, 999, time.Now())
	require.NoError(t, err)
	assert.False(t, claimed)
}

// --- GetExpiredVerifications ---

func TestGetExpiredVerifications_ReturnsExpiredSkipsFuture(t *testing.T) {
	db := newTestDB(t)

	pastExpires := time.Now().Add(-1 * time.Second).Truncate(time.Second)
	futureExpires := time.Now().Add(1 * time.Hour).Truncate(time.Second)

	require.NoError(t, db.CreatePendingVerification(1, 1, []string{"a"}, pastExpires, false, "en"))
	require.NoError(t, db.CreatePendingVerification(1, 2, []string{"b"}, futureExpires, false, "en"))

	expired, err := db.GetExpiredVerifications()
	require.NoError(t, err)
	require.Len(t, expired, 1)
	assert.Equal(t, int64(1), expired[0].UserID)
}

// --- RecordUserJoin / GetRecentJoinCount ---

func TestRecordUserJoin_CountBeforeAndAfter(t *testing.T) {
	db := newTestDB(t)

	count, err := db.GetRecentJoinCount(1, 1, 3600)
	require.NoError(t, err)
	assert.Equal(t, 0, count)

	require.NoError(t, db.RecordUserJoin(1, 1))

	count, err = db.GetRecentJoinCount(1, 1, 3600)
	require.NoError(t, err)
	assert.Equal(t, 1, count)
}

func TestGetRecentJoinCount_ZeroOutsideWindow(t *testing.T) {
	db := newTestDB(t)
	require.NoError(t, db.RecordUserJoin(1, 1))

	// Window of 0 seconds — the just-inserted record should not be counted
	count, err := db.GetRecentJoinCount(1, 1, 0)
	require.NoError(t, err)
	assert.Equal(t, 0, count)
}

// --- CleanupOldJoinHistory ---

func TestCleanupOldJoinHistory(t *testing.T) {
	db := newTestDB(t)

	// Insert a row with a past joined_at (2 seconds ago) directly
	_, err := db.Exec(
		"INSERT INTO user_join_history (chat_id, user_id, joined_at) VALUES (?, ?, datetime('now', '-2 seconds'))",
		int64(1), int64(1),
	)
	require.NoError(t, err)

	// Cleanup records older than 1 second — should remove the 2-second-old row
	require.NoError(t, db.CleanupOldJoinHistory(1))

	count, err := db.GetRecentJoinCount(1, 1, 3600)
	require.NoError(t, err)
	assert.Equal(t, 0, count)
}

// --- DeleteUserJoinHistory ---

func TestDeleteUserJoinHistory(t *testing.T) {
	db := newTestDB(t)
	require.NoError(t, db.RecordUserJoin(1, 1))
	require.NoError(t, db.RecordUserJoin(1, 1))

	require.NoError(t, db.DeleteUserJoinHistory(1, 1))

	count, err := db.GetRecentJoinCount(1, 1, 3600)
	require.NoError(t, err)
	assert.Equal(t, 0, count)
}

// --- IsAdmin / AddAdmin / RemoveAdmin / GetAllAdmins ---

func TestAdmin_FullLifecycle(t *testing.T) {
	db := newTestDB(t)

	ok, err := db.IsAdmin(100)
	require.NoError(t, err)
	assert.False(t, ok)

	require.NoError(t, db.AddAdmin(100))

	ok, err = db.IsAdmin(100)
	require.NoError(t, err)
	assert.True(t, ok)

	admins, err := db.GetAllAdmins()
	require.NoError(t, err)
	assert.Contains(t, admins, int64(100))

	require.NoError(t, db.RemoveAdmin(100))

	ok, err = db.IsAdmin(100)
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestAddAdmin_Idempotent(t *testing.T) {
	db := newTestDB(t)
	require.NoError(t, db.AddAdmin(100))
	require.NoError(t, db.AddAdmin(100)) // INSERT OR IGNORE — should not error

	admins, err := db.GetAllAdmins()
	require.NoError(t, err)

	var count int
	for _, id := range admins {
		if id == 100 {
			count++
		}
	}
	assert.Equal(t, 1, count)
}

func TestGetAllAdmins_EmptySliceWhenNone(t *testing.T) {
	db := newTestDB(t)
	admins, err := db.GetAllAdmins()
	require.NoError(t, err)
	assert.Empty(t, admins)
}

// --- RecordVerificationFailure / HasPreviousFailure / ClearVerificationFailure / CleanupOldFailures ---

func TestVerificationFailure_UpsertAndCheck(t *testing.T) {
	db := newTestDB(t)

	has, err := db.HasPreviousFailure(1, 1)
	require.NoError(t, err)
	assert.False(t, has)

	require.NoError(t, db.RecordVerificationFailure(1, 1))

	has, err = db.HasPreviousFailure(1, 1)
	require.NoError(t, err)
	assert.True(t, has)

	// UPSERT — second call should not error
	require.NoError(t, db.RecordVerificationFailure(1, 1))

	has, err = db.HasPreviousFailure(1, 1)
	require.NoError(t, err)
	assert.True(t, has)
}

func TestClearVerificationFailure(t *testing.T) {
	db := newTestDB(t)
	require.NoError(t, db.RecordVerificationFailure(1, 1))

	require.NoError(t, db.ClearVerificationFailure(1, 1))

	has, err := db.HasPreviousFailure(1, 1)
	require.NoError(t, err)
	assert.False(t, has)
}

func TestClearVerificationFailure_NoOpOnMissing(t *testing.T) {
	db := newTestDB(t)
	require.NoError(t, db.ClearVerificationFailure(999, 999))
}

func TestCleanupOldFailures(t *testing.T) {
	db := newTestDB(t)

	// Insert a failure with a past failed_at (2 seconds ago) directly
	_, err := db.Exec(
		"INSERT INTO verification_failures (chat_id, user_id, failed_at) VALUES (?, ?, datetime('now', '-2 seconds'))",
		int64(1), int64(1),
	)
	require.NoError(t, err)

	// Cleanup records older than 1 second — should remove the 2-second-old row
	require.NoError(t, db.CleanupOldFailures(1))

	has, err := db.HasPreviousFailure(1, 1)
	require.NoError(t, err)
	assert.False(t, has)
}

// Ensure the sql package import is used (GetPendingVerification returns sql.NullInt64)
var _ = sql.NullInt64{}
