package bot

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// telegram_ephemeral.go wraps Bot API 10.2 methods for Ephemeral Messages
// (July 14, 2026). The v5 client library predates these methods, so we go
// through raw HTTP just like telegram_join_query.go.
//
// Ephemeral messages are visible ONLY to the specified user (receiver_user_id)
// plus the bot itself inside a group chat. Regular members see nothing.
// Perfect fit for the in-group verification captcha, which currently posts
// a big photo + keyboard to the whole group even though only the joining
// user needs to see it.
//
// Ephemeral messages carry an ephemeral_message_id (not message_id — which
// is documented as 0 for these) and require the ephemeral edit/delete
// methods to modify or remove them.

// ephemeralHTTPClient is separate from joinQueryHTTPClient because the
// timeout budget is different: verification captchas send a ~1 MB photo,
// which needs more headroom than the 5-second window used for
// chat_join_request query responses.
var ephemeralHTTPClient = &http.Client{Timeout: 30 * time.Second}

// sendPhotoEphemeralResult carries the ephemeral_message_id returned from
// sendPhoto when receiver_user_id is set. The lib's Message.MessageID is
// 0 for ephemeral responses per the Bot API 10.2 spec, so we can't reuse
// that field.
type sendPhotoEphemeralResult struct {
	EphemeralMessageID int
}

