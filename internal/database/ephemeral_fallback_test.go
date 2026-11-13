package database

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const fallbackChatID int64 = -100

// seedChallenge stores a pending challenge that expires at expiresAt and,
// when ephemeralID is non-zero, marks it as sent ephemerally.
func seedChallenge(t *testing.T, db *DB, userID int64, expiresAt time.Time, ephemeralID int) {
	t.Helper()
	require.NoError(t, db.CreatePendingVerification(fallbackChatID, userID, []string{"cat", "dog"}, expiresAt, false, "en", []string{"cat", "dog", "bird"}))
	if ephemeralID != 0 {
		require.NoError(t, db.UpdateVerificationEphemeralMessageID(fallbackChatID, userID, ephemeralID))
	}
}

func untouchedUserIDs(t *testing.T, db *DB, now, expiresBy time.Time) []int64 {
	t.Helper()
	pvs, err := db.GetUntouchedEphemeralChallenges(now, expiresBy)
	require.NoError(t, err)
	ids := make([]int64, 0, len(pvs))
	for _, pv := range pvs {
		ids = append(ids, pv.UserID)
	}
	return ids
}

func TestGetUntouchedEphemeralChallenges_SelectsOnlyOldUntouchedEphemeralOnes(t *testing.T) {
	db := newTestDB(t)
	now := time.Unix(1_800_000_000, 0)
	// With a 120s timeout and a 30s grace period, a challenge is due once it
	// expires within the next 90 seconds.
	expiresBy := now.Add(90 * time.Second)

	const (
		due         int64 = 1
		tooRecent   int64 = 2
		answered    int64 = 3
		regenerated int64 = 4
		regular     int64 = 5
		alreadyGone int64 = 6
		dueExactly  int64 = 7
		expiringNow int64 = 8
	)
	seedChallenge(t, db, due, now.Add(60*time.Second), 9001)
	seedChallenge(t, db, tooRecent, now.Add(110*time.Second), 9002)
	seedChallenge(t, db, answered, now.Add(60*time.Second), 9003)
	seedChallenge(t, db, regenerated, now.Add(60*time.Second), 9004)
	seedChallenge(t, db, regular, now.Add(60*time.Second), 0)
	seedChallenge(t, db, alreadyGone, now.Add(-time.Second), 9006)
	seedChallenge(t, db, dueExactly, expiresBy, 9007)
	seedChallenge(t, db, expiringNow, now, 9008)

	advanced, err := db.UpdateVerificationStep(fallbackChatID, answered, 0, 1, []string{"cat"}, 0)
	require.NoError(t, err)
	require.True(t, advanced)
	// A regenerated challenge is one the joiner asked for, so they have seen
	// the ephemeral message. Regenerating also issues a new ephemeral ID.
	swapped, err := db.RegenerateVerification(fallbackChatID, regenerated, []string{"fish"}, now.Add(60*time.Second), []string{"fish", "cat"})
	require.NoError(t, err)
	require.True(t, swapped)
	require.NoError(t, db.UpdateVerificationEphemeralMessageID(fallbackChatID, regenerated, 9104))

	assert.ElementsMatch(t, []int64{due, dueExactly}, untouchedUserIDs(t, db, now, expiresBy))
}

func TestReplaceUntouchedEphemeralChallenge_SwapsInTheRegularMessage(t *testing.T) {
	db := newTestDB(t)
	const userID int64 = 1
	seedChallenge(t, db, userID, time.Unix(1_800_000_060, 0), 9001)
	require.NoError(t, db.SetVerificationAdminBroadcast(fallbackChatID, userID, true))
	newDeadline := time.Unix(1_800_000_500, 0)

	replaced, err := db.ReplaceUntouchedEphemeralChallenge(fallbackChatID, userID, 0, []string{"fish", "bird"}, newDeadline, []string{"fish", "bird", "cat"}, 321)
	require.NoError(t, err)
	require.True(t, replaced)

	pv, err := db.GetPendingVerification(fallbackChatID, userID)
	require.NoError(t, err)
	require.NotNil(t, pv)
	assert.Zero(t, pv.EphemeralMessageID, "the challenge is no longer ephemeral")
	assert.True(t, pv.MessageID.Valid)
	assert.Equal(t, int64(321), pv.MessageID.Int64)
	assert.Equal(t, []string{"fish", "bird"}, pv.CorrectLabels)
	assert.Equal(t, []string{"fish", "bird", "cat"}, pv.Options)
	assert.Equal(t, newDeadline.Unix(), pv.ExpiresAt.Unix(), "the joiner gets a full fresh timeout")
	assert.Equal(t, 1, pv.ChallengeVersion, "taps on the old ephemeral keyboard must be rejected as stale")
	assert.Zero(t, pv.CurrentStep)
	assert.Empty(t, pv.UserAnswers)
	assert.False(t, pv.Regenerated, "the regenerate chance is not consumed")
	assert.False(t, pv.AdminBroadcast, "admin copies belonged to the ephemeral challenge")
}

func TestReplaceUntouchedEphemeralChallenge_RefusedOnceTheJoinerResponded(t *testing.T) {
	deadline := time.Unix(1_800_000_060, 0)
	newDeadline := time.Unix(1_800_000_500, 0)
	const userID int64 = 1

	cases := map[string]func(t *testing.T, db *DB){
		"answered the first picture": func(t *testing.T, db *DB) {
			ok, err := db.UpdateVerificationStep(fallbackChatID, userID, 0, 1, []string{"cat"}, 0)
			require.NoError(t, err)
			require.True(t, ok)
		},
		"asked for a new challenge": func(t *testing.T, db *DB) {
			ok, err := db.RegenerateVerification(fallbackChatID, userID, []string{"fish"}, deadline, []string{"fish"})
			require.NoError(t, err)
			require.True(t, ok)
			require.NoError(t, db.UpdateVerificationEphemeralMessageID(fallbackChatID, userID, 9101))
		},
		"finished or left": func(t *testing.T, db *DB) {
			require.NoError(t, db.DeletePendingVerification(fallbackChatID, userID))
		},
	}
	for name, respond := range cases {
		t.Run(name, func(t *testing.T) {
			db := newTestDB(t)
			seedChallenge(t, db, userID, deadline, 9001)
			respond(t, db)
			before, err := db.GetPendingVerification(fallbackChatID, userID)
			require.NoError(t, err)

			replaced, err := db.ReplaceUntouchedEphemeralChallenge(fallbackChatID, userID, 0, []string{"x"}, newDeadline, []string{"x"}, 321)
			require.NoError(t, err)
			assert.False(t, replaced)

			after, err := db.GetPendingVerification(fallbackChatID, userID)
			require.NoError(t, err)
			assert.Equal(t, before, after, "a refused swap must leave the challenge untouched")
		})
	}
}

func TestReplaceUntouchedEphemeralChallenge_IgnoresRegularChallenges(t *testing.T) {
	db := newTestDB(t)
	const userID int64 = 1
	seedChallenge(t, db, userID, time.Unix(1_800_000_060, 0), 0)

	replaced, err := db.ReplaceUntouchedEphemeralChallenge(fallbackChatID, userID, 0, []string{"x"}, time.Unix(1_800_000_500, 0), []string{"x"}, 321)
	require.NoError(t, err)
	assert.False(t, replaced)
}
