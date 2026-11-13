package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// capturedCall is one raw Bot API request intercepted on ephemeralHTTPClient.
type capturedCall struct {
	Method string
	JSON   map[string]interface{}
	Form   map[string]string
}

// withEphemeralClient routes ephemeralHTTPClient through a recorder that
// answers every call with respond(method). Raw calls are what the ephemeral
// helpers use, so this is the seam where the wire format is observable.
func withEphemeralClient(t *testing.T, respond func(method string) (int, string)) *[]capturedCall {
	t.Helper()
	var (
		mu    sync.Mutex
		calls []capturedCall
	)
	old := ephemeralHTTPClient
	ephemeralHTTPClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		method := req.URL.Path[strings.LastIndex(req.URL.Path, "/")+1:]
		call := capturedCall{Method: method}
		mediaType, params, _ := mime.ParseMediaType(req.Header.Get("Content-Type"))
		switch {
		case mediaType == "application/json":
			body, _ := io.ReadAll(req.Body)
			_ = json.Unmarshal(body, &call.JSON)
		case strings.HasPrefix(mediaType, "multipart/"):
			call.Form = map[string]string{}
			mr := multipart.NewReader(req.Body, params["boundary"])
			for {
				part, err := mr.NextPart()
				if err != nil {
					break
				}
				if part.FileName() == "" {
					v, _ := io.ReadAll(part)
					call.Form[part.FormName()] = string(v)
				}
			}
		}
		mu.Lock()
		calls = append(calls, call)
		mu.Unlock()
		status, body := respond(method)
		return jsonResponse(status, body), nil
	})}
	t.Cleanup(func() { ephemeralHTTPClient = old })
	return &calls
}

func okFor(method string) (int, string) {
	if method == "sendPhoto" {
		return 200, `{"ok":true,"result":{"message_id":0,"ephemeral_message_id":555}}`
	}
	return 200, `{"ok":true,"result":true}`
}

func TestEditEphemeralMessageCaption_WireFormat(t *testing.T) {
	calls := withEphemeralClient(t, okFor)
	b := &Bot{token: "123:ABC"}
	kb := tgbotapi.NewInlineKeyboardMarkup(tgbotapi.NewInlineKeyboardRow(
		tgbotapi.NewInlineKeyboardButtonData("cat", "v:7:0:1:cat"),
	))

	require.NoError(t, b.editEphemeralMessageCaption(context.Background(), -100, 7, 42, "<b>hi</b>", kb))

	require.Len(t, *calls, 1)
	c := (*calls)[0]
	assert.Equal(t, "editEphemeralMessageCaption", c.Method)
	assert.EqualValues(t, -100, c.JSON["chat_id"])
	assert.EqualValues(t, 7, c.JSON["receiver_user_id"], "receiver is required to address a per-recipient ID")
	assert.EqualValues(t, 42, c.JSON["ephemeral_message_id"])
	assert.Equal(t, "<b>hi</b>", c.JSON["caption"])
	assert.Equal(t, "HTML", c.JSON["parse_mode"])
	markup, ok := c.JSON["reply_markup"].(map[string]interface{})
	require.True(t, ok, "reply_markup must be an object")
	assert.Contains(t, fmt.Sprint(markup["inline_keyboard"]), "v:7:0:1:cat")
}

func TestEditEphemeralMessageCaption_RejectsMissingIdentifiers(t *testing.T) {
	calls := withEphemeralClient(t, okFor)
	b := &Bot{token: "123:ABC"}
	kb := tgbotapi.InlineKeyboardMarkup{}

	assert.Error(t, b.editEphemeralMessageCaption(context.Background(), -100, 0, 42, "x", kb))
	assert.Error(t, b.editEphemeralMessageCaption(context.Background(), -100, 7, 0, "x", kb))
	assert.Empty(t, *calls, "invalid targets must be rejected before any network call")
}

