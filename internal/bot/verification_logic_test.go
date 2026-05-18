package bot

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// makeQuery builds a minimal CallbackQuery for a given user and chat.
func makeQuery(userID int64, chatID int64) *tgbotapi.CallbackQuery {
	return &tgbotapi.CallbackQuery{
		ID:   "test-query",
		From: &tgbotapi.User{ID: userID, FirstName: "Test"},
		Message: &tgbotapi.Message{
			MessageID: 1,
			Chat:      &tgbotapi.Chat{ID: chatID},
		},
	}
}

func TestBuildVerificationKeyboard(t *testing.T) {
	b, _ := newTestBot(t)

	t.Run("3 options produce 2 rows (2+1)", func(t *testing.T) {
		opts := []string{"cat", "dog", "bird"}
		kb := b.buildVerificationKeyboard(100, 0, 0, opts, "en", false, false)
		require.Len(t, kb.InlineKeyboard, 2)
		assert.Len(t, kb.InlineKeyboard[0], 2)
		assert.Len(t, kb.InlineKeyboard[1], 1)
	})

	t.Run("4 options produce 2 rows of 2", func(t *testing.T) {
		opts := []string{"cat", "dog", "bird", "fish"}
		kb := b.buildVerificationKeyboard(100, 0, 0, opts, "en", false, false)
		require.Len(t, kb.InlineKeyboard, 2)
		assert.Len(t, kb.InlineKeyboard[0], 2)
		assert.Len(t, kb.InlineKeyboard[1], 2)
	})

	t.Run("6 options produce 3 rows of 2", func(t *testing.T) {
		opts := []string{"cat", "dog", "bird", "fish", "lion", "bear"}
		kb := b.buildVerificationKeyboard(100, 0, 0, opts, "en", false, false)
		require.Len(t, kb.InlineKeyboard, 3)
		assert.Len(t, kb.InlineKeyboard[0], 2)
		assert.Len(t, kb.InlineKeyboard[1], 2)
		assert.Len(t, kb.InlineKeyboard[2], 2)
	})

	t.Run("callback data format is v:{userID}:{ver}:{step}:{label}", func(t *testing.T) {
		opts := []string{"cat"}
		kb := b.buildVerificationKeyboard(42, 7, 3, opts, "en", false, false)
		require.Len(t, kb.InlineKeyboard, 1)
		require.Len(t, kb.InlineKeyboard[0], 1)
		btn := kb.InlineKeyboard[0][0]
		require.NotNil(t, btn.CallbackData)
		assert.Equal(t, "v:42:3:7:cat", *btn.CallbackData)
	})

	t.Run("step and version are encoded correctly in all buttons", func(t *testing.T) {
		opts := []string{"cat", "dog", "bird"}
		kb := b.buildVerificationKeyboard(99, 2, 5, opts, "en", false, false)
		for _, row := range kb.InlineKeyboard {
			for _, btn := range row {
				require.NotNil(t, btn.CallbackData)
				assert.True(t, strings.HasPrefix(*btn.CallbackData, "v:99:5:2:"),
					"expected prefix v:99:5:2: in %q", *btn.CallbackData)
			}
		}
	})

	t.Run("regenerate row is appended when showRegenerate is true", func(t *testing.T) {
		opts := []string{"cat", "dog"}
		kb := b.buildVerificationKeyboard(7, 0, 0, opts, "en", true, false)
		require.Len(t, kb.InlineKeyboard, 2)
		require.Len(t, kb.InlineKeyboard[1], 1)
		btn := kb.InlineKeyboard[1][0]
		require.NotNil(t, btn.CallbackData)
		assert.Equal(t, "r:7:0", *btn.CallbackData)
	})

	t.Run("regenerate row is omitted when showRegenerate is false", func(t *testing.T) {
		opts := []string{"cat", "dog"}
		kb := b.buildVerificationKeyboard(7, 0, 0, opts, "en", false, false)
		require.Len(t, kb.InlineKeyboard, 1)
	})

	t.Run("admin controls row is appended with two buttons when showAdminControls is true", func(t *testing.T) {
		opts := []string{"cat", "dog"}
		kb := b.buildVerificationKeyboard(7, 0, 5, opts, "en", false, true)
		require.Len(t, kb.InlineKeyboard, 2)
		require.Len(t, kb.InlineKeyboard[1], 2)
		approveBtn := kb.InlineKeyboard[1][0]
		banBtn := kb.InlineKeyboard[1][1]
		require.NotNil(t, approveBtn.CallbackData)
		require.NotNil(t, banBtn.CallbackData)
		assert.Equal(t, "a:7:5", *approveBtn.CallbackData)
		assert.Equal(t, "b:7:5", *banBtn.CallbackData)
	})

	t.Run("regenerate and admin rows can coexist (regenerate first)", func(t *testing.T) {
		opts := []string{"cat", "dog"}
		kb := b.buildVerificationKeyboard(7, 0, 0, opts, "en", true, true)
		require.Len(t, kb.InlineKeyboard, 3)
		require.Len(t, kb.InlineKeyboard[1], 1) // regenerate row
		require.Len(t, kb.InlineKeyboard[2], 2) // admin row
	})
}

