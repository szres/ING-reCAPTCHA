package bot

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/szres/ing-recaptcha/internal/database"
)

const (
	passChatID      int64 = -1001
	passOtherChatID int64 = -1002
	passUserID      int64 = 4242
	passBotID       int64 = 999
)

func assertVerified(t *testing.T, b *Bot, userID int64, want bool, msg string) {
	t.Helper()
	got, err := b.db.IsUserVerified(userID)
	require.NoError(t, err)
	assert.Equal(t, want, got, msg)
}

// seedPendingChallenge stores a three-picture challenge and returns it.
func seedPendingChallenge(t *testing.T, b *Bot, chatID, userID int64, isTest bool) *database.PendingVerification {
	t.Helper()
	require.NoError(t, b.db.CreatePendingVerification(chatID, userID, []string{"cat", "dog", "bird"}, time.Now().Add(time.Minute), isTest, "en", nil))
	pv, err := b.db.GetPendingVerification(chatID, userID)
	require.NoError(t, err)
	require.NotNil(t, pv)
	return pv
}

func TestEvaluateVerification_SolvedCaptchaJoinsPassList(t *testing.T) {
	b, _ := newTestBot(t)
	pv := seedPendingChallenge(t, b, passChatID, passUserID, false)

	b.evaluateVerification(makeQuery(passUserID, passChatID), pv, []string{"cat", "dog", "bird"})

	assertVerified(t, b, passUserID, true, "solving the captcha earns the cross-chat pass")
}

func TestEvaluateVerification_FailedCaptchaStaysOffPassList(t *testing.T) {
	b, _ := newTestBot(t)
	pv := seedPendingChallenge(t, b, passChatID, passUserID, false)

	b.evaluateVerification(makeQuery(passUserID, passChatID), pv, []string{"fish", "lion", "bear"})

	assertVerified(t, b, passUserID, false, "a failed captcha earns nothing")
}

func TestEvaluateVerification_TestRunStaysOffPassList(t *testing.T) {
	b, _ := newTestBot(t)
	pv := seedPendingChallenge(t, b, passChatID, passUserID, true)

	b.evaluateVerification(makeQuery(passUserID, passChatID), pv, []string{"cat", "dog", "bird"})

	assertVerified(t, b, passUserID, false, "a /test run is not a join verification")
}

func TestHandleAdminApproveCallback_StaysOffPassList(t *testing.T) {
	b, mock := newTestBot(t)
	const adminID int64 = 7
	mock.chatMemberFn = func(tgbotapi.GetChatMemberConfig) (tgbotapi.ChatMember, error) {
		return tgbotapi.ChatMember{Status: "administrator"}, nil
	}
	pv := seedPendingChallenge(t, b, passChatID, passUserID, false)

	query := makeQuery(adminID, passChatID)
	query.Data = fmt.Sprintf("a:%d:%d", passUserID, pv.ChallengeVersion)
	b.handleAdminApproveCallback(query)

	gone, err := b.db.GetPendingVerification(passChatID, passUserID)
	require.NoError(t, err)
	require.Nil(t, gone, "the approval must have gone through for this test to mean anything")
	assertVerified(t, b, passUserID, false, "an admin letting someone in is not that user solving a captcha")
}

// provisionedBot gives the bot the admin rights handleChatMemberUpdate
// requires before it will verify anyone.
func provisionedBot(t *testing.T) (*Bot, *mockTelegramAPI) {
	t.Helper()
	b, mock := newTestBot(t)
	b.self = tgbotapi.User{ID: passBotID, UserName: "ING_reCAPTCHA_bot"}
	mock.chatMemberFn = func(cfg tgbotapi.GetChatMemberConfig) (tgbotapi.ChatMember, error) {
		if cfg.UserID == passBotID {
			return tgbotapi.ChatMember{Status: "administrator", CanRestrictMembers: true, CanDeleteMessages: true}, nil
		}
		return tgbotapi.ChatMember{Status: "member"}, nil
	}
	return b, mock
}

func freshJoin(chatID, userID int64) *tgbotapi.ChatMemberUpdated {
	return &tgbotapi.ChatMemberUpdated{
		Chat:          tgbotapi.Chat{ID: chatID, Type: "supergroup"},
		OldChatMember: tgbotapi.ChatMember{Status: "left", User: &tgbotapi.User{ID: userID}},
		NewChatMember: tgbotapi.ChatMember{Status: "member", User: &tgbotapi.User{ID: userID, FirstName: "Joiner"}},
	}
}

