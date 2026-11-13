package bot

import (
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/szres/ing-recaptcha/internal/database"
	"github.com/szres/ing-recaptcha/internal/imaging"
)

const (
	fallbackEphemeralID = 9001
	fallbackRegularID   = 555
)

// fallbackBot is a bot that can compose real challenges (it has image sets on
// disk) and whose chat still contains the joiner. Timeout is 120s and an
// ephemeral challenge may sit untouched for 30s.
func fallbackBot(t *testing.T) (*Bot, *mockTelegramAPI) {
	t.Helper()
	b, mock := newTestBot(t)
	b.cfg.VerifyTimeoutSeconds = 120
	b.cfg.EphemeralFallbackSeconds = 30

	dir := t.TempDir()
	b.cfg.ImageCachePath = dir
	b.composer = imaging.NewComposer(dir)
	for i, label := range []string{"cat", "dog", "bird", "fish", "lion", "bear"} {
		path := filepath.Join(dir, label+".png")
		img := image.NewRGBA(image.Rect(0, 0, 32, 32))
		for p := range img.Pix {
			img.Pix[p] = uint8(40 * (i + 1))
		}
		img.Set(0, 0, color.RGBA{A: 255})
		f, err := os.Create(path)
		require.NoError(t, err)
		require.NoError(t, png.Encode(f, img))
		require.NoError(t, f.Close())

		set, err := b.db.CreateImageSet(label)
		require.NoError(t, err)
		_, err = b.db.AddImage(set.ID, "file-"+label, path)
		require.NoError(t, err)
	}

	mock.sentMessageID = fallbackRegularID
	mock.chatMemberFn = func(cfg tgbotapi.GetChatMemberConfig) (tgbotapi.ChatMember, error) {
		return tgbotapi.ChatMember{
			Status: "restricted",
			User:   &tgbotapi.User{ID: cfg.UserID, FirstName: "Joiner"},
		}, nil
	}
	return b, mock
}

// seedEphemeralSentAgo stores an ephemeral challenge as it stands `age` after
// being sent, nobody having tapped it.
func seedEphemeralSentAgo(t *testing.T, b *Bot, age time.Duration) *database.PendingVerification {
	t.Helper()
	expiresAt := time.Now().Add(time.Duration(b.cfg.VerifyTimeoutSeconds)*time.Second - age)
	require.NoError(t, b.db.CreatePendingVerification(stepChatID, stepUserID, []string{"cat", "dog", "bird"}, expiresAt, false, "en", stepOptions))
	require.NoError(t, b.db.UpdateVerificationEphemeralMessageID(stepChatID, stepUserID, fallbackEphemeralID))
	return currentChallenge(t, b)
}

func currentChallenge(t *testing.T, b *Bot) *database.PendingVerification {
	t.Helper()
	pv, err := b.db.GetPendingVerification(stepChatID, stepUserID)
	require.NoError(t, err)
	require.NotNil(t, pv)
	return pv
}

func sentPhotos(mock *mockTelegramAPI) []tgbotapi.PhotoConfig {
	mock.mu.Lock()
	defer mock.mu.Unlock()
	var photos []tgbotapi.PhotoConfig
	for _, s := range mock.sent {
		if p, ok := s.(tgbotapi.PhotoConfig); ok {
			photos = append(photos, p)
		}
	}
	return photos
}

func deletedRegularMessages(mock *mockTelegramAPI) []int {
	mock.mu.Lock()
	defer mock.mu.Unlock()
	return append([]int(nil), mock.deletedMessages...)
}

func callbackData(t *testing.T, photo tgbotapi.PhotoConfig) []string {
	t.Helper()
	markup, ok := photo.ReplyMarkup.(tgbotapi.InlineKeyboardMarkup)
	require.True(t, ok, "challenge must carry an inline keyboard, got %T", photo.ReplyMarkup)
	var data []string
	for _, row := range markup.InlineKeyboard {
		for _, button := range row {
			require.NotNil(t, button.CallbackData)
			data = append(data, *button.CallbackData)
		}
	}
	return data
}

// deletedEphemerals maps receiver → ephemeral_message_id for every
// deleteEphemeralMessage call captured.
func deletedEphemerals(calls []capturedCall) map[int64]int {
	out := map[int64]int{}
	for _, c := range calls {
		if c.Method == "deleteEphemeralMessage" {
			out[int64(c.JSON["receiver_user_id"].(float64))] = int(c.JSON["ephemeral_message_id"].(float64))
		}
	}
	return out
}

