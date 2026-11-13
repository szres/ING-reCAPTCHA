package miniapp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/szres/ing-recaptcha/internal/database"
)

func newServerTestDB(t *testing.T) *database.DB {
	t.Helper()
	db, err := database.New(":memory:")
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	require.NoError(t, db.Migrate(""))
	t.Cleanup(func() { db.Close() })
	return db
}

func validInitData(t *testing.T, userID int64) string {
	t.Helper()
	return signInitData(t, testBotToken, map[string]string{
		"auth_date": strconv.FormatInt(time.Now().Unix(), 10),
		"user":      `{"id":` + strconv.FormatInt(userID, 10) + `,"first_name":"Test"}`,
	})
}

func postJSON(t *testing.T, handler http.Handler, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	payload, err := json.Marshal(body)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func TestServerHandleInitBuildsChallengeOnce(t *testing.T) {
	db := newServerTestDB(t)
	expires := time.Now().Add(5 * time.Minute).Unix()
	_, _, err := db.CreateMiniAppSession("nonce", "query-id", -100, 42, expires, "pending")
	require.NoError(t, err)

	composeCalls := 0
	s := New("", Deps{
		DB:              db,
		BotToken:        testBotToken,
		SessionTTL:      5 * time.Minute,
		RequiredCorrect: 2,
		ComposeChallenge: func() (string, []string, []string, error) {
			composeCalls++
			return "", []string{"cat", "dog"}, []string{"cat", "dog", "bird"}, nil
		},
		CleanupTempFile: func(string) {},
	})

	req := initRequest{Nonce: "nonce", InitData: validInitData(t, 42)}
	for i := 0; i < 2; i++ {
		rec := postJSON(t, s.mux, "/v/init", req)
		require.Equal(t, http.StatusOK, rec.Code)

		var resp initResponse
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
		assert.Equal(t, "/v/image/nonce", resp.ImageURL)
		assert.Equal(t, []string{"cat", "dog", "bird"}, resp.Options)
		assert.Equal(t, 2, resp.TotalSteps)
		assert.Equal(t, 0, resp.CurrentStep)
		assert.Equal(t, expires, resp.ExpiresAtSec)
	}
	assert.Equal(t, 1, composeCalls, "challenge should be reused for repeated init calls")
}

func TestServerHandleAnswerApprovesQuerySession(t *testing.T) {
	db := newServerTestDB(t)
	expires := time.Now().Add(5 * time.Minute).Unix()
	_, _, err := db.CreateMiniAppSession("nonce", "query-id", -100, 42, expires, "pending")
	require.NoError(t, err)

	var approvedQueryID string
	var approvedChatID int64
	var approvedUserID int64
	cleaned := false
	s := New("", Deps{
		DB:              db,
		BotToken:        testBotToken,
		SessionTTL:      5 * time.Minute,
		RequiredCorrect: 2,
		ComposeChallenge: func() (string, []string, []string, error) {
			return "composed.png", []string{"cat", "dog"}, []string{"cat", "dog", "bird"}, nil
		},
		CleanupTempFile: func(path string) {
			if path == "composed.png" {
				cleaned = true
			}
		},
		ApproveJoinRequest: func(_ context.Context, queryID string) error {
			approvedQueryID = queryID
			return nil
		},
		OnApproved: func(chatID, userID int64) {
			approvedChatID = chatID
			approvedUserID = userID
		},
	})

	initData := validInitData(t, 42)
	rec := postJSON(t, s.mux, "/v/init", initRequest{Nonce: "nonce", InitData: initData})
	require.Equal(t, http.StatusOK, rec.Code)

	rec = postJSON(t, s.mux, "/v/answer", answerRequest{Nonce: "nonce", InitData: initData, Step: 0, Label: "cat"})
	require.Equal(t, http.StatusOK, rec.Code)
	var next answerResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&next))
	assert.Equal(t, "next", next.Status)
	assert.Equal(t, 1, next.CurrentStep)
	assert.Equal(t, 2, next.TotalSteps)

	rec = postJSON(t, s.mux, "/v/answer", answerRequest{Nonce: "nonce", InitData: initData, Step: 1, Label: "dog"})
	require.Equal(t, http.StatusOK, rec.Code)
	var passed answerResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&passed))
	assert.Equal(t, "passed", passed.Status)
	assert.Equal(t, "query-id", approvedQueryID)
	assert.Equal(t, int64(-100), approvedChatID)
	assert.Equal(t, int64(42), approvedUserID)
	assert.True(t, cleaned)

	sess, err := db.GetMiniAppSession("nonce")
	require.NoError(t, err)
	assert.Equal(t, "approved_prejoin", sess.Status)

	verified, err := db.IsUserVerified(42)
	require.NoError(t, err)
	assert.True(t, verified, "solving the Mini App captcha earns the cross-chat pass")
}

