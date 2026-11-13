package bot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// update_poller.go replaces the v5 client library's GetUpdatesChan when the
// Mini App (chat_join_request) flow is enabled. The reason:
//
//   v5.5.1 (mid-2021) freezes its ChatJoinRequest struct without `query_id`,
//   the field that Telegram introduced for the new Guard Bots flow. v5
//   silently drops unknown fields on unmarshal, so going through the v5
//   updates channel costs us the only handle we need to call
//   sendChatJoinRequestWebApp / answerChatJoinRequestQuery within the
//   10-second window.
//
// Telegram permits only one active getUpdates consumer per bot token, so we
// can't run two pollers side by side. This module owns the polling loop
// when MiniAppEnabled=true; the existing v5-typed handlers keep working
// because we parse into v5 types for everything that has a v5 equivalent.

// rawChatJoinRequest mirrors v5's ChatJoinRequest and adds the two fields
// v5.5.1 lacks: query_id (Guard Bots / Mini App opens) and user_chat_id
// (5-minute proactive-DM window). user_chat_id was added in Bot API 6.5
// (Feb 2023) and is the chat identifier the bot can use to DM the
// applicant within 5 minutes — even for users who have never messaged
// the bot. After the join request is processed (approved/declined) or
// the 5 minutes elapse, the identifier stops working.
//
// Important: user_chat_id and From.ID can differ — From.ID is the user's
// Telegram ID; user_chat_id is a chat identifier for messaging that user.
// For our purposes we always use user_chat_id when sending to applicants
// because From.ID may resolve to a chat the bot can't initiate.
type rawChatJoinRequest struct {
	Chat       tgbotapi.Chat            `json:"chat"`
	From       tgbotapi.User            `json:"from"`
	UserChatID int64                    `json:"user_chat_id,omitempty"`
	Date       int                      `json:"date"`
	Bio        string                   `json:"bio,omitempty"`
	InviteLink *tgbotapi.ChatInviteLink `json:"invite_link,omitempty"`
	QueryID    string                   `json:"query_id,omitempty"`
}

// rawUpdate carries the fields the bot actually uses. For everything that
// v5 already models correctly, we reuse its types verbatim; only
// ChatJoinRequest is replaced.
type rawUpdate struct {
	UpdateID        int                         `json:"update_id"`
	Message         *tgbotapi.Message           `json:"message,omitempty"`
	EditedMessage   *tgbotapi.Message           `json:"edited_message,omitempty"`
	CallbackQuery   *tgbotapi.CallbackQuery     `json:"callback_query,omitempty"`
	ChatMember      *tgbotapi.ChatMemberUpdated `json:"chat_member,omitempty"`
	MyChatMember    *tgbotapi.ChatMemberUpdated `json:"my_chat_member,omitempty"`
	ChatJoinRequest *rawChatJoinRequest         `json:"chat_join_request,omitempty"`
}

// rawPoller is a minimal getUpdates long-poller. It mirrors v5's behaviour
// closely so dropping it back into v5 later (when the lib catches up to
// query_id) is a one-line revert.
//
// Stop() cancels the poller-scoped context, which aborts any in-flight HTTP
// request immediately. Without that, Stop would only signal the goroutine
// to exit between polls — leaving us blocked up to 90s on the next request
// and creating a race window where a restarting bot has two consumers on
// the same offset (Telegram rejects this).
type rawPoller struct {
	token        string
	httpClient   *http.Client
	allowedTypes []string
	pollTimeout  int

	// dispatchJoinRequest is called SYNCHRONOUSLY for each chat_join_request
	// update, BEFORE the local offset advances. The next getUpdates call
	// uses the advanced offset, which is what acknowledges the update with
	// Telegram. Running the handler before that point means a crash here
	// leaves the update re-deliverable on restart — important because the
	// Mini App flow has only a 10s window from query_id delivery and we
	// can't afford to silently lose join requests.
	dispatchJoinRequest func(*rawChatJoinRequest)

	ctx       context.Context
	cancelCtx context.CancelFunc
}

func newRawPoller(token string, allowed []string, dispatchJoinRequest func(*rawChatJoinRequest)) *rawPoller {
	ctx, cancel := context.WithCancel(context.Background())
	return &rawPoller{
		token: token,
		// 90s matches the rest of the bot's network timeouts and stays above
		// the 60s long-poll timeout below.
		httpClient:          &http.Client{Timeout: 90 * time.Second},
		allowedTypes:        allowed,
		pollTimeout:         60,
		dispatchJoinRequest: dispatchJoinRequest,
		ctx:                 ctx,
		cancelCtx:           cancel,
	}
}