func TestEphemeralFallback_UntouchedChallengeIsResentAsRegularMessage(t *testing.T) {
	calls := withEphemeralClient(t, okFor)
	b, mock := fallbackBot(t)
	seedEphemeralSentAgo(t, b, 40*time.Second)

	b.replaceUntouchedEphemeralChallenges(time.Now())

	photos := sentPhotos(mock)
	require.Len(t, photos, 1)
	assert.Equal(t, stepChatID, photos[0].ChatID)
	assert.Contains(t, photos[0].Caption, fmt.Sprintf("tg://user?id=%d", stepUserID), "the joiner is mentioned so they can find it")
	assert.Equal(t, "HTML", photos[0].ParseMode)

	data := callbackData(t, photos[0])
	answers := 0
	for _, d := range data {
		if strings.HasPrefix(d, fmt.Sprintf("v:%d:1:0:", stepUserID)) {
			answers++
		}
	}
	assert.Equal(t, b.cfg.VerifyImageCount+b.cfg.VerifyDistractorCount, answers, "answer buttons belong to the new challenge version")
	assert.Contains(t, data, fmt.Sprintf("r:%d:1", stepUserID), "the regenerate chance is still on offer")
	assert.Contains(t, data, fmt.Sprintf("a:%d:1", stepUserID), "a regular message is visible to admins, so it carries their controls")
	assert.Contains(t, data, fmt.Sprintf("b:%d:1", stepUserID))

	pv := currentChallenge(t, b)
	assert.Zero(t, pv.EphemeralMessageID)
	assert.Equal(t, int64(fallbackRegularID), pv.MessageID.Int64)
	assert.Equal(t, 1, pv.ChallengeVersion)
	assert.Len(t, pv.CorrectLabels, b.cfg.VerifyImageCount)
	assert.Greater(t, time.Until(pv.ExpiresAt), 110*time.Second, "the joiner gets a full fresh timeout")

	assert.Equal(t, map[int64]int{stepUserID: fallbackEphemeralID}, deletedEphemerals(*calls), "the ephemeral challenge is withdrawn")
	assert.Empty(t, deletedRegularMessages(mock))
}

func TestEphemeralFallback_RemovesAdminCopies(t *testing.T) {
	calls := withEphemeralClient(t, okFor)
	b, _ := fallbackBot(t)
	seedEphemeralSentAgo(t, b, 40*time.Second)
	const adminID int64 = 77
	require.NoError(t, b.db.SetVerificationAdminBroadcast(stepChatID, stepUserID, true))
	require.NoError(t, b.db.RecordAdminEphemeral(stepChatID, stepUserID, adminID, 9100, 0))

	b.replaceUntouchedEphemeralChallenges(time.Now())

	assert.Equal(t, map[int64]int{stepUserID: fallbackEphemeralID, adminID: 9100}, deletedEphemerals(*calls))
	left, err := b.db.ListAdminEphemerals(stepChatID, stepUserID)
	require.NoError(t, err)
	assert.Empty(t, left)
	assert.False(t, currentChallenge(t, b).AdminBroadcast)
}

func TestEphemeralFallback_LeavesChallengeAlone(t *testing.T) {
	cases := map[string]struct {
		age     time.Duration
		prepare func(t *testing.T, b *Bot, mock *mockTelegramAPI)
	}{
		"still within the grace period": {age: 20 * time.Second},
		"joiner answered a picture": {age: 40 * time.Second, prepare: func(t *testing.T, b *Bot, _ *mockTelegramAPI) {
			ok, err := b.db.UpdateVerificationStep(stepChatID, stepUserID, 0, 1, []string{"cat"}, 0)
			require.NoError(t, err)
			require.True(t, ok)
		}},
		"fallback disabled": {age: 40 * time.Second, prepare: func(_ *testing.T, b *Bot, _ *mockTelegramAPI) {
			b.cfg.EphemeralFallbackSeconds = 0
		}},
		"grace period not shorter than the timeout": {age: 100 * time.Second, prepare: func(_ *testing.T, b *Bot, _ *mockTelegramAPI) {
			b.cfg.EphemeralFallbackSeconds = b.cfg.VerifyTimeoutSeconds
		}},
		"joiner already left": {age: 40 * time.Second, prepare: func(_ *testing.T, _ *Bot, mock *mockTelegramAPI) {
			mock.chatMemberFn = func(cfg tgbotapi.GetChatMemberConfig) (tgbotapi.ChatMember, error) {
				return tgbotapi.ChatMember{Status: "left", User: &tgbotapi.User{ID: cfg.UserID}}, nil
			}
		}},
		"member lookup fails": {age: 40 * time.Second, prepare: func(_ *testing.T, _ *Bot, mock *mockTelegramAPI) {
			mock.chatMemberFn = func(tgbotapi.GetChatMemberConfig) (tgbotapi.ChatMember, error) {
				return tgbotapi.ChatMember{}, errors.New("Bad Gateway")
			}
		}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			calls := withEphemeralClient(t, okFor)
			b, mock := fallbackBot(t)
			seedEphemeralSentAgo(t, b, tc.age)
			if tc.prepare != nil {
				tc.prepare(t, b, mock)
			}
			before := currentChallenge(t, b)

			b.replaceUntouchedEphemeralChallenges(time.Now())

			assert.Empty(t, sentPhotos(mock), "no regular challenge is posted")
			assert.Empty(t, *calls, "the ephemeral challenge is not withdrawn")
			assert.Equal(t, before, currentChallenge(t, b))
		})
	}
}

