package bot

import (
	"fmt"
	"testing"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func groupMsg(chatID int64, userID int64, msgID int) *tgbotapi.Message {
	return &tgbotapi.Message{
		MessageID: msgID,
		Chat:      &tgbotapi.Chat{ID: chatID, Type: "supergroup"},
		From:      &tgbotapi.User{ID: userID},
	}
}

func TestHandleMessage_JoinServiceMessage(t *testing.T) {
	b, mock := newTestBot(t)

	const (
		chatID   int64 = 100
		memberID int64 = 999
		msgID          = 42
	)

	// Create a pending verification so UpdateJoinMessageID has a record to update
	require.NoError(t, b.db.CreatePendingVerification(chatID, memberID, []string{"cat"}, time.Now().Add(time.Minute), false, "en"))

	msg := groupMsg(chatID, 0, msgID)
	msg.NewChatMembers = []tgbotapi.User{{ID: memberID, FirstName: "Alice"}}
	msg.From = nil // service messages have no From

	b.handleMessage(msg)

	// No immediate delete — join message ID should be stored in DB instead
	mock.mu.Lock()
	reqCount := len(mock.requested)
	mock.mu.Unlock()
	assert.Equal(t, 0, reqCount, "join message should NOT be deleted immediately")

	pv, err := b.db.GetPendingVerification(chatID, memberID)
	require.NoError(t, err)
	require.NotNil(t, pv)
	assert.True(t, pv.JoinMessageID.Valid, "join_message_id should be stored")
	assert.Equal(t, int64(msgID), pv.JoinMessageID.Int64)
}

func TestHandleMessage_JoinServiceMessage_NoPendingVerification(t *testing.T) {
	b, mock := newTestBot(t)

	// No pending verification record — UpdateJoinMessageID is a no-op, no panic
	msg := groupMsg(100, 0, 42)
	msg.NewChatMembers = []tgbotapi.User{{ID: 999}}
	msg.From = nil

	assert.NotPanics(t, func() { b.handleMessage(msg) })

	mock.mu.Lock()
	defer mock.mu.Unlock()
	assert.Empty(t, mock.requested)
}

func TestHandleMessage_PendingUser_DeletesMessage(t *testing.T) {
	b, mock := newTestBot(t)

	const (
		chatID int64 = 100
		userID int64 = 200
	)
	require.NoError(t, b.db.CreatePendingVerification(chatID, userID, []string{"cat"}, time.Now().Add(time.Minute), false, "en"))

	b.handleMessage(groupMsg(chatID, userID, 55))

	mock.mu.Lock()
	defer mock.mu.Unlock()
	require.Len(t, mock.requested, 1)
	deleteConfig, ok := mock.requested[0].(tgbotapi.DeleteMessageConfig)
	require.True(t, ok)
	assert.Equal(t, 55, deleteConfig.MessageID)
}

func TestHandleMessage_NoPendingVerification_NoDelete(t *testing.T) {
	b, mock := newTestBot(t)

	b.handleMessage(groupMsg(100, 999, 77))

	mock.mu.Lock()
	defer mock.mu.Unlock()
	assert.Empty(t, mock.requested, "no pending verification → no delete")
}

func TestHandleMessage_PrivateChat_NoDelete(t *testing.T) {
	b, mock := newTestBot(t)

	msg := &tgbotapi.Message{
		MessageID: 1,
		Chat:      &tgbotapi.Chat{ID: 1, Type: "private"},
		From:      &tgbotapi.User{ID: 42},
	}
	b.handleMessage(msg)

	mock.mu.Lock()
	defer mock.mu.Unlock()
	assert.Empty(t, mock.requested)
	assert.Empty(t, mock.sent)
}

func TestHandleMessage_NilFrom_NoPanic(t *testing.T) {
	b, _ := newTestBot(t)

	msg := groupMsg(100, 0, 10)
	msg.From = nil

	assert.NotPanics(t, func() { b.handleMessage(msg) })
}

func TestHandleChatMemberUpdate_UserRemoved_CleansVerificationState(t *testing.T) {
	b, mock := newTestBot(t)

	const (
		chatID            int64 = 100
		userID            int64 = 200
		verificationMsgID int   = 1001
		joinMsgID         int   = 1000
	)

	require.NoError(t, b.db.CreatePendingVerification(chatID, userID, []string{"cat"}, time.Now().Add(time.Minute), false, "en"))
	require.NoError(t, b.db.UpdateVerificationMessageID(chatID, userID, verificationMsgID))
	require.NoError(t, b.db.UpdateJoinMessageID(chatID, userID, joinMsgID))
	require.NoError(t, b.db.RecordUserJoin(chatID, userID))
	b.joinMsgCache.Store(fmt.Sprintf("%d:%d", chatID, userID), joinMsgID)

	update := &tgbotapi.ChatMemberUpdated{
		Chat:          tgbotapi.Chat{ID: chatID},
		OldChatMember: tgbotapi.ChatMember{Status: "restricted", User: &tgbotapi.User{ID: userID}},
		NewChatMember: tgbotapi.ChatMember{Status: "kicked", User: &tgbotapi.User{ID: userID}},
	}

	b.handleChatMemberUpdate(update)

	// Verify verification message deletion was attempted
	assert.True(t, mock.deleteMessageCalled, "should attempt to delete verification message")
	assert.Contains(t, mock.deletedMessages, verificationMsgID, "should delete verification message")
	assert.Contains(t, mock.deletedMessages, joinMsgID, "should delete join message")

	// Verify database cleanup
	pv, err := b.db.GetPendingVerification(chatID, userID)
	require.NoError(t, err)
	assert.Nil(t, pv, "pending verification should be removed when user is kicked/left")

	recent, err := b.db.GetRecentJoinCount(chatID, userID, 3600)
	require.NoError(t, err)
	assert.Equal(t, 0, recent, "join history should be removed when user is kicked/left")

	_, ok := b.joinMsgCache.Load(fmt.Sprintf("%d:%d", chatID, userID))
	assert.False(t, ok, "join message cache entry should be removed when user is kicked/left")
}

func TestCleanupVerificationState_OnlyCacheHasJoinMsgID(t *testing.T) {
	b, mock := newTestBot(t)

	const (
		chatID            int64 = 100
		userID            int64 = 200
		verificationMsgID int   = 1001
		joinMsgID         int   = 1000
	)

	// Create pending verification but don't set join_message_id in DB
	require.NoError(t, b.db.CreatePendingVerification(chatID, userID, []string{"cat"}, time.Now().Add(time.Minute), false, "en"))
	require.NoError(t, b.db.UpdateVerificationMessageID(chatID, userID, verificationMsgID))
	// Only cache has join message ID
	b.joinMsgCache.Store(fmt.Sprintf("%d:%d", chatID, userID), joinMsgID)

	b.cleanupVerificationState(chatID, userID)

	// Verify both messages were deleted (join message from cache fallback)
	assert.True(t, mock.deleteMessageCalled, "should attempt to delete messages")
	assert.Contains(t, mock.deletedMessages, verificationMsgID, "should delete verification message")
	assert.Contains(t, mock.deletedMessages, joinMsgID, "should delete join message from cache")
}

func TestCleanupVerificationState_DeleteMessageFailure_StillCleansDB(t *testing.T) {
	b, mock := newTestBot(t)

	const (
		chatID            int64 = 100
		userID            int64 = 200
		verificationMsgID int   = 1001
	)

	require.NoError(t, b.db.CreatePendingVerification(chatID, userID, []string{"cat"}, time.Now().Add(time.Minute), false, "en"))
	require.NoError(t, b.db.UpdateVerificationMessageID(chatID, userID, verificationMsgID))

	// Simulate message deletion failure
	mock.requestErr = fmt.Errorf("message not found")

	b.cleanupVerificationState(chatID, userID)

	// Verify DB cleanup still happened despite message deletion failure
	pv, err := b.db.GetPendingVerification(chatID, userID)
	require.NoError(t, err)
	assert.Nil(t, pv, "pending verification should be removed even if message deletion fails")
}

func TestCleanupVerificationState_NoPendingVerification_OnlyCleansJoinHistory(t *testing.T) {
	b, mock := newTestBot(t)

	const (
		chatID int64 = 100
		userID int64 = 200
	)

	// Only join history exists, no pending verification
	require.NoError(t, b.db.RecordUserJoin(chatID, userID))
	b.joinMsgCache.Store(fmt.Sprintf("%d:%d", chatID, userID), 1000)

	b.cleanupVerificationState(chatID, userID)

	// Verify no message deletion was attempted
	assert.False(t, mock.deleteMessageCalled, "should not attempt to delete messages when no pending verification")

	// Verify join history was cleaned
	recent, err := b.db.GetRecentJoinCount(chatID, userID, 3600)
	require.NoError(t, err)
	assert.Equal(t, 0, recent, "join history should be cleaned")
}

func TestCleanupVerificationState_MessageNotYetSent_SchedulesDelayedCleanup(t *testing.T) {
	b, mock := newTestBot(t)

	const (
		chatID            int64 = 100
		userID            int64 = 200
		verificationMsgID int   = 1001
	)

	// Create pending verification without message ID (simulating message still being sent)
	require.NoError(t, b.db.CreatePendingVerification(chatID, userID, []string{"cat"}, time.Now().Add(time.Minute), false, "en"))

	// Verify message ID is not set
	pv, err := b.db.GetPendingVerification(chatID, userID)
	require.NoError(t, err)
	require.NotNil(t, pv)
	assert.False(t, pv.MessageID.Valid, "message ID should not be set yet")

	// Call cleanup (should schedule delayed cleanup)
	b.cleanupVerificationState(chatID, userID)

	// Verify immediate cleanup didn't attempt to delete message
	assert.False(t, mock.deleteMessageCalled, "should not attempt immediate message deletion")

	// Simulate message being sent after cleanup
	require.NoError(t, b.db.UpdateVerificationMessageID(chatID, userID, verificationMsgID))

	// Wait for delayed cleanup to execute
	time.Sleep(11 * time.Second)

	// Verify delayed cleanup deleted the message
	assert.True(t, mock.deleteMessageCalled, "delayed cleanup should delete verification message")
	assert.Contains(t, mock.deletedMessages, verificationMsgID, "should delete verification message")
}