func TestEditEphemeralMessageCaption_SurfacesTelegramError(t *testing.T) {
	withEphemeralClient(t, func(string) (int, string) {
		return 400, `{"ok":false,"error_code":400,"description":"Bad Request: message to edit not found"}`
	})
	b := &Bot{token: "123:ABC"}
	err := b.editEphemeralMessageCaption(context.Background(), -100, 7, 42, "x", tgbotapi.InlineKeyboardMarkup{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "message to edit not found")
}

func TestDeleteEphemeralMessage_WireFormat(t *testing.T) {
	calls := withEphemeralClient(t, okFor)
	b := &Bot{token: "123:ABC"}

	require.NoError(t, b.deleteEphemeralMessage(context.Background(), -100, 7, 42))
	require.Len(t, *calls, 1)
	c := (*calls)[0]
	assert.Equal(t, "deleteEphemeralMessage", c.Method)
	assert.EqualValues(t, 7, c.JSON["receiver_user_id"])
	assert.EqualValues(t, 42, c.JSON["ephemeral_message_id"])

	// A zero ID means "nothing to delete" and must not hit the network.
	require.NoError(t, b.deleteEphemeralMessage(context.Background(), -100, 7, 0))
	assert.Len(t, *calls, 1)
}

func TestSendPhotoEphemeral_UsesEphemeralMessageParameters(t *testing.T) {
	calls := withEphemeralClient(t, okFor)
	b := &Bot{token: "123:ABC"}
	photo := filepath.Join(t.TempDir(), "c.png")
	require.NoError(t, os.WriteFile(photo, []byte("png"), 0o600))

	res, err := b.sendPhotoEphemeral(context.Background(), -100, 7, photo, "cap", tgbotapi.InlineKeyboardMarkup{}, true)
	require.NoError(t, err)
	assert.Equal(t, 555, res.EphemeralMessageID)

	require.Len(t, *calls, 1)
	form := (*calls)[0].Form
	// Bot API 10.3 moved receiver_user_id into ephemeral_message_parameters;
	// the flat field is no longer part of sendPhoto.
	_, flat := form["receiver_user_id"]
	assert.False(t, flat, "must not send the removed flat receiver_user_id field")
	var params map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(form["ephemeral_message_parameters"]), &params))
	assert.EqualValues(t, 7, params["receiver_user_id"])
}

// --- handler-level regression: the reported "stuck on picture 1" bug ---

const (
	stepChatID int64 = -1001680000000
	stepUserID int64 = 4242
)

var stepOptions = []string{"cat", "dog", "bird", "fish"}

// seedEphemeralChallenge creates a pending verification as the ephemeral
// send path leaves it: options stored with the challenge + an ephemeral ID.
func seedEphemeralChallenge(t *testing.T, b *Bot, adminBroadcast bool) {
	t.Helper()
	require.NoError(t, b.db.CreatePendingVerification(stepChatID, stepUserID, []string{"cat", "dog", "bird"}, time.Now().Add(time.Minute), false, "en", stepOptions))
	require.NoError(t, b.db.UpdateVerificationEphemeralMessageID(stepChatID, stepUserID, 9001))
	if adminBroadcast {
		require.NoError(t, b.db.SetVerificationAdminBroadcast(stepChatID, stepUserID, true))
		require.NoError(t, b.db.RecordAdminEphemeral(stepChatID, stepUserID, 77, 9100, 0))
	}
}

// ephemeralAnswerQuery mimics a tap on an ephemeral challenge: message_id is
// 0 and the message carries no reply_markup, so nothing can be scraped.
func ephemeralAnswerQuery(step int, label string) *tgbotapi.CallbackQuery {
	return &tgbotapi.CallbackQuery{
		ID:   "cbq",
		From: &tgbotapi.User{ID: stepUserID, FirstName: "Joiner", LanguageCode: "en"},
		Message: &tgbotapi.Message{
			MessageID: 0,
			Chat:      &tgbotapi.Chat{ID: stepChatID, Type: "supergroup"},
		},
		Data: fmt.Sprintf("v:%d:0:%d:%s", stepUserID, step, label),
	}
}