// solveMiniApp answers the two-step challenge the test servers compose.
func solveMiniApp(t *testing.T, s *Server, nonce string, userID int64, labels ...string) answerResponse {
	t.Helper()
	initData := validInitData(t, userID)
	rec := postJSON(t, s.mux, "/v/init", initRequest{Nonce: nonce, InitData: initData})
	require.Equal(t, http.StatusOK, rec.Code)

	var last answerResponse
	for step, label := range labels {
		rec = postJSON(t, s.mux, "/v/answer", answerRequest{Nonce: nonce, InitData: initData, Step: step, Label: label})
		require.Equal(t, http.StatusOK, rec.Code)
		last = answerResponse{}
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&last))
	}
	return last
}

func TestServerSolvedCaptchaJoinsPassListEvenWhenApproveFails(t *testing.T) {
	db := newServerTestDB(t)
	expires := time.Now().Add(5 * time.Minute).Unix()
	_, _, err := db.CreateMiniAppSession("nonce", "query-id", -100, 42, expires, "pending")
	require.NoError(t, err)

	s := New("", Deps{
		DB:              db,
		BotToken:        testBotToken,
		SessionTTL:      5 * time.Minute,
		RequiredCorrect: 2,
		ComposeChallenge: func() (string, []string, []string, error) {
			return "", []string{"cat", "dog"}, []string{"cat", "dog", "bird"}, nil
		},
		CleanupTempFile: func(string) {},
		ApproveJoinRequest: func(context.Context, string) error {
			return errors.New("Bad Request: QUERY_ID_INVALID")
		},
	})

	solveMiniApp(t, s, "nonce", 42, "cat", "dog")

	sess, err := db.GetMiniAppSession("nonce")
	require.NoError(t, err)
	require.Equal(t, "expired", sess.Status, "the approval must have failed for this test to mean anything")

	verified, err := db.IsUserVerified(42)
	require.NoError(t, err)
	assert.True(t, verified, "the user solved it; their next join request must not ask again")
}

func TestServerFailedCaptchaStaysOffPassList(t *testing.T) {
	db := newServerTestDB(t)
	expires := time.Now().Add(5 * time.Minute).Unix()
	_, _, err := db.CreateMiniAppSession("nonce", "query-id", -100, 42, expires, "pending")
	require.NoError(t, err)

	s := New("", Deps{
		DB:              db,
		BotToken:        testBotToken,
		SessionTTL:      5 * time.Minute,
		RequiredCorrect: 2,
		ComposeChallenge: func() (string, []string, []string, error) {
			return "", []string{"cat", "dog"}, []string{"cat", "dog", "bird"}, nil
		},
		CleanupTempFile:    func(string) {},
		DeclineJoinRequest: func(context.Context, string) error { return nil },
	})

	resp := solveMiniApp(t, s, "nonce", 42, "bird")
	require.Equal(t, "failed", resp.Status)

	verified, err := db.IsUserVerified(42)
	require.NoError(t, err)
	assert.False(t, verified)
}

func TestServerHandleAnswerDeclinesLegacySessionOnEarlyFailure(t *testing.T) {
	db := newServerTestDB(t)
	expires := time.Now().Add(5 * time.Minute).Unix()
	_, _, err := db.CreateMiniAppSession("legacy", "", -100, 42, expires, "pending")
	require.NoError(t, err)

	var declinedChatID int64
	var declinedUserID int64
	s := New("", Deps{
		DB:              db,
		BotToken:        testBotToken,
		SessionTTL:      5 * time.Minute,
		RequiredCorrect: 2,
		ComposeChallenge: func() (string, []string, []string, error) {
			return "", []string{"cat", "dog"}, []string{"cat", "dog", "bird"}, nil
		},
		CleanupTempFile: func(string) {},
		DeclineJoinLegacy: func(_ context.Context, chatID, userID int64) error {
			declinedChatID = chatID
			declinedUserID = userID
			return nil
		},
	})

	initData := validInitData(t, 42)
	rec := postJSON(t, s.mux, "/v/init", initRequest{Nonce: "legacy", InitData: initData})
	require.Equal(t, http.StatusOK, rec.Code)

	rec = postJSON(t, s.mux, "/v/answer", answerRequest{Nonce: "legacy", InitData: initData, Step: 0, Label: "bird"})
	require.Equal(t, http.StatusOK, rec.Code)
	var resp answerResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.Equal(t, "failed", resp.Status)
	assert.Equal(t, int64(-100), declinedChatID)
	assert.Equal(t, int64(42), declinedUserID)

	sess, err := db.GetMiniAppSession("legacy")
	require.NoError(t, err)
	assert.Equal(t, "declined", sess.Status)
}

func TestServerRoutesAndAuthErrors(t *testing.T) {
	db := newServerTestDB(t)
	s := New("", Deps{DB: db, BotToken: testBotToken})

	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	assert.Equal(t, http.StatusNoContent, rec.Code)

	rec = httptest.NewRecorder()
	s.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v", nil))
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)

	rec = postJSON(t, s.mux, "/v/init", initRequest{Nonce: "", InitData: ""})
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestGenerateNonce(t *testing.T) {
	nonce, err := GenerateNonce()
	require.NoError(t, err)
	assert.Len(t, nonce, 43)
	assert.NotContains(t, nonce, "+")
	assert.NotContains(t, nonce, "/")
	assert.NotContains(t, nonce, "=")
}
