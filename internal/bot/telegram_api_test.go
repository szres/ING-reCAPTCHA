package bot

import (
	"sync"
	"testing"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/stretchr/testify/require"

	"github.com/szres/ing-recaptcha/internal/config"
	"github.com/szres/ing-recaptcha/internal/database"
	"github.com/szres/ing-recaptcha/internal/i18n"
)

type mockTelegramAPI struct {
	mu                 sync.Mutex
	sent               []tgbotapi.Chattable
	requested          []tgbotapi.Chattable
	deletedMessages    []int
	deleteMessageCalled bool
	chatMemberFn       func(tgbotapi.GetChatMemberConfig) (tgbotapi.ChatMember, error)
	sendErr            error
	requestErr         error
}

func (m *mockTelegramAPI) Send(c tgbotapi.Chattable) (tgbotapi.Message, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sent = append(m.sent, c)
	return tgbotapi.Message{}, m.sendErr
}

func (m *mockTelegramAPI) Request(c tgbotapi.Chattable) (*tgbotapi.APIResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.requested = append(m.requested, c)

	// Track delete message requests
	if deleteMsg, ok := c.(tgbotapi.DeleteMessageConfig); ok {
		m.deleteMessageCalled = true
		m.deletedMessages = append(m.deletedMessages, deleteMsg.MessageID)
	}

	return &tgbotapi.APIResponse{Ok: true}, m.requestErr
}

func (m *mockTelegramAPI) GetChatMember(cfg tgbotapi.GetChatMemberConfig) (tgbotapi.ChatMember, error) {
	if m.chatMemberFn != nil {
		return m.chatMemberFn(cfg)
	}
	return tgbotapi.ChatMember{Status: "member"}, nil
}

func (m *mockTelegramAPI) GetFile(cfg tgbotapi.FileConfig) (tgbotapi.File, error) {
	return tgbotapi.File{}, nil
}

func (m *mockTelegramAPI) GetMe() (tgbotapi.User, error) {
	return tgbotapi.User{ID: 1, UserName: "testbot"}, nil
}

func (m *mockTelegramAPI) GetUpdatesChan(cfg tgbotapi.UpdateConfig) tgbotapi.UpdatesChannel {
	ch := make(chan tgbotapi.Update)
	close(ch)
	return ch
}

func (m *mockTelegramAPI) StopReceivingUpdates() {}

func newBotTestDB(t *testing.T) *database.DB {
	t.Helper()
	db, err := database.New(":memory:")
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	require.NoError(t, db.Migrate(""))
	t.Cleanup(func() { db.Close() })
	return db
}

func newTestBot(t *testing.T) (*Bot, *mockTelegramAPI) {
	t.Helper()
	db := newBotTestDB(t)
	tr, err := i18n.New("en")
	require.NoError(t, err)
	mock := &mockTelegramAPI{}
	b := &Bot{
		api:   mock,
		db:    db,
		token: "test-token",
		cfg: &config.Config{
			DefaultLanguage:       "en",
			VerifyImageCount:      3,
			VerifyRequiredCorrect: 2,
			VerifyDistractorCount: 2,
		},
		i18n:       tr,
		workerPool: make(chan struct{}, 5),
		updatePool: make(chan struct{}, 8),
		stopChan:   make(chan struct{}),
	}
	return b, mock
}