// mutedOnJoin reports whether the bot restricted the user's permissions.
func mutedOnJoin(mock *mockTelegramAPI, userID int64) bool {
	mock.mu.Lock()
	defer mock.mu.Unlock()
	for _, r := range mock.requested {
		rc, ok := r.(tgbotapi.RestrictChatMemberConfig)
		if ok && rc.UserID == userID && rc.Permissions != nil && !rc.Permissions.CanSendMessages {
			return true
		}
	}
	return false
}

func TestHandleChatMemberUpdate_UnverifiedUserIsMuted(t *testing.T) {
	b, mock := provisionedBot(t)

	b.handleChatMemberUpdate(freshJoin(passChatID, passUserID))

	assert.True(t, mutedOnJoin(mock, passUserID), "a user who never solved a captcha is verified on join")
}

func TestHandleChatMemberUpdate_VerifiedUserJoinsUntouched(t *testing.T) {
	b, mock := provisionedBot(t)
	require.NoError(t, b.db.MarkUserVerified(passUserID, passOtherChatID, database.VerifiedSourceCaptcha))

	b.handleChatMemberUpdate(freshJoin(passChatID, passUserID))

	mock.mu.Lock()
	requests, sends := len(mock.requested), len(mock.sent)
	mock.mu.Unlock()
	assert.Zero(t, requests, "no restriction, no kick")
	assert.Zero(t, sends, "no challenge message")
	assert.Empty(t, rawRequests(mock), "their permissions are left exactly as the group has them")
	pv, err := b.db.GetPendingVerification(passChatID, passUserID)
	require.NoError(t, err)
	assert.Nil(t, pv)
}

func TestHandleChatMemberUpdate_VerifiedUserSkipsRejoinCooldown(t *testing.T) {
	b, mock := provisionedBot(t)
	b.cfg.RejoinCooldownSeconds = 300
	require.NoError(t, b.db.RecordUserJoin(passChatID, passUserID))
	require.NoError(t, b.db.MarkUserVerified(passUserID, passOtherChatID, database.VerifiedSourceCaptcha))

	b.handleChatMemberUpdate(freshJoin(passChatID, passUserID))

	mock.mu.Lock()
	defer mock.mu.Unlock()
	assert.Empty(t, mock.requested, "a verified user rejoining is not tempbanned as a join spammer")
}

func TestHandleChatMemberUpdate_VerifiedUserLosesOldStrike(t *testing.T) {
	b, _ := provisionedBot(t)
	require.NoError(t, b.db.RecordVerificationFailure(passChatID, passUserID))
	require.NoError(t, b.db.MarkUserVerified(passUserID, passOtherChatID, database.VerifiedSourceCaptcha))

	b.handleChatMemberUpdate(freshJoin(passChatID, passUserID))

	strike, err := b.db.HasPreviousFailure(passChatID, passUserID)
	require.NoError(t, err)
	assert.False(t, strike, "a leftover strike would get them banned on their first message")
}

func TestHandleChatMemberUpdate_PassListLookupErrorVerifiesAsUsual(t *testing.T) {
	b, mock := provisionedBot(t)
	_, err := b.db.Exec("DROP TABLE verified_users")
	require.NoError(t, err)

	b.handleChatMemberUpdate(freshJoin(passChatID, passUserID))

	assert.True(t, mutedOnJoin(mock, passUserID), "when the pass list can't be read, nobody gets waved through")
}

// joinQueryRecorder captures the raw join-request API calls and answers each
// with respond(method).
type joinQueryRecorder struct {
	mu    sync.Mutex
	calls []joinQueryCall
}

type joinQueryCall struct {
	Method string
	Form   url.Values
}

func (r *joinQueryRecorder) byMethod(method string) []joinQueryCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []joinQueryCall
	for _, c := range r.calls {
		if c.Method == method {
			out = append(out, c)
		}
	}
	return out
}

func recordJoinQueryCalls(t *testing.T, respond func(method string) (int, string)) *joinQueryRecorder {
	t.Helper()
	rec := &joinQueryRecorder{}
	withJoinQueryClient(t, func(req *http.Request) (*http.Response, error) {
		method := req.URL.Path[strings.LastIndex(req.URL.Path, "/")+1:]
		body, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		form, err := url.ParseQuery(string(body))
		if err != nil {
			return nil, err
		}
		rec.mu.Lock()
		rec.calls = append(rec.calls, joinQueryCall{Method: method, Form: form})
		rec.mu.Unlock()
		status, respBody := respond(method)
		return jsonResponse(status, respBody), nil
	})
	return rec
}

func joinQueryOK(string) (int, string) { return http.StatusOK, `{"ok":true,"result":true}` }

func miniAppBot(t *testing.T) (*Bot, *mockTelegramAPI) {
	t.Helper()
	b, mock := newTestBot(t)
	b.cfg.MiniAppEnabled = true
	b.cfg.MiniAppPublicURL = "https://verify.example"
	b.cfg.MiniAppSessionTTLSeconds = 300
	return b, mock
}

