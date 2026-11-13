package bot

import (
	"testing"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func langCommandMsg() *tgbotapi.Message {
	return &tgbotapi.Message{
		From: &tgbotapi.User{ID: 77, FirstName: "Lang"},
		Chat: &tgbotapi.Chat{ID: 77, Type: "private"},
	}
}

func TestCmdSetLang_ShowSetAndRejectUnsupported(t *testing.T) {
	b, mock := newTestBot(t)
	msg := langCommandMsg()

	b.cmdSetLang(msg, "")
	reply := lastSentMessage(t, mock)
	assert.Equal(t, "HTML", reply.ParseMode)
	assert.Contains(t, reply.Text, "Current language")
	assert.Contains(t, reply.Text, "<b>en</b>")

	b.cmdSetLang(msg, "zh-TW")
	lang, err := b.db.GetUserLanguage(77)
	require.NoError(t, err)
	assert.Equal(t, "zh", lang)

	b, mock = newTestBot(t)
	b.cmdSetLang(msg, "zz")
	reply = lastSentMessage(t, mock)
	assert.Contains(t, reply.Text, "Unsupported language")
	assert.Contains(t, reply.Text, "Supported languages: en, zh")
}

func TestSupportedLanguagesList(t *testing.T) {
	b, _ := newTestBot(t)
	assert.Equal(t, "en, zh", b.supportedLanguagesList())
}