func callbackTexts(mock *mockTelegramAPI) []string {
	mock.mu.Lock()
	defer mock.mu.Unlock()
	var out []string
	for _, r := range mock.requested {
		if cb, ok := r.(tgbotapi.CallbackConfig); ok {
			out = append(out, cb.Text)
		}
	}
	return out
}

func sentRegularCaptionEdits(mock *mockTelegramAPI) int {
	mock.mu.Lock()
	defer mock.mu.Unlock()
	n := 0
	for _, s := range mock.sent {
		if _, ok := s.(tgbotapi.EditMessageCaptionConfig); ok {
			n++
		}
	}
	return n
}

func toast(b *Bot, key string, args ...interface{}) string {
	return b.t(&tgbotapi.User{LanguageCode: "en"}, key, args...)
}

// switchableResponder lets a test flip the ephemeral API between failing and
// succeeding mid-flow.
type switchableResponder struct {
	mu   sync.Mutex
	fail bool
}

func (r *switchableResponder) set(fail bool) { r.mu.Lock(); r.fail = fail; r.mu.Unlock() }
func (r *switchableResponder) respond(method string) (int, string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fail {
		return 400, `{"ok":false,"error_code":400,"description":"Bad Request: message to edit not found"}`
	}
	return okFor(method)
}

func TestHandleAnswerCallback_EphemeralAdvancesViaEphemeralEdit(t *testing.T) {
	calls := withEphemeralClient(t, okFor)
	b, mock := newTestBot(t)
	seedEphemeralChallenge(t, b, false)

	b.handleAnswerCallback(ephemeralAnswerQuery(0, "cat"))

	pv, err := b.db.GetPendingVerification(stepChatID, stepUserID)
	require.NoError(t, err)
	assert.Equal(t, 1, pv.CurrentStep)
	assert.Zero(t, sentRegularCaptionEdits(mock), "regular editMessageCaption cannot address message_id=0")

	require.Len(t, *calls, 1)
	c := (*calls)[0]
	assert.Equal(t, "editEphemeralMessageCaption", c.Method)
	assert.EqualValues(t, stepUserID, c.JSON["receiver_user_id"])
	assert.EqualValues(t, 9001, c.JSON["ephemeral_message_id"])

	kb := fmt.Sprint(c.JSON["reply_markup"])
	// Buttons must now encode step 1, built from the STORED options even
	// though the callback carried no keyboard to scrape.
	for _, opt := range stepOptions {
		assert.Contains(t, kb, fmt.Sprintf("v:%d:0:1:%s", stepUserID, opt))
	}
	assert.NotContains(t, kb, ":0:0:", "no button may still carry step 0")
	assert.NotContains(t, kb, fmt.Sprintf("a:%d:", stepUserID), "admin approve must not appear on an ephemeral challenge")
	assert.NotContains(t, kb, fmt.Sprintf("b:%d:", stepUserID), "admin ban must not appear on an ephemeral challenge")

	assert.Equal(t, []string{"Selected: cat"}, callbackTexts(mock))
}

func TestHandleAnswerCallback_EphemeralSecondTapIsAccepted(t *testing.T) {
	// The user-visible bug: after the first tap the second was rejected.
	// With the redraw working, step 1 is accepted.
	withEphemeralClient(t, okFor)
	b, mock := newTestBot(t)
	seedEphemeralChallenge(t, b, false)

	b.handleAnswerCallback(ephemeralAnswerQuery(0, "cat"))
	b.handleAnswerCallback(ephemeralAnswerQuery(1, "dog"))

	pv, err := b.db.GetPendingVerification(stepChatID, stepUserID)
	require.NoError(t, err)
	assert.Equal(t, 2, pv.CurrentStep)
	assert.Equal(t, []string{"cat", "dog"}, pv.UserAnswers)
	assert.Equal(t, []string{"Selected: cat", "Selected: dog"}, callbackTexts(mock))
}

