package database

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIsValidJoinPolicy(t *testing.T) {
	assert.True(t, IsValidJoinPolicy(JoinPolicyApprove))
	assert.True(t, IsValidJoinPolicy(JoinPolicyDecline))
	assert.True(t, IsValidJoinPolicy(JoinPolicyDismiss))
	assert.False(t, IsValidJoinPolicy(""))
	assert.False(t, IsValidJoinPolicy("ignore"))
}

func TestChatJoinPolicy_GetMissingReturnsEmptyPolicy(t *testing.T) {
	db := newTestDB(t)

	policy, err := db.GetChatJoinPolicy(-100)
	require.NoError(t, err)
	assert.Equal(t, "", policy)
}

func TestChatJoinPolicy_SetAndUpdate(t *testing.T) {
	db := newTestDB(t)
	const chatID int64 = -100123

	require.NoError(t, db.SetChatJoinPolicy(chatID, JoinPolicyDecline, 42))
	policy, err := db.GetChatJoinPolicy(chatID)
	require.NoError(t, err)
	assert.Equal(t, JoinPolicyDecline, policy)

	require.NoError(t, db.SetChatJoinPolicy(chatID, JoinPolicyDismiss, 99))
	policy, err = db.GetChatJoinPolicy(chatID)
	require.NoError(t, err)
	assert.Equal(t, JoinPolicyDismiss, policy)

	var updatedBy int64
	err = db.QueryRow(
		`SELECT updated_by FROM chat_join_policy WHERE chat_id = ?`,
		chatID,
	).Scan(&updatedBy)
	require.NoError(t, err)
	assert.Equal(t, int64(99), updatedBy)
}

func TestChatJoinPolicy_SetRejectsInvalidPolicy(t *testing.T) {
	db := newTestDB(t)

	err := db.SetChatJoinPolicy(-100, "ignore", 42)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `invalid join policy "ignore"`)

	policy, err := db.GetChatJoinPolicy(-100)
	require.NoError(t, err)
	assert.Equal(t, "", policy)
}
