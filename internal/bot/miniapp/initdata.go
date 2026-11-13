// Package miniapp implements the pre-join verification Mini App: the HTTP
// server that serves the static frontend, the API endpoints the frontend
// calls, and the cryptographic validation of Telegram WebApp initData.
//
// The flow this package supports:
//
//  1. Bot receives chat_join_request, persists a miniapp_session, and asks
//     Telegram to open the Mini App at /v?n=<nonce>.
//  2. Mini App frontend loads, sends its initData query string (signed by
//     Telegram with the bot token) to the bot's HTTP API.
//  3. ValidateInitData verifies the HMAC, freshness, and that the user_id
//     claimed by initData matches the user bound to the nonce. If any
//     check fails, the request is rejected.
//  4. User solves the captcha, frontend POSTs the answer, server calls
//     answerChatJoinRequestQuery(..., "approve" or "decline").
package miniapp

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Telegram's WebApp data validation scheme, per
// https://core.telegram.org/bots/webapps#validating-data-received-via-the-mini-app
//
//   secret_key      = HMAC_SHA256(key="WebAppData", message=bot_token)
//   data_check_string = "\n".join(sorted(k+"="+v for k,v in fields if k != "hash"))
//   expected_hash   = hex(HMAC_SHA256(key=secret_key, message=data_check_string))
//
// Codex flagged that the prior internal plan had key/message swapped:
// the HMAC key is the LITERAL STRING "WebAppData", and the message is the
// bot token. This file is the canonical implementation; do not invert.

// MaxInitDataAgeSeconds bounds how stale Telegram's initData signature may
// be. Codex's review pointed out that auth_date freshness is the only thing
// that gates replay of a still-valid Telegram signature, so we enforce it
// even though we also bind to a single-use nonce. 10 minutes matches the
// generous end of Telegram's recommended range.
const MaxInitDataAgeSeconds = 10 * 60

// ValidateInitData verifies a Telegram WebApp initData query string.
//
// It returns the parsed user_id and auth_date on success. On failure it
// returns a structured error the caller can translate into HTTP 401/403
// without leaking which check failed (to avoid giving an oracle to an
// attacker probing the HMAC scheme).
//
// rawInitData is the verbatim value of Telegram.WebApp.initData — a
// query-string-encoded payload like
//
//	auth_date=…&hash=…&query_id=…&user=%7B%22id%22%3A123…%7D
//
// botToken is the same bot token used to sign initData; in our case the
// long-poll token.
func ValidateInitData(rawInitData, botToken string, now time.Time) (userID int64, authDate int64, err error) {
	if rawInitData == "" {
		return 0, 0, ErrMissingInitData
	}
	if botToken == "" {
		return 0, 0, errors.New("validate initdata: empty bot token")
	}

	values, err := url.ParseQuery(rawInitData)
	if err != nil {
		return 0, 0, fmt.Errorf("parse initdata: %w", err)
	}

	receivedHash := values.Get("hash")
	if receivedHash == "" {
		return 0, 0, ErrInvalidInitData
	}

	// Build the data-check-string: sorted key=value pairs joined by '\n',
	// excluding the 'hash' field. Telegram sends values URL-encoded; the
	// signed form uses the DECODED values, which is what ParseQuery gives us.
	keys := make([]string, 0, len(values))
	for k := range values {
		if k == "hash" {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(values.Get(k))
	}
	dataCheckString := b.String()

	// secret_key = HMAC_SHA256(key="WebAppData", message=bot_token).
	mac := hmac.New(sha256.New, []byte("WebAppData"))
	mac.Write([]byte(botToken))
	secretKey := mac.Sum(nil)

	// expected_hash = HMAC_SHA256(key=secret_key, message=data_check_string).
	mac = hmac.New(sha256.New, secretKey)
	mac.Write([]byte(dataCheckString))
	expected := mac.Sum(nil)

	receivedBytes, err := hex.DecodeString(receivedHash)
	if err != nil {
		return 0, 0, ErrInvalidInitData
	}
	if subtle.ConstantTimeCompare(receivedBytes, expected) != 1 {
		return 0, 0, ErrInvalidInitData
	}

	// auth_date freshness. Telegram sends Unix seconds.
	authDateStr := values.Get("auth_date")
	if authDateStr == "" {
		return 0, 0, ErrInvalidInitData
	}
	authDate, err = strconv.ParseInt(authDateStr, 10, 64)
	if err != nil {
		return 0, 0, ErrInvalidInitData
	}
	age := now.Unix() - authDate
	if age < -60 || age > MaxInitDataAgeSeconds {
		// allow up to 60s of clock skew in either direction beyond the
		// freshness window; reject anything farther.
		return 0, 0, ErrStaleInitData
	}

	// Extract user.id from the user JSON blob. We do a narrow parse — only
	// what we need — to avoid taking a dependency on Telegram's full User
	// shape.
	userJSON := values.Get("user")
	if userJSON == "" {
		return 0, 0, ErrInvalidInitData
	}
	userID, err = parseUserID(userJSON)
	if err != nil {
		return 0, 0, ErrInvalidInitData
	}

	return userID, authDate, nil
}

// parseUserID extracts only the `id` field from Telegram's user JSON without
// pulling in encoding/json on the hot path. The input is small and trusted
// in shape (Telegram-controlled), but we still validate strictly.
func parseUserID(s string) (int64, error) {
	// Find `"id":`
	const key = `"id"`
	i := strings.Index(s, key)
	if i < 0 {
		return 0, errors.New("user.id not found")
	}
	rest := s[i+len(key):]
	// Skip whitespace and the colon.
	j := 0
	for j < len(rest) && (rest[j] == ' ' || rest[j] == '\t') {
		j++
	}
	if j >= len(rest) || rest[j] != ':' {
		return 0, errors.New("user.id malformed")
	}
	j++
	for j < len(rest) && (rest[j] == ' ' || rest[j] == '\t') {
		j++
	}
	// Consume digits.
	start := j
	if j < len(rest) && rest[j] == '-' {
		j++
	}
	for j < len(rest) && rest[j] >= '0' && rest[j] <= '9' {
		j++
	}
	if j == start {
		return 0, errors.New("user.id not a number")
	}
	return strconv.ParseInt(rest[start:j], 10, 64)
}

// Sentinel errors. Callers should translate all of these to a single
// opaque HTTP error response so an attacker probing the endpoint can't
// distinguish "wrong hash" from "stale timestamp" from "missing user".
var (
	ErrMissingInitData = errors.New("missing initdata")
	ErrInvalidInitData = errors.New("invalid initdata")
	ErrStaleInitData   = errors.New("stale initdata")
)
