package database

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func passNoticeTTL(t *testing.T, db *DB, chatID int64) int {
	t.Helper()
	cs, err := db.GetChatSettings(chatID)
	require.NoError(t, err)
	if cs == nil {
		return 0
	}
	return cs.PassNoticeTTLSeconds
}

func TestChatPassNoticeTTL_DefaultsToOffAndRoundTrips(t *testing.T) {
	db := newTestDB(t)
	const chatID int64 = -100

	assert.Zero(t, passNoticeTTL(t, db, chatID), "a chat nobody configured keeps its notices")

	require.NoError(t, db.SetChatPassNoticeTTL(chatID, 45, 7))
	assert.Equal(t, 45, passNoticeTTL(t, db, chatID))
	assert.Zero(t, passNoticeTTL(t, db, -200), "the setting is per chat")

	require.NoError(t, db.SetChatPassNoticeTTL(chatID, 0, 7))
	assert.Zero(t, passNoticeTTL(t, db, chatID))
}

func TestChatPassNoticeTTL_IndependentOfOtherSettings(t *testing.T) {
	db := newTestDB(t)
	const chatID int64 = -100
	on := true
	require.NoError(t, db.SetChatEphemeralVerify(chatID, &on, 7))
	require.NoError(t, db.SetChatEphemeralAdminVisible(chatID, true, 7))

	require.NoError(t, db.SetChatPassNoticeTTL(chatID, 60, 8))

	cs, err := db.GetChatSettings(chatID)
	require.NoError(t, err)
	require.NotNil(t, cs.EphemeralVerify)
	assert.True(t, *cs.EphemeralVerify)
	assert.True(t, cs.EphemeralAdminVisible)
	assert.Equal(t, 60, cs.PassNoticeTTLSeconds)

	off := false
	require.NoError(t, db.SetChatEphemeralVerify(chatID, &off, 9))
	assert.Equal(t, 60, passNoticeTTL(t, db, chatID), "changing another setting leaves this one alone")
}

func TestMigrate_AddsPassNoticeTTLToExistingChatSettings(t *testing.T) {
	db := newTestDB(t)
	on := true
	require.NoError(t, db.SetChatEphemeralVerify(-100, &on, 7))
	// A deployment from before the setting existed: same table, no column.
	_, err := db.Exec("ALTER TABLE chat_settings DROP COLUMN pass_notice_ttl_seconds")
	require.NoError(t, err)

	require.NoError(t, db.Migrate(""))

	cs, err := db.GetChatSettings(-100)
	require.NoError(t, err)
	require.NotNil(t, cs)
	assert.True(t, *cs.EphemeralVerify, "existing settings survive the upgrade")
	assert.Zero(t, cs.PassNoticeTTLSeconds, "and autodelete starts off")
	require.NoError(t, db.SetChatPassNoticeTTL(-100, 30, 7))
	assert.Equal(t, 30, passNoticeTTL(t, db, -100))
}

func TestScheduledDeletions_DueOnlyOnceTheDeadlinePasses(t *testing.T) {
	db := newTestDB(t)
	now := time.Unix(1_800_000_000, 0)
	require.NoError(t, db.ScheduleMessageDeletion(-100, 1, now.Add(-time.Minute)))
	require.NoError(t, db.ScheduleMessageDeletion(-100, 2, now))
	require.NoError(t, db.ScheduleMessageDeletion(-100, 3, now.Add(time.Second)))
	require.NoError(t, db.ScheduleMessageDeletion(-200, 1, now.Add(-time.Hour)))

	due, err := db.GetDueMessageDeletions(now, 10)
	require.NoError(t, err)

	assert.Equal(t, []ScheduledDeletion{
		{ChatID: -200, MessageID: 1, DeleteAt: now.Add(-time.Hour)},
		{ChatID: -100, MessageID: 1, DeleteAt: now.Add(-time.Minute)},
		{ChatID: -100, MessageID: 2, DeleteAt: now},
	}, due, "oldest first; message IDs are only unique within a chat")
}

func TestScheduledDeletions_BatchLimitTakesTheOldest(t *testing.T) {
	db := newTestDB(t)
	now := time.Unix(1_800_000_000, 0)
	for id := 1; id <= 5; id++ {
		require.NoError(t, db.ScheduleMessageDeletion(-100, id, now.Add(-time.Duration(id)*time.Minute)))
	}

	due, err := db.GetDueMessageDeletions(now, 2)
	require.NoError(t, err)

	require.Len(t, due, 2)
	assert.Equal(t, []int{5, 4}, []int{due[0].MessageID, due[1].MessageID})
}

func TestScheduledDeletions_RemoveAndReschedule(t *testing.T) {
	db := newTestDB(t)
	now := time.Unix(1_800_000_000, 0)
	require.NoError(t, db.ScheduleMessageDeletion(-100, 1, now.Add(-time.Minute)))
	require.NoError(t, db.ScheduleMessageDeletion(-100, 2, now.Add(-time.Minute)))

	require.NoError(t, db.RemoveScheduledDeletion(-100, 1))
	require.NoError(t, db.RemoveScheduledDeletion(-100, 99), "removing an unknown message is not an error")
	// Scheduling the same message again moves its deadline instead of failing.
	require.NoError(t, db.ScheduleMessageDeletion(-100, 2, now.Add(time.Hour)))

	due, err := db.GetDueMessageDeletions(now, 10)
	require.NoError(t, err)
	assert.Empty(t, due)
	later, err := db.GetDueMessageDeletions(now.Add(2*time.Hour), 10)
	require.NoError(t, err)
	assert.Equal(t, []ScheduledDeletion{{ChatID: -100, MessageID: 2, DeleteAt: now.Add(time.Hour)}}, later)
}