// TestHandleAnswerCallback_FailedRedrawKeepsAnswerAndResyncs covers a redraw
// that fails outright: the answer stays recorded (no rollback), and the next
// tap on the still-visible old keyboard redraws the recorded step instead of
// being rejected — and is not itself recorded.
func TestHandleAnswerCallback_FailedRedrawKeepsAnswerAndResyncs(t *testing.T) {
	r := &switchableResponder{fail: true}
	calls := withEphemeralClient(t, r.respond)
	b, mock := newTestBot(t)
	seedEphemeralChallenge(t, b, false)

	b.handleAnswerCallback(ephemeralAnswerQuery(0, "cat"))
	pv, err := b.db.GetPendingVerification(stepChatID, stepUserID)
	require.NoError(t, err)
	assert.Equal(t, 1, pv.CurrentStep, "the answer stays recorded")
	assert.Equal(t, []string{"cat"}, pv.UserAnswers)
	assert.Equal(t, []string{toast(b, "verify_callback_retry")}, callbackTexts(mock))

	r.set(false)
	b.handleAnswerCallback(ephemeralAnswerQuery(0, "dog")) // tap on the stale picture-1 keyboard

	pv, err = b.db.GetPendingVerification(stepChatID, stepUserID)
	require.NoError(t, err)
	assert.Equal(t, 1, pv.CurrentStep)
	assert.Equal(t, []string{"cat"}, pv.UserAnswers, "an answer against the wrong picture must not be recorded")
	assert.Equal(t, toast(b, "verify_callback_resync"), callbackTexts(mock)[1])

	last := (*calls)[len(*calls)-1]
	assert.Equal(t, "editEphemeralMessageCaption", last.Method)
	assert.Contains(t, fmt.Sprint(last.JSON["reply_markup"]), fmt.Sprintf("v:%d:0:1:", stepUserID), "resync must redraw the recorded step")

	b.handleAnswerCallback(ephemeralAnswerQuery(1, "dog"))
	pv, err = b.db.GetPendingVerification(stepChatID, stepUserID)
	require.NoError(t, err)
	assert.Equal(t, []string{"cat", "dog"}, pv.UserAnswers, "the user continues normally after resync")
}

// TestHandleAnswerCallback_LostEditResponseNeedsNoRepair covers the ambiguous
// failure: Telegram applied the edit but the response was lost. Because the
// answer is kept rather than rolled back, screen and database already agree
// and the user's next tap on the new keyboard just works.
func TestHandleAnswerCallback_LostEditResponseNeedsNoRepair(t *testing.T) {
	r := &switchableResponder{fail: true}
	withEphemeralClient(t, r.respond)
	b, mock := newTestBot(t)
	seedEphemeralChallenge(t, b, false)

	b.handleAnswerCallback(ephemeralAnswerQuery(0, "cat")) // edit "fails" but was applied
	r.set(false)
	b.handleAnswerCallback(ephemeralAnswerQuery(1, "dog")) // user taps the picture-2 keyboard they see

	pv, err := b.db.GetPendingVerification(stepChatID, stepUserID)
	require.NoError(t, err)
	assert.Equal(t, 2, pv.CurrentStep)
	assert.Equal(t, []string{"cat", "dog"}, pv.UserAnswers)
	assert.NotContains(t, callbackTexts(mock), toast(b, "verify_callback_resync"))
}

// TestHandleAnswerCallback_ConcurrentTapsRecordOneAnswer: callbacks run
// concurrently, so several taps on the same step must record exactly one.
func TestHandleAnswerCallback_ConcurrentTapsRecordOneAnswer(t *testing.T) {
	withEphemeralClient(t, okFor)
	b, mock := newTestBot(t)
	seedEphemeralChallenge(t, b, false)

	var wg sync.WaitGroup
	for _, label := range stepOptions {
		wg.Add(1)
		go func(l string) {
			defer wg.Done()
			b.handleAnswerCallback(ephemeralAnswerQuery(0, l))
		}(label)
	}
	wg.Wait()

	pv, err := b.db.GetPendingVerification(stepChatID, stepUserID)
	require.NoError(t, err)
	assert.Equal(t, 1, pv.CurrentStep, "concurrent taps must not advance past one step")
	require.Len(t, pv.UserAnswers, 1)

	// Without a step guard every racing tap is told "Selected: X" while only
	// the last write survives, so the bot can acknowledge one label and
	// record another. Exactly one tap may be acknowledged, and it must be
	// the recorded one.
	var acked []string
	for _, txt := range callbackTexts(mock) {
		if strings.HasPrefix(txt, "Selected: ") {
			acked = append(acked, strings.TrimPrefix(txt, "Selected: "))
		}
	}
	require.Len(t, acked, 1, "only the winning tap may be acknowledged")
	assert.Equal(t, pv.UserAnswers[0], acked[0], "the acknowledged answer must be the recorded one")
}

