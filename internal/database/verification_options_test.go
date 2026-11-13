package database

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Options back the per-step keyboard redraw for ephemeral challenges, whose
// callbacks don't carry the keyboard to scrape. They are written in the same
// statement as the challenge itself, so a challenge can never exist without
// its keypad, and a replaced challenge can never keep the previous one.

func TestVerificationOptions_StoredWithChallenge(t *testing.T) {
	db := newTestDB(t)
	opts := []string{"cat", "dog", "能量塔/门泉/Portal"}
	require.NoError(t, db.CreatePendingVerification(1, 2, []string{"cat"}, time.Now().Add(time.Minute), false, "en", opts))

	pv, err := db.GetPendingVerification(1, 2)
	require.NoError(t, err)
	assert.Equal(t, opts, pv.Options, "labels incl. non-ASCII must survive verbatim")
}

func TestVerificationOptions_NilStoredAsEmptyList(t *testing.T) {
	db := newTestDB(t)
	require.NoError(t, db.CreatePendingVerification(1, 2, []string{"cat"}, time.Now().Add(time.Minute), false, "en", nil))

	var raw string
	require.NoError(t, db.QueryRow(`SELECT options FROM pending_verifications WHERE chat_id = 1 AND user_id = 2`).Scan(&raw))
	assert.Equal(t, "[]", raw, "the column must never hold JSON null")
}

func TestVerificationOptions_ReturnedByExpiredSweep(t *testing.T) {
	db := newTestDB(t)
	require.NoError(t, db.CreatePendingVerification(1, 2, []string{"cat"}, time.Now().Add(-time.Second), false, "en", []string{"cat", "dog"}))

	expired, err := db.GetExpiredVerifications()
	require.NoError(t, err)
	require.Len(t, expired, 1)
	assert.Equal(t, []string{"cat", "dog"}, expired[0].Options)
}

func TestVerificationOptions_ReplacedOnRecreate(t *testing.T) {
	db := newTestDB(t)
	require.NoError(t, db.CreatePendingVerification(1, 2, []string{"cat"}, time.Now().Add(time.Minute), false, "en", []string{"old", "keypad"}))

	// A rejoin upserts the same (chat,user) row with a new challenge.
	require.NoError(t, db.CreatePendingVerification(1, 2, []string{"bird"}, time.Now().Add(time.Minute), false, "en", []string{"new", "keys"}))

	pv, err := db.GetPendingVerification(1, 2)
	require.NoError(t, err)
	assert.Equal(t, []string{"new", "keys"}, pv.Options, "a new challenge must not keep the previous keypad")
}

func TestVerificationOptions_ReplacedOnRegenerate(t *testing.T) {
	db := newTestDB(t)
	require.NoError(t, db.CreatePendingVerification(1, 2, []string{"cat"}, time.Now().Add(time.Minute), false, "en", []string{"old", "keypad"}))

	ok, err := db.RegenerateVerification(1, 2, []string{"bird"}, time.Now().Add(time.Minute), []string{"new", "keys"})
	require.NoError(t, err)
	require.True(t, ok)

	pv, err := db.GetPendingVerification(1, 2)
	require.NoError(t, err)
	assert.Equal(t, []string{"new", "keys"}, pv.Options)
}

func TestVerificationOptions_RefusedRegenerateKeepsKeypad(t *testing.T) {
	// The one-time regenerate is gated on regenerated=0. A refused second
	// regenerate must leave the current keypad intact, not half-apply.
	db := newTestDB(t)
	require.NoError(t, db.CreatePendingVerification(1, 2, []string{"cat"}, time.Now().Add(time.Minute), false, "en", []string{"a"}))
	ok, err := db.RegenerateVerification(1, 2, []string{"bird"}, time.Now().Add(time.Minute), []string{"b"})
	require.NoError(t, err)
	require.True(t, ok)

	ok, err = db.RegenerateVerification(1, 2, []string{"fish"}, time.Now().Add(time.Minute), []string{"c"})
	require.NoError(t, err)
	require.False(t, ok)

	pv, err := db.GetPendingVerification(1, 2)
	require.NoError(t, err)
	assert.Equal(t, []string{"b"}, pv.Options)
}

func TestVerificationOptions_MalformedDegradesToEmpty(t *testing.T) {
	db := newTestDB(t)
	require.NoError(t, db.CreatePendingVerification(1, 2, []string{"cat"}, time.Now().Add(time.Minute), false, "en", nil))
	_, err := db.Exec(`UPDATE pending_verifications SET options = 'not-json' WHERE chat_id = 1 AND user_id = 2`)
	require.NoError(t, err)

	pv, err := db.GetPendingVerification(1, 2)
	require.NoError(t, err, "a bad options value must not fail the whole lookup")
	require.NotNil(t, pv)
	assert.Empty(t, pv.Options)
	assert.Equal(t, []string{"cat"}, pv.CorrectLabels)
}

// The step write is a compare-and-swap: callbacks run concurrently, so two
// taps on the same step must not both be recorded.
func TestUpdateVerificationStep_CompareAndSwapOnStep(t *testing.T) {
	db := newTestDB(t)
	require.NoError(t, db.CreatePendingVerification(1, 2, []string{"a", "b", "c"}, time.Now().Add(time.Minute), false, "en", nil))

	ok, err := db.UpdateVerificationStep(1, 2, 0, 1, []string{"first"}, 0)
	require.NoError(t, err)
	require.True(t, ok)

	// A second tap that also read step 0 must lose.
	ok, err = db.UpdateVerificationStep(1, 2, 0, 1, []string{"second"}, 0)
	require.NoError(t, err)
	assert.False(t, ok, "only one tap may advance a given step")

	pv, err := db.GetPendingVerification(1, 2)
	require.NoError(t, err)
	assert.Equal(t, 1, pv.CurrentStep)
	assert.Equal(t, []string{"first"}, pv.UserAnswers, "the losing tap's answer must not overwrite the winner's")

	ok, err = db.UpdateVerificationStep(1, 2, 1, 2, []string{"first", "next"}, 0)
	require.NoError(t, err)
	assert.True(t, ok, "advancing from the current step still works")
}