func TestEphemeralFallback_RegularChallengeIsNeverTouched(t *testing.T) {
	b, mock := fallbackBot(t)
	expiresAt := time.Now().Add(60 * time.Second)
	require.NoError(t, b.db.CreatePendingVerification(stepChatID, stepUserID, []string{"cat", "dog", "bird"}, expiresAt, false, "en", stepOptions))
	require.NoError(t, b.db.UpdateVerificationMessageID(stepChatID, stepUserID, 42))

	b.replaceUntouchedEphemeralChallenges(time.Now())

	assert.Empty(t, sentPhotos(mock))
	assert.Equal(t, int64(42), currentChallenge(t, b).MessageID.Int64)
}

func TestEphemeralFallback_TapDuringTheSwapKeepsTheEphemeralChallenge(t *testing.T) {
	calls := withEphemeralClient(t, okFor)
	b, mock := fallbackBot(t)
	selected := seedEphemeralSentAgo(t, b, 40*time.Second)
	// The joiner taps after the challenge was picked for fallback but before
	// the swap: they have the ephemeral message after all.
	ok, err := b.db.UpdateVerificationStep(stepChatID, stepUserID, 0, 1, []string{"cat"}, 0)
	require.NoError(t, err)
	require.True(t, ok)
	answered := currentChallenge(t, b)

	b.replaceWithRegularChallenge(selected)

	require.Len(t, sentPhotos(mock), 1)
	assert.Equal(t, []int{fallbackRegularID}, deletedRegularMessages(mock), "the regular message it raced with is taken down")
	assert.Empty(t, deletedEphemerals(*calls), "the challenge the joiner is answering stays")
	assert.Equal(t, answered, currentChallenge(t, b))
}

func TestEphemeralFallback_FailedSendKeepsTheEphemeralChallenge(t *testing.T) {
	calls := withEphemeralClient(t, okFor)
	b, mock := fallbackBot(t)
	before := seedEphemeralSentAgo(t, b, 40*time.Second)
	mock.sendErr = errors.New("Too Many Requests: retry after 5")

	b.replaceUntouchedEphemeralChallenges(time.Now())

	assert.Empty(t, *calls, "withdrawing it would leave the joiner with nothing to answer")
	assert.Equal(t, before, currentChallenge(t, b), "still due, so the next tick tries again")
}

func TestEphemeralFallback_TapOnWithdrawnEphemeralKeyboardIsRejected(t *testing.T) {
	withEphemeralClient(t, okFor)
	b, mock := fallbackBot(t)
	seedEphemeralSentAgo(t, b, 40*time.Second)
	b.replaceUntouchedEphemeralChallenges(time.Now())
	replaced := currentChallenge(t, b)

	// Telegram doesn't guarantee the deletion reaches the client either, so
	// the old keyboard can still be tapped. Its answers belong to a picture
	// the joiner is no longer being asked about.
	b.handleAnswerCallback(ephemeralAnswerQuery(0, "cat"))

	assert.Equal(t, []string{toast(b, "verify_callback_expired")}, callbackTexts(mock))
	assert.Equal(t, replaced, currentChallenge(t, b))
}

func TestEphemeralFallback_RegularReplacementCanBeAnswered(t *testing.T) {
	withEphemeralClient(t, okFor)
	b, mock := fallbackBot(t)
	seedEphemeralSentAgo(t, b, 40*time.Second)
	b.replaceUntouchedEphemeralChallenges(time.Now())
	replaced := currentChallenge(t, b)

	query := &tgbotapi.CallbackQuery{
		ID:   "cbq",
		From: &tgbotapi.User{ID: stepUserID, FirstName: "Joiner", LanguageCode: "en"},
		Message: &tgbotapi.Message{
			MessageID: fallbackRegularID,
			Chat:      &tgbotapi.Chat{ID: stepChatID, Type: "supergroup"},
		},
		Data: fmt.Sprintf("v:%d:1:0:%s", stepUserID, replaced.Options[0]),
	}
	b.handleAnswerCallback(query)

	after := currentChallenge(t, b)
	assert.Equal(t, 1, after.CurrentStep)
	assert.Equal(t, []string{replaced.Options[0]}, after.UserAnswers)
	assert.Equal(t, 1, sentRegularCaptionEdits(mock), "the next picture is drawn with the regular edit, on the regular message")
}
