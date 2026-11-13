package bot

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func withJoinQueryClient(t *testing.T, fn roundTripFunc) {
	t.Helper()
	old := joinQueryHTTPClient
	joinQueryHTTPClient = &http.Client{Transport: fn}
	t.Cleanup(func() { joinQueryHTTPClient = old })
}

func jsonResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func TestSendChatJoinRequestWebApp_PostsDocumentedFormFields(t *testing.T) {
	b := &Bot{token: "123456:ABC"}

	withJoinQueryClient(t, func(req *http.Request) (*http.Response, error) {
		assert.Equal(t, http.MethodPost, req.Method)
		assert.Equal(t, "/bot123456:ABC/sendChatJoinRequestWebApp", req.URL.Path)
		assert.Equal(t, "application/x-www-form-urlencoded", req.Header.Get("Content-Type"))

		body, err := io.ReadAll(req.Body)
		require.NoError(t, err)
		form, err := url.ParseQuery(string(body))
		require.NoError(t, err)
		assert.Equal(t, "join-query-1", form.Get("chat_join_request_query_id"))
		assert.Equal(t, "https://verify.example/v?n=nonce", form.Get("web_app_url"))
		return jsonResponse(http.StatusOK, `{"ok":true,"result":true}`), nil
	})

	err := b.sendChatJoinRequestWebApp(context.Background(), "join-query-1", "https://verify.example/v?n=nonce")
	require.NoError(t, err)
}

func TestAnswerChatJoinRequestQuery_ValidatesAndPostsResult(t *testing.T) {
	b := &Bot{token: "token"}

	for _, result := range []string{"approve", "decline", "queue"} {
		t.Run(result, func(t *testing.T) {
			withJoinQueryClient(t, func(req *http.Request) (*http.Response, error) {
				assert.Equal(t, "/bottoken/answerChatJoinRequestQuery", req.URL.Path)
				body, err := io.ReadAll(req.Body)
				require.NoError(t, err)
				form, err := url.ParseQuery(string(body))
				require.NoError(t, err)
				assert.Equal(t, "query-id", form.Get("chat_join_request_query_id"))
				assert.Equal(t, result, form.Get("result"))
				return jsonResponse(http.StatusOK, `{"ok":true}`), nil
			})

			require.NoError(t, b.answerChatJoinRequestQuery(context.Background(), "query-id", result))
		})
	}

	err := b.answerChatJoinRequestQuery(context.Background(), "query-id", "maybe")
	require.Error(t, err)
	assert.Contains(t, err.Error(), `invalid result "maybe"`)
}

func TestSendMessageWithMiniAppButton_PostsJSONKeyboard(t *testing.T) {
	b := &Bot{token: "token"}

	withJoinQueryClient(t, func(req *http.Request) (*http.Response, error) {
		assert.Equal(t, http.MethodPost, req.Method)
		assert.Equal(t, "/bottoken/sendMessage", req.URL.Path)
		assert.Equal(t, "application/json", req.Header.Get("Content-Type"))

		var payload struct {
			ChatID      int64  `json:"chat_id"`
			Text        string `json:"text"`
			ParseMode   string `json:"parse_mode"`
			ReplyMarkup struct {
				InlineKeyboard [][]struct {
					Text   string `json:"text"`
					WebApp struct {
						URL string `json:"url"`
					} `json:"web_app"`
				} `json:"inline_keyboard"`
			} `json:"reply_markup"`
		}
		require.NoError(t, json.NewDecoder(req.Body).Decode(&payload))
		assert.Equal(t, int64(9_007_199_254_740_991), payload.ChatID)
		assert.Equal(t, "<b>Open verification</b>", payload.Text)
		assert.Equal(t, "HTML", payload.ParseMode)
		require.Len(t, payload.ReplyMarkup.InlineKeyboard, 1)
		require.Len(t, payload.ReplyMarkup.InlineKeyboard[0], 1)
		assert.Equal(t, "Verify", payload.ReplyMarkup.InlineKeyboard[0][0].Text)
		assert.Equal(t, "https://verify.example/v?n=nonce", payload.ReplyMarkup.InlineKeyboard[0][0].WebApp.URL)
		return jsonResponse(http.StatusOK, `{"ok":true,"result":{"message_id":10}}`), nil
	})

	err := b.sendMessageWithMiniAppButton(
		context.Background(),
		9_007_199_254_740_991,
		"<b>Open verification</b>",
		"Verify",
		"https://verify.example/v?n=nonce",
	)
	require.NoError(t, err)
}

func TestPostBotAPI_ErrorResponses(t *testing.T) {
	b := &Bot{}
	err := b.postBotAPI(context.Background(), "method", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty bot token")

	b.token = "token"
	withJoinQueryClient(t, func(_ *http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusBadRequest, `{"ok":false,"error_code":400,"description":"bad request"}`), nil
	})
	err = b.postBotAPI(context.Background(), "method", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "telegram error 400: bad request")

	withJoinQueryClient(t, func(_ *http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusBadGateway, `not-json`), nil
	})
	err = b.postBotAPI(context.Background(), "method", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse envelope")
}

func TestTruncate(t *testing.T) {
	assert.Equal(t, "short", truncate("short", 10))
	assert.Equal(t, "abc"+string(rune(0x2026)), truncate("abcdef", 3))
}