func TestPickLocalizedLabel(t *testing.T) {
	tests := []struct {
		name     string
		labels   map[string]string
		userLang string
		fallback string
		want     string
	}{
		{
			name:     "user lang present",
			labels:   map[string]string{"zh": "猫", "en": "Cat"},
			userLang: "en",
			fallback: "cat",
			want:     "Cat",
		},
		{
			name:     "user lang zh",
			labels:   map[string]string{"zh": "猫", "en": "Cat"},
			userLang: "zh",
			fallback: "cat",
			want:     "猫",
		},
		{
			name:     "user lang missing falls back to raw label",
			labels:   map[string]string{"zh": "猫"},
			userLang: "en",
			fallback: "cat",
			want:     "cat",
		},
		{
			name:     "empty labels returns fallback",
			labels:   map[string]string{},
			userLang: "en",
			fallback: "cat",
			want:     "cat",
		},
		{
			name:     "empty value treated as missing",
			labels:   map[string]string{"en": ""},
			userLang: "en",
			fallback: "cat",
			want:     "cat",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, pickLocalizedLabel(tc.labels, tc.userLang, tc.fallback))
		})
	}
}

func TestEvaluateVerification(t *testing.T) {
	const (
		chatID int64 = 200
		userID int64 = 100
	)

	correctLabels := []string{"cat", "dog", "bird"}

	setup := func(t *testing.T) (*Bot, *mockTelegramAPI) {
		t.Helper()
		b, mock := newTestBot(t)
		// VerifyRequiredCorrect is 2 (set in newTestBot)
		err := b.db.CreatePendingVerification(chatID, userID, correctLabels, time.Now().Add(time.Minute), false, "en")
		require.NoError(t, err)
		return b, mock
	}

	t.Run("all correct answers trigger success path", func(t *testing.T) {
		b, mock := setup(t)
		query := makeQuery(userID, chatID)
		pv, err := b.db.GetPendingVerification(chatID, userID)
		require.NoError(t, err)
		require.NotNil(t, pv)

		// All answers match correct labels positionally
		b.evaluateVerification(query, pv, []string{"cat", "dog", "bird"})

		// unrestrictUser calls Request; verify at least one Request was made
		mock.mu.Lock()
		reqCount := len(mock.requested)
		mock.mu.Unlock()
		assert.Greater(t, reqCount, 0, "expected at least one API request for unrestrict/delete")

		// Record should be deleted
		got, err := b.db.GetPendingVerification(chatID, userID)
		require.NoError(t, err)
		assert.Nil(t, got, "pending verification should be deleted after evaluation")
	})

	t.Run("all wrong answers trigger failure path (kick called)", func(t *testing.T) {
		b, mock := setup(t)
		query := makeQuery(userID, chatID)
		pv, err := b.db.GetPendingVerification(chatID, userID)
		require.NoError(t, err)
		require.NotNil(t, pv)

		// No answers match
		b.evaluateVerification(query, pv, []string{"fish", "lion", "bear"})

		mock.mu.Lock()
		reqCount := len(mock.requested)
		mock.mu.Unlock()
		assert.Greater(t, reqCount, 0, "expected API requests for kick/delete on failure")

		got, err := b.db.GetPendingVerification(chatID, userID)
		require.NoError(t, err)
		assert.Nil(t, got)
	})

	t.Run("2 of 3 correct at threshold 2 passes", func(t *testing.T) {
		b, mock := setup(t)
		// VerifyRequiredCorrect = 2
		query := makeQuery(userID, chatID)
		pv, err := b.db.GetPendingVerification(chatID, userID)
		require.NoError(t, err)
		require.NotNil(t, pv)

		// First two match, third does not
		b.evaluateVerification(query, pv, []string{"cat", "dog", "wrong"})

		// Success path: ClearVerificationFailure should have been called (no failure recorded)
		hasFail, err := b.db.HasPreviousFailure(chatID, userID)
		require.NoError(t, err)
		assert.False(t, hasFail, "no failure should be recorded after passing")

		mock.mu.Lock()
		reqCount := len(mock.requested)
		mock.mu.Unlock()
		assert.Greater(t, reqCount, 0)
	})

	t.Run("1 of 3 correct at threshold 2 fails", func(t *testing.T) {
		b, _ := setup(t)
		query := makeQuery(userID, chatID)
		pv, err := b.db.GetPendingVerification(chatID, userID)
		require.NoError(t, err)
		require.NotNil(t, pv)

		// Only first matches
		b.evaluateVerification(query, pv, []string{"cat", "wrong", "wrong"})

		// Failure path: a failure record should have been written
		hasFail, err := b.db.HasPreviousFailure(chatID, userID)
		require.NoError(t, err)
		assert.True(t, hasFail, "failure should be recorded after failing below threshold")
	})

	t.Run("pending verification is deleted after evaluation", func(t *testing.T) {
		b, _ := setup(t)
		query := makeQuery(userID, chatID)
		pv, err := b.db.GetPendingVerification(chatID, userID)
		require.NoError(t, err)
		require.NotNil(t, pv)

		b.evaluateVerification(query, pv, []string{"cat", "dog", "bird"})

		got, err := b.db.GetPendingVerification(chatID, userID)
		require.NoError(t, err)
		assert.Nil(t, got, fmt.Sprintf("record for user %d in chat %d should be gone", userID, chatID))
	})
}

func TestHandleVerificationFailure_KickError_DoesNotRecordFailure(t *testing.T) {
	b, mock := newTestBot(t)
	mock.requestErr = errors.New("telegram: bad request: user not found")

	const (
		chatID int64 = 200
		userID int64 = 100
	)

	require.NoError(t, b.db.CreatePendingVerification(chatID, userID, []string{"cat"}, time.Now().Add(time.Minute), false, "en"))
	pv, err := b.db.GetPendingVerification(chatID, userID)
	require.NoError(t, err)
	require.NotNil(t, pv)

	query := makeQuery(userID, chatID)
	b.handleVerificationFailure(query, pv)

	hasFail, err := b.db.HasPreviousFailure(chatID, userID)
	require.NoError(t, err)
	assert.False(t, hasFail, "kick error should not poison failure history")
}
