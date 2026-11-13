package bot

import (
	"strings"
	"testing"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/szres/ing-recaptcha/internal/database"
)

func commandMsg(chatType string) *tgbotapi.Message {
	return &tgbotapi.Message{
		MessageID: 10,
		From:      &tgbotapi.User{ID: 42, FirstName: "Admin"},
		Chat:      &tgbotapi.Chat{ID: -100123, Type: chatType},
	}
}

func lastSentMessage(t *testing.T, mock *mockTelegramAPI) tgbotapi.MessageConfig {
	t.Helper()
	mock.mu.Lock()
	defer mock.mu.Unlock()
	require.NotEmpty(t, mock.sent)
	msg, ok := mock.sent[len(mock.sent)-1].(tgbotapi.MessageConfig)
	require.True(t, ok, "expected MessageConfig, got %T", mock.sent[len(mock.sent)-1])
	return msg
}

func TestCmdJoinPolicy_ShowDefaultAndUpdate(t *testing.T) {
	b, mock := newTestBot(t)
	require.NoError(t, b.db.AddAdmin(42))
	msg := commandMsg("supergroup")

	b.cmdJoinPolicy(msg, "")
	reply := lastSentMessage(t, mock)
	assert.Equal(t, "HTML", reply.ParseMode)
	assert.Equal(t, 10, reply.ReplyToMessageID)
	assert.Contains(t, reply.Text, "approve (default)")

	b.cmdJoinPolicy(msg, "decline")
	policy, err := b.db.GetChatJoinPolicy(msg.Chat.ID)
	require.NoError(t, err)
	assert.Equal(t, database.JoinPolicyDecline, policy)

	reply = lastSentMessage(t, mock)
	assert.Contains(t, reply.Text, "<code>decline</code>")
	assert.Contains(t, reply.Text, "rejected outright")
}

func TestCmdJoinPolicy_RejectsInvalidContextAndPolicy(t *testing.T) {
	b, mock := newTestBot(t)
	require.NoError(t, b.db.AddAdmin(42))

	b.cmdJoinPolicy(commandMsg("private"), "approve")
	assert.Contains(t, lastSentMessage(t, mock).Text, "can only be used inside a group")

	b.cmdJoinPolicy(commandMsg("supergroup"), "ignore")
	reply := lastSentMessage(t, mock)
	assert.Contains(t, reply.Text, "Invalid policy")
	assert.True(t, strings.Contains(reply.Text, "<code>approve</code>"))
}