func TestHandleAnswerCallback_NoOptionsNeverPushesEmptyKeypad(t *testing.T) {
	// Defensive: a challenge row without options must never be redrawn as a
	// keyboard with no answer buttons.
	calls := withEphemeralClient(t, okFor)
	b, mock := newTestBot(t)
	require.NoError(t, b.db.CreatePendingVerification(stepChatID, stepUserID, []string{"cat", "dog", "bird"}, time.Now().Add(time.Minute), false, "en", nil))
	require.NoError(t, b.db.UpdateVerificationEphemeralMessageID(stepChatID, stepUserID, 9001))

	b.handleAnswerCallback(ephemeralAnswerQuery(0, "cat"))

	assert.Empty(t, *calls, "must not push a keyboard with no answer buttons")
	assert.Equal(t, []string{toast(b, "verify_callback_retry")}, callbackTexts(mock))
}

func TestHandleAnswerCallback_RefreshesAdminCopies(t *testing.T) {
	calls := withEphemeralClient(t, okFor)
	b, _ := newTestBot(t)
	seedEphemeralChallenge(t, b, true)

	b.handleAnswerCallback(ephemeralAnswerQuery(0, "cat"))

	receivers := map[float64]float64{}
	for _, c := range *calls {
		require.Equal(t, "editEphemeralMessageCaption", c.Method)
		receivers[c.JSON["receiver_user_id"].(float64)] = c.JSON["ephemeral_message_id"].(float64)
	}
	assert.Equal(t, float64(9001), receivers[float64(stepUserID)], "joiner's copy")
	assert.Equal(t, float64(9100), receivers[77], "admin's copy, addressed with the admin as receiver")
}

func TestHandleAnswerCallback_AdminRefreshSkipsOtherChallengeVersion(t *testing.T) {
	// An answer handled just before a regenerate can reach the admin refresh
	// after the tracker rows were replaced by the new challenge's copies.
	// Those must not be overwritten with this (older) challenge's keyboard.
	calls := withEphemeralClient(t, okFor)
	b, _ := newTestBot(t)
	seedEphemeralChallenge(t, b, false)
	require.NoError(t, b.db.SetVerificationAdminBroadcast(stepChatID, stepUserID, true))
	require.NoError(t, b.db.RecordAdminEphemeral(stepChatID, stepUserID, 77, 9200, 1)) // version 1 copy

	b.handleAnswerCallback(ephemeralAnswerQuery(0, "cat")) // answers version 0

	for _, c := range *calls {
		assert.NotEqualValues(t, 9200, c.JSON["ephemeral_message_id"], "a newer challenge's admin copy must be left alone")
	}
}

func TestHandleAnswerCallback_RegularMessageStillUsesStandardEdit(t *testing.T) {
	calls := withEphemeralClient(t, okFor)
	b, mock := newTestBot(t)
	require.NoError(t, b.db.CreatePendingVerification(stepChatID, stepUserID, []string{"cat", "dog", "bird"}, time.Now().Add(time.Minute), false, "en", stepOptions))
	require.NoError(t, b.db.UpdateVerificationMessageID(stepChatID, stepUserID, 321))

	q := ephemeralAnswerQuery(0, "cat")
	q.Message.MessageID = 321
	b.handleAnswerCallback(q)

	assert.Equal(t, 1, sentRegularCaptionEdits(mock))
	assert.Empty(t, *calls, "non-ephemeral challenges must not use ephemeral methods")
}