func (p *rawPoller) Stop() {
	p.cancelCtx()
	// CloseIdleConnections kicks any keep-alive sockets so a fresh client
	// (e.g. on supervised restart) doesn't reuse a half-closed connection.
	p.httpClient.CloseIdleConnections()
}

// Updates streams parsed updates until Stop is called. On any non-fatal
// transient error it logs and backs off 2s before retrying — same posture
// as v5's loop.
//
// On Stop the in-flight HTTP request is cancelled (via the poller-scoped
// context); the `request canceled` error is detected and exits cleanly
// without an extra backoff cycle.
func (p *rawPoller) Updates() <-chan rawUpdate {
	out := make(chan rawUpdate, 64)
	go func() {
		defer close(out)
		offset := 0
		allowed, _ := json.Marshal(p.allowedTypes)
		for {
			if p.ctx.Err() != nil {
				return
			}
			params := url.Values{}
			params.Set("offset", fmt.Sprintf("%d", offset))
			params.Set("timeout", fmt.Sprintf("%d", p.pollTimeout))
			if len(allowed) > 0 {
				params.Set("allowed_updates", string(allowed))
			}
			endpoint := apiBaseURL + p.token + "/getUpdates"
			req, err := http.NewRequestWithContext(p.ctx, "GET", endpoint+"?"+params.Encode(), nil)
			if err != nil {
				log.Printf("[ERROR] raw poller build request: %v", err)
				if !p.sleepOrStop(2 * time.Second) {
					return
				}
				continue
			}
			resp, err := p.httpClient.Do(req)
			if err != nil {
				if errors.Is(err, context.Canceled) || p.ctx.Err() != nil {
					return
				}
				log.Printf("[WARN] raw poller request: %v", err)
				if !p.sleepOrStop(2 * time.Second) {
					return
				}
				continue
			}
			body, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil {
				if errors.Is(err, context.Canceled) || p.ctx.Err() != nil {
					return
				}
				log.Printf("[WARN] raw poller read body: %v", err)
				if !p.sleepOrStop(2 * time.Second) {
					return
				}
				continue
			}
			var env struct {
				OK     bool        `json:"ok"`
				Desc   string      `json:"description"`
				Code   int         `json:"error_code"`
				Result []rawUpdate `json:"result"`
			}
			if err := json.Unmarshal(body, &env); err != nil {
				log.Printf("[WARN] raw poller parse: %v (body=%s)", err, truncate(string(body), 200))
				if !p.sleepOrStop(2 * time.Second) {
					return
				}
				continue
			}
			if !env.OK {
				// Telegram telling us off (e.g. wrong allowed_updates,
				// missing admin rights). Log and back off; usually fatal
				// without operator intervention, but a short backoff keeps
				// the bot from spinning at 100% CPU while they fix it.
				log.Printf("[ERROR] raw poller api error %d: %s", env.Code, env.Desc)
				if !p.sleepOrStop(5 * time.Second) {
					return
				}
				continue
			}
			for _, u := range env.Result {
				// chat_join_request: dispatch synchronously BEFORE advancing
				// the local offset so the very next getUpdates can't ack the
				// message away while a handler is still running. The handler
				// persists a miniapp_session row and calls
				// sendChatJoinRequestWebApp; both happen here on the poll
				// goroutine, then we mark the update consumed.
				if u.ChatJoinRequest != nil {
					if p.dispatchJoinRequest != nil {
						p.dispatchJoinRequest(u.ChatJoinRequest)
					}
					if u.UpdateID >= offset {
						offset = u.UpdateID + 1
					}
					continue
				}
				// Everything else fans out through the channel. We still
				// advance the offset before sending because losing a
				// callback_query or message on a crash is recoverable
				// (users retry), while losing a chat_join_request strands
				// the user in Telegram's pending queue.
				if u.UpdateID >= offset {
					offset = u.UpdateID + 1
				}
				select {
				case out <- u:
				case <-p.ctx.Done():
					return
				}
			}
		}
	}()
	return out
}

func (p *rawPoller) sleepOrStop(d time.Duration) bool {
	select {
	case <-time.After(d):
		return true
	case <-p.ctx.Done():
		return false
	}
}