// sendPhotoEphemeral posts a photo as an ephemeral message visible only to
// receiverUserID and the bot. Used by the in-group verification captcha
// when EPHEMERAL_VERIFY_ENABLED=true — hides the challenge from other
// group members, eliminating both spam and shoulder-surfing.
//
// The photo is uploaded from a local file path (multipart/form-data). The
// inline keyboard is JSON-encoded per Bot API convention. Silent-mode
// mirrors the existing photo path so unrelated members aren't buzzed by
// notifications on a message they can't even see.
func (b *Bot) sendPhotoEphemeral(
	ctx context.Context,
	chatID, receiverUserID int64,
	photoPath, caption string,
	keyboard tgbotapi.InlineKeyboardMarkup,
	disableNotification bool,
) (*sendPhotoEphemeralResult, error) {
	if b.token == "" {
		return nil, fmt.Errorf("sendPhotoEphemeral: empty bot token")
	}
	if receiverUserID == 0 {
		return nil, fmt.Errorf("sendPhotoEphemeral: receiverUserID required")
	}

	file, err := os.Open(photoPath)
	if err != nil {
		return nil, fmt.Errorf("open photo: %w", err)
	}
	defer file.Close()

	keyboardJSON, err := json.Marshal(keyboard)
	if err != nil {
		return nil, fmt.Errorf("marshal keyboard: %w", err)
	}
	// Bot API 10.3 moved receiver_user_id (and callback_query_id) off the
	// send methods into an ephemeral_message_parameters object. The flat
	// 10.2 field is no longer documented on sendPhoto.
	ephemeralParams, err := json.Marshal(struct {
		ReceiverUserID int64 `json:"receiver_user_id"`
	}{receiverUserID})
	if err != nil {
		return nil, fmt.Errorf("marshal ephemeral params: %w", err)
	}

	body := &bytes.Buffer{}
	mw := multipart.NewWriter(body)

	writeField := func(name, value string) error {
		return mw.WriteField(name, value)
	}
	for _, kv := range [][2]string{
		{"chat_id", strconv.FormatInt(chatID, 10)},
		{"ephemeral_message_parameters", string(ephemeralParams)},
		{"caption", caption},
		{"parse_mode", "HTML"},
		{"reply_markup", string(keyboardJSON)},
		{"disable_notification", strconv.FormatBool(disableNotification)},
	} {
		if err := writeField(kv[0], kv[1]); err != nil {
			return nil, fmt.Errorf("write field %s: %w", kv[0], err)
		}
	}

	fw, err := mw.CreateFormFile("photo", filepath.Base(photoPath))
	if err != nil {
		return nil, fmt.Errorf("create form file: %w", err)
	}
	if _, err := io.Copy(fw, file); err != nil {
		return nil, fmt.Errorf("copy photo bytes: %w", err)
	}
	if err := mw.Close(); err != nil {
		return nil, fmt.Errorf("close multipart: %w", err)
	}

	endpoint := apiBaseURL + b.token + "/sendPhoto"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, body)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())

	resp, err := ephemeralHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("post: %w", err)
	}
	defer resp.Body.Close()
	rb, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	var env struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
		ErrorCode   int    `json:"error_code"`
		Result      struct {
			MessageID          int `json:"message_id"`
			EphemeralMessageID int `json:"ephemeral_message_id"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rb, &env); err != nil {
		return nil, fmt.Errorf("parse envelope (status %d, body %q): %w", resp.StatusCode, truncate(string(rb), 200), err)
	}
	if !env.OK {
		return nil, fmt.Errorf("sendPhoto ephemeral (code %d): %s", env.ErrorCode, env.Description)
	}
	if env.Result.EphemeralMessageID == 0 {
		// Older-server-fallback safeguard: if the field is missing the
		// bot server responded to receiver_user_id as if it were a plain
		// send, so we got a normal message_id back. We refuse to store
		// that because subsequent deleteEphemeralMessage would then
		// silently target the wrong ID.
		return nil, fmt.Errorf("sendPhoto: response missing ephemeral_message_id (message_id=%d) — server may not support Bot API 10.2 ephemeral messages", env.Result.MessageID)
	}
	return &sendPhotoEphemeralResult{EphemeralMessageID: env.Result.EphemeralMessageID}, nil
}

// ephemeralTarget identifies one delivered ephemeral message. Bot API
// ephemeral_message_id is scoped per recipient and may be reused across
// recipients, so edit/delete always need the receiver alongside the ID.
type ephemeralTarget struct {
	ChatID             int64 `json:"chat_id"`
	ReceiverUserID     int64 `json:"receiver_user_id"`
	EphemeralMessageID int   `json:"ephemeral_message_id"`
}

func (t ephemeralTarget) validate(method string) error {
	if t.ReceiverUserID == 0 {
		return fmt.Errorf("%s: receiverUserID required", method)
	}
	if t.EphemeralMessageID == 0 {
		return fmt.Errorf("%s: ephemeralMessageID required", method)
	}
	return nil
}

// deleteEphemeralMessage removes an ephemeral message. Called on
// verification success (challenge no longer needed), timeout (cleanup),
// or user leaving the chat (invalidated). Errors — including "not found"
// for a message already gone — are returned; every caller treats them as
// non-fatal and logs at debug level.
func (b *Bot) deleteEphemeralMessage(ctx context.Context, chatID, receiverUserID int64, ephemeralMessageID int) error {
	if ephemeralMessageID == 0 {
		return nil
	}
	target := ephemeralTarget{
		ChatID:             chatID,
		ReceiverUserID:     receiverUserID,
		EphemeralMessageID: ephemeralMessageID,
	}
	if err := target.validate("deleteEphemeralMessage"); err != nil {
		return err
	}
	return b.postEphemeralJSON(ctx, "deleteEphemeralMessage", target)
}

// editEphemeralMessageCaption replaces the caption and inline keyboard of an
// ephemeral photo. Callbacks from ephemeral messages carry message_id=0, so
// the regular editMessageCaption cannot address them — the Bot API requires
// these dedicated editEphemeralMessage… methods instead.
func (b *Bot) editEphemeralMessageCaption(
	ctx context.Context,
	chatID, receiverUserID int64,
	ephemeralMessageID int,
	caption string,
	keyboard tgbotapi.InlineKeyboardMarkup,
) error {
	target := ephemeralTarget{
		ChatID:             chatID,
		ReceiverUserID:     receiverUserID,
		EphemeralMessageID: ephemeralMessageID,
	}
	if err := target.validate("editEphemeralMessageCaption"); err != nil {
		return err
	}
	return b.postEphemeralJSON(ctx, "editEphemeralMessageCaption", struct {
		ephemeralTarget
		Caption     string                        `json:"caption"`
		ParseMode   string                        `json:"parse_mode"`
		ReplyMarkup tgbotapi.InlineKeyboardMarkup `json:"reply_markup"`
	}{
		ephemeralTarget: target,
		Caption:         caption,
		ParseMode:       "HTML",
		ReplyMarkup:     keyboard,
	})
}

// postEphemeralJSON sends one JSON-bodied ephemeral edit/delete call and
// decodes Telegram's {ok, error_code, description} envelope. Every
// ephemeral edit/delete method returns plain True on success, so the
// result payload is not inspected.
func (b *Bot) postEphemeralJSON(ctx context.Context, method string, payload interface{}) error {
	if b.token == "" {
		return fmt.Errorf("%s: empty bot token", method)
	}
	buf, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("%s: marshal: %w", method, err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiBaseURL+b.token+"/"+method, bytes.NewReader(buf))
	if err != nil {
		return fmt.Errorf("%s: build request: %w", method, err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := ephemeralHTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("%s: post: %w", method, err)
	}
	defer resp.Body.Close()
	rb, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("%s: read response: %w", method, err)
	}
	var env struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
		ErrorCode   int    `json:"error_code"`
	}
	if err := json.Unmarshal(rb, &env); err != nil {
		return fmt.Errorf("%s: parse envelope (status %d, body %q): %w", method, resp.StatusCode, truncate(string(rb), 200), err)
	}
	if !env.OK {
		return fmt.Errorf("%s (code %d): %s", method, env.ErrorCode, env.Description)
	}
	return nil
}
