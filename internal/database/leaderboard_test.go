package database

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLeaderboard_RecordTopAndRankings(t *testing.T) {
	db := newTestDB(t)

	require.NoError(t, db.RecordLeaderboardEntry(1, "slow", "Slow", "Agent", 5*time.Second))
	require.NoError(t, db.RecordLeaderboardEntry(2, "fast", "Fast", "", 2*time.Second))
	require.NoError(t, db.RecordLeaderboardEntry(3, "mid", "Mid", "Agent", 3*time.Second))

	entries, err := db.GetTopLeaderboard(2)
	require.NoError(t, err)
	require.Len(t, entries, 2)
	assert.Equal(t, 1, entries[0].Rank)
	assert.Equal(t, int64(2), entries[0].UserID)
	assert.Equal(t, "fast", entries[0].Username)
	assert.Equal(t, "", entries[0].LastName)
	assert.Equal(t, int64(2000), entries[0].CompletionTimeMs)
	assert.False(t, entries[0].CompletedAt.IsZero())
	assert.Equal(t, 2, entries[1].Rank)
	assert.Equal(t, int64(3), entries[1].UserID)

	rank, err := db.GetUserRank(1)
	require.NoError(t, err)
	assert.Equal(t, 3, rank)

	percentile, err := db.GetUserPercentile(2)
	require.NoError(t, err)
	assert.InDelta(t, 66.666, percentile, 0.01)

	timePercentile, err := db.GetTimePercentile(2500 * time.Millisecond)
	require.NoError(t, err)
	assert.InDelta(t, 66.666, timePercentile, 0.01)
}

func TestLeaderboard_RecordKeepsBestTimeButUpdatesProfile(t *testing.T) {
	db := newTestDB(t)

	require.NoError(t, db.RecordLeaderboardEntry(1, "first", "First", "Name", 2*time.Second))
	require.NoError(t, db.RecordLeaderboardEntry(1, "newname", "New", "Profile", 4*time.Second))

	entries, err := db.GetTopLeaderboard(10)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, "newname", entries[0].Username)
	assert.Equal(t, "New", entries[0].FirstName)
	assert.Equal(t, "Profile", entries[0].LastName)
	assert.Equal(t, int64(2000), entries[0].CompletionTimeMs)

	require.NoError(t, db.RecordLeaderboardEntry(1, "newname", "New", "Profile", 1500*time.Millisecond))
	entries, err = db.GetTopLeaderboard(10)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, int64(1500), entries[0].CompletionTimeMs)
}

func TestLeaderboard_EmptyAndMissingUser(t *testing.T) {
	db := newTestDB(t)

	top, err := db.GetTopLeaderboard(5)
	require.NoError(t, err)
	assert.Empty(t, top)

	rank, err := db.GetUserRank(99)
	require.NoError(t, err)
	assert.Equal(t, 0, rank)

	percentile, err := db.GetUserPercentile(99)
	require.NoError(t, err)
	assert.Equal(t, 0.0, percentile)

	timePercentile, err := db.GetTimePercentile(time.Second)
	require.NoError(t, err)
	assert.Equal(t, 0.0, timePercentile)

	require.NoError(t, db.RecordLeaderboardEntry(1, "one", "One", "", time.Second))
	rank, err = db.GetUserRank(99)
	require.NoError(t, err)
	assert.Equal(t, 0, rank)
}