func joinRequest(queryID string) *rawChatJoinRequest {
	return &rawChatJoinRequest{
		Chat:    tgbotapi.Chat{ID: passChatID, Type: "supergroup"},
		From:    tgbotapi.User{ID: passUserID, FirstName: "Joiner"},
		QueryID: queryID,
	}
}

func miniAppSessionCount(t *testing.T, b *Bot) int {
	t.Helper()
	var n int
	require.NoError(t, b.db.QueryRow("SELECT COUNT(*) FROM miniapp_sessions").Scan(&n))
	return n
}

func TestHandleChatJoinRequest_UnverifiedUserGetsMiniApp(t *testing.T) {
	b, _ := miniAppBot(t)
	rec := recordJoinQueryCalls(t, joinQueryOK)

	b.handleChatJoinRequest(joinRequest("q-1"))

	assert.Len(t, rec.byMethod("sendChatJoinRequestWebApp"), 1)
	assert.Empty(t, rec.byMethod("answerChatJoinRequestQuery"), "nobody is approved without solving")
	assert.Equal(t, 1, miniAppSessionCount(t, b))
}

func TestHandleChatJoinRequest_VerifiedUserIsApprovedByQuery(t *testing.T) {
	b, mock := miniAppBot(t)
	require.NoError(t, b.db.MarkUserVerified(passUserID, passOtherChatID, database.VerifiedSourceMiniApp))
	rec := recordJoinQueryCalls(t, joinQueryOK)

	b.handleChatJoinRequest(joinRequest("q-1"))

	answers := rec.byMethod("answerChatJoinRequestQuery")
	require.Len(t, answers, 1)
	assert.Equal(t, "q-1", answers[0].Form.Get("chat_join_request_query_id"))
	assert.Equal(t, "approve", answers[0].Form.Get("result"))
	assert.Empty(t, rec.byMethod("sendChatJoinRequestWebApp"), "no captcha is opened")
	assert.Zero(t, miniAppSessionCount(t, b))
	mock.mu.Lock()
	defer mock.mu.Unlock()
	assert.Empty(t, mock.requested, "the query-bound approve is the only call needed")
}

func TestHandleChatJoinRequest_VerifiedUserWithoutQueryIDUsesLegacyApprove(t *testing.T) {
	b, mock := miniAppBot(t)
	require.NoError(t, b.db.MarkUserVerified(passUserID, passOtherChatID, database.VerifiedSourceCaptcha))
	rec := recordJoinQueryCalls(t, joinQueryOK)

	b.handleChatJoinRequest(joinRequest(""))

	mock.mu.Lock()
	requested := append([]tgbotapi.Chattable(nil), mock.requested...)
	mock.mu.Unlock()
	require.Len(t, requested, 1)
	approve, ok := requested[0].(tgbotapi.ApproveChatJoinRequestConfig)
	require.True(t, ok, "expected approveChatJoinRequest, got %T", requested[0])
	assert.Equal(t, passChatID, approve.ChatID)
	assert.Equal(t, passUserID, approve.UserID)
	assert.Empty(t, rec.byMethod("answerChatJoinRequestQuery"))
	assert.Zero(t, miniAppSessionCount(t, b))
}

func TestHandleChatJoinRequest_FailedApproveFallsBackToMiniApp(t *testing.T) {
	b, _ := miniAppBot(t)
	require.NoError(t, b.db.MarkUserVerified(passUserID, passOtherChatID, database.VerifiedSourceCaptcha))
	rec := recordJoinQueryCalls(t, func(method string) (int, string) {
		if method == "answerChatJoinRequestQuery" {
			return http.StatusBadRequest, `{"ok":false,"error_code":400,"description":"Bad Request: QUERY_ID_INVALID"}`
		}
		return joinQueryOK(method)
	})

	b.handleChatJoinRequest(joinRequest("q-1"))

	assert.Len(t, rec.byMethod("sendChatJoinRequestWebApp"), 1, "the user can still get in by solving the captcha")
	assert.Equal(t, 1, miniAppSessionCount(t, b))
}

func TestHandleChatJoinRequest_FailedLegacyApproveFallsBackToDMSession(t *testing.T) {
	b, mock := miniAppBot(t)
	require.NoError(t, b.db.MarkUserVerified(passUserID, passOtherChatID, database.VerifiedSourceCaptcha))
	mock.requestErr = errors.New("Bad Request: HIDE_REQUESTER_MISSING")
	recordJoinQueryCalls(t, joinQueryOK)

	b.handleChatJoinRequest(joinRequest(""))

	assert.Equal(t, 1, miniAppSessionCount(t, b), "the request is still tracked so the chat's join policy resolves it")
}
