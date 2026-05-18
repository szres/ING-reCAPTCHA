package database

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRegenerateVerification_SwapsChallengeAndBumpsVersion(t *testing.T) {
	db := newTestDB(t)
	const chatID, userID int64 = 1, 1
	expiresAt := time.Now().Add(time.Minute).Truncate(time.Second)
	require.NoError(t, db.CreatePendingVerification(chatID, userID, []string{"cat", "dog"}, expiresAt, false, "en"))

	pv, err := db.GetPendingVerification(chatID, userID)
	require.NoError(t, err)
	require.NotNil(t, pv)
	assert.Equal(t, 0, pv.ChallengeVersion)
	assert.False(t, pv.Regenerated)

	newExpires := time.Now().Add(2 * time.Minute).Truncate(time.Second)
	ok, err := db.RegenerateVerification(chatID, userID, []string{"bird", "fish"}, newExpires)
	require.NoError(t, err)
	require.True(t, ok)

	got, err := db.GetPendingVerification(chatID, userID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.True(t, got.Regenerated)
	assert.Equal(t, 1, got.ChallengeVersion)
	assert.Equal(t, []string{"bird", "fish"}, got.CorrectLabels)
	assert.Equal(t, 0, got.CurrentStep)
	assert.Empty(t, got.UserAnswers)
	assert.WithinDuration(t, newExpires, got.ExpiresAt, time.Second)
	assert.False(t, got.MessageID.Valid)
}

func TestRegenerateVerification_RefusesSecondTime(t *testing.T) {
	db := newTestDB(t)
	const chatID, userID int64 = 1, 1
	require.NoError(t, db.CreatePendingVerification(chatID, userID, []string{"cat"}, time.Now().Add(time.Minute), false, "en"))

	ok, err := db.RegenerateVerification(chatID, userID, []string{"dog"}, time.Now().Add(time.Minute))
	require.NoError(t, err)
	require.True(t, ok)

	// Second call must be refused — regenerated=1 now.
	ok, err = db.RegenerateVerification(chatID, userID, []string{"bird"}, time.Now().Add(time.Minute))
	require.NoError(t, err)
	assert.False(t, ok)

	pv, err := db.GetPendingVerification(chatID, userID)
	require.NoError(t, err)
	assert.Equal(t, []string{"dog"}, pv.CorrectLabels, "second regenerate must not alter state")
	assert.Equal(t, 1, pv.ChallengeVersion)
}

func TestRegenerateVerification_NoopOnMissingRow(t *testing.T) {
	db := newTestDB(t)
	ok, err := db.RegenerateVerification(999, 999, []string{"x"}, time.Now().Add(time.Minute))
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestCreatePendingVerification_ResetsRegenerateState(t *testing.T) {
	db := newTestDB(t)
	const chatID, userID int64 = 1, 1
	require.NoError(t, db.CreatePendingVerification(chatID, userID, []string{"a"}, time.Now().Add(time.Minute), false, "en"))

	ok, err := db.RegenerateVerification(chatID, userID, []string{"b"}, time.Now().Add(time.Minute))
	require.NoError(t, err)
	require.True(t, ok)

	// Fresh session via the upsert path: regenerated and challenge_version must reset to 0.
	require.NoError(t, db.CreatePendingVerification(chatID, userID, []string{"c"}, time.Now().Add(time.Minute), false, "en"))

	pv, err := db.GetPendingVerification(chatID, userID)
	require.NoError(t, err)
	require.NotNil(t, pv)
	assert.False(t, pv.Regenerated, "regenerated must reset on fresh session")
	assert.Equal(t, 0, pv.ChallengeVersion, "challenge_version must reset on fresh session")
}

func TestUpdateVerificationStep_RejectsStaleVersion(t *testing.T) {
	db := newTestDB(t)
	const chatID, userID int64 = 1, 1
	require.NoError(t, db.CreatePendingVerification(chatID, userID, []string{"a", "b"}, time.Now().Add(time.Minute), false, "en"))

	// Bump version via regenerate.
	ok, err := db.RegenerateVerification(chatID, userID, []string{"c", "d"}, time.Now().Add(time.Minute))
	require.NoError(t, err)
	require.True(t, ok)

	// A late-arriving callback tries to advance using the OLD version (0).
	ok, err = db.UpdateVerificationStep(chatID, userID, 1, []string{"stale"}, 0)
	require.NoError(t, err)
	assert.False(t, ok, "stale-version update must not apply")

	// New version (1) succeeds.
	ok, err = db.UpdateVerificationStep(chatID, userID, 1, []string{"fresh"}, 1)
	require.NoError(t, err)
	assert.True(t, ok)

	pv, err := db.GetPendingVerification(chatID, userID)
	require.NoError(t, err)
	assert.Equal(t, []string{"fresh"}, pv.UserAnswers, "only the version-matched answer should persist")
}
