package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// telegram_join_query.go wraps the new Bot API methods that the v5 client
// library (frozen at v5.5.1, mid-2021) does not know about:
//
//   sendChatJoinRequestWebApp(chat_join_request_query_id, web_app_url)
//   answerChatJoinRequestQuery(chat_join_request_query_id, result)
//
// Both methods are POSTed as form-encoded requests to
// https://api.telegram.org/bot<token>/<method>. We keep them in the bot
// package and reuse Bot.token; if the lib ever catches up we can drop this
// file and switch the call sites to v5's typed configs.

// apiBaseURL is overridable for tests.
const apiBaseURL = "https://api.telegram.org/bot"

// joinQueryHTTPClient handles the new-API POSTs. The 10-second window
// Telegram enforces for chat_join_request query responses is tight, so we
// cap the request at 5s — well under the window — and let the caller decide
// what to do if it times out (almost always: log and move on, since the
// query_id will be unusable anyway).
var joinQueryHTTPClient = &http.Client{Timeout: 5 * time.Second}

// sendChatJoinRequestWebApp opens the Mini App for the user that issued the
// given chat_join_request. Must be called within 10 seconds of receiving
// the update. On success the user's Telegram client opens web_app_url as a
// Mini App; the bot then waits for the Mini App's HTTPS callback before
// approving or declining.
func (b *Bot) sendChatJoinRequestWebApp(ctx context.Context, queryID, webAppURL string) error {
	form := url.Values{}
	form.Set("chat_join_request_query_id", queryID)
	form.Set("web_app_url", webAppURL)
	return b.postBotAPI(ctx, "sendChatJoinRequestWebApp", form)
}

// sendMessageWithMiniAppButton sends an HTML-formatted message with an
// inline keyboard whose single button opens the given Mini App URL. This
// uses raw HTTP because the v5 client predates Mini App keyboard buttons
// and doesn't expose the `web_app` field on inline_keyboard buttons.
//
// chatID is the recipient (typically a user_id for DM delivery).
func (b *Bot) sendMessageWithMiniAppButton(ctx context.Context, chatID int64, text, buttonLabel, webAppURL string) error {
	if b.token == "" {
		return fmt.Errorf("sendMessage: empty bot token")
	}
	endpoint := apiBaseURL + b.token + "/sendMessage"

	body := struct {
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
	}{
		ChatID:    chatID,
		Text:      text,
		ParseMode: "HTML",
	}
	row := []struct {
		Text   string `json:"text"`
		WebApp struct {
			URL string `json:"url"`
		} `json:"web_app"`
	}{{Text: buttonLabel}}
	row[0].WebApp.URL = webAppURL
	body.ReplyMarkup.InlineKeyboard = append(body.ReplyMarkup.InlineKeyboard, row)

	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("sendMessage: marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, strings.NewReader(string(payload)))
	if err != nil {
		return fmt.Errorf("sendMessage: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := joinQueryHTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("sendMessage: do request: %w", err)
	}
	defer resp.Body.Close()
	rb, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("sendMessage: read body: %w", err)
	}
	var env struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
		ErrorCode   int    `json:"error_code"`
	}
	if err := json.Unmarshal(rb, &env); err != nil {
		return fmt.Errorf("sendMessage: parse envelope (status %d, body %q): %w", resp.StatusCode, truncate(string(rb), 200), err)
	}
	if !env.OK {
		return fmt.Errorf("sendMessage: telegram error %d: %s", env.ErrorCode, env.Description)
	}
	return nil
}

// answerChatJoinRequestQuery resolves the chat_join_request that issued
// queryID. result must be one of "approve", "decline", or "queue":
//
//   - approve: admits the user immediately. Use after Mini App success.
//   - decline: rejects the request. Use on Mini App failure / proven spam.
//   - queue:   defers to the regular approval queue (admins decide).
//
// Per Codex's review, prefer this over approveChatJoinRequest(chat_id,
// user_id) for the Mini App flow because the new API ties the action to
// the specific query_id and gives Telegram unambiguous signal that the
// query has been answered.
func (b *Bot) answerChatJoinRequestQuery(ctx context.Context, queryID, result string) error {
	switch result {
	case "approve", "decline", "queue":
	default:
		return fmt.Errorf("answerChatJoinRequestQuery: invalid result %q", result)
	}
	form := url.Values{}
	form.Set("chat_join_request_query_id", queryID)
	form.Set("result", result)
	return b.postBotAPI(ctx, "answerChatJoinRequestQuery", form)
}

// postBotAPI is a generic form-encoded POST to a Bot API method, decoding
// the standard {ok, description, result} envelope. It returns an error with
// the API's description text on ok=false so the caller can log it.
func (b *Bot) postBotAPI(ctx context.Context, method string, form url.Values) error {
	if b.token == "" {
		return fmt.Errorf("%s: empty bot token", method)
	}
	endpoint := apiBaseURL + b.token + "/" + method
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("%s: build request: %w", method, err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := joinQueryHTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("%s: do request: %w", method, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("%s: read body: %w", method, err)
	}

	var env struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
		ErrorCode   int    `json:"error_code"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return fmt.Errorf("%s: parse envelope (status %d, body %q): %w", method, resp.StatusCode, truncate(string(body), 200), err)
	}
	if !env.OK {
		return fmt.Errorf("%s: telegram error %d: %s", method, env.ErrorCode, env.Description)
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
