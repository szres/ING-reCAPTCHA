package bot

import (
	"context"
	"log"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"github.com/szres/ing-recaptcha/internal/bot/miniapp"
	"github.com/szres/ing-recaptcha/internal/database"
)

// chat_join_request.go owns the bot-side wiring for the Mini App pre-join
// flow and the DM-Mini-App fallback.
//
// Flow when query_id is present (modern Guard Bots path):
//   1. Mint a high-entropy nonce.
//   2. Persist a miniapp_session row (status=pending) bound to (chat_id,
//      user_id, query_id).
//   3. Call sendChatJoinRequestWebApp(query_id, <public-url>/v?n=<nonce>)
//      within Telegram's 10-second window.
//   4. The Mini App takes it from there: HMAC validation, captcha, then
//      answerChatJoinRequestQuery(approve|decline). See internal/bot/miniapp.
//
// Flow when query_id is absent (older chat / older client / no Mini App
// support on Telegram's side):
//   1. Mint a nonce.
//   2. Persist a miniapp_session row (status=awaiting_dm) bound to
//      (chat_id, user_id) with a 5-minute TTL. query_id stays empty
//      because we don't have one.
//   3. Do NOT call any Telegram approve/decline yet — leave the request
//      in the pending queue.
//   4. If the user sends /start to the bot in DM before the TTL expires,
//      the /start handler promotes the session to status=pending and
//      replies with a Mini App keyboard button. The user solves in DM and
//      we call legacy approveChatJoinRequest(chat_id, user_id) on success.
//   5. If the TTL expires without a solve, the policy sweeper applies
//      the chat's configured fallback (approve | decline | dismiss).
//
// The DM fallback is best-effort: it relies on the user knowing they need
// to /start the bot. Groups whose join UX makes that discoverable (e.g.
// pinned message in the linked channel telling users to /start the bot)
// see a high success rate; drive-by joiners typically time out and hit
// the configured fallback policy.

// handleChatJoinRequest is invoked from the update loop for every
// chat_join_request update Telegram delivers. It is safe to call when
// MiniAppEnabled is false (the caller filters before us).
func (b *Bot) handleChatJoinRequest(cjr *rawChatJoinRequest) {
	if cjr == nil {
		return
	}
	chatID := cjr.Chat.ID
	user := cjr.From

	if b.approveVerifiedJoinRequest(cjr) {
		return
	}

	if b.cfg.MiniAppPublicURL == "" {
		log.Printf("[ERROR] MiniAppEnabled=true but MINIAPP_PUBLIC_URL is empty; cannot handle chat=%d user=%d", chatID, user.ID)
		return
	}

	nonce, err := miniapp.GenerateNonce()
	if err != nil {
		log.Printf("[ERROR] chat_join_request mint nonce: %v", err)
		return
	}

	// Initial status reflects which delivery vector we'll try first:
	// modern (sendChatJoinRequestWebApp via Telegram's native popup) when
	// query_id is present, otherwise the DM fallback. The reuse path
	// below can transition between vectors when Telegram changes its mind
	// on retry (e.g. dropping query_id on a re-request after a ban/unban
	// cycle — observed in production).
	initialStatus := "pending"
	if cjr.QueryID == "" {
		initialStatus = "awaiting_dm"
	}
	expiresAt := time.Now().Add(time.Duration(b.cfg.MiniAppSessionTTLSeconds) * time.Second).Unix()
	sess, fresh, err := b.db.CreateMiniAppSession(nonce, cjr.QueryID, chatID, user.ID, expiresAt, initialStatus)
	if err != nil {
		log.Printf("[ERROR] chat_join_request persist session chat=%d user=%d: %v", chatID, user.ID, err)
		return
	}

	if !fresh {
		// A prior chat_join_request for this (chat,user) already minted a
		// session. Two situations bring us here:
		//   (a) Telegram redelivered the same update — rare, but harmless
		//       to re-engage.
		//   (b) The user tapped "Join" again because the prior popup is
		//       gone (e.g. they were banned + unbanned, or the original
		//       Mini App popup auto-dismissed).
		// In both cases we want to re-engage the user. Sync the stored
		// query_id to whatever Telegram gave us this time (could be a
		// fresh one, or empty if Telegram dropped enrichment on retry)
		// and extend the deadline so the user has a full TTL window.
		ok, rErr := b.db.RefreshSessionForRetry(sess.Nonce, cjr.QueryID, expiresAt)
		if rErr != nil {
			log.Printf("[ERROR] refresh session on retry nonce=%s: %v", sess.Nonce, rErr)
			return
		}
		if !ok {
			log.Printf("[INFO] chat_join_request retry hit non-active session (race) nonce=%s status=%s", sess.Nonce, sess.Status)
			return
		}
		// Apply the refresh to the in-memory copy too.
		sess.QueryID = cjr.QueryID
		sess.ExpiresAt = expiresAt
		log.Printf("[INFO] chat_join_request retry: refreshed session chat=%d user=%d nonce=%s prior_status=%s new_query_id=%q",
			chatID, user.ID, sess.Nonce, sess.Status, cjr.QueryID)
	}

	// Modern path: query_id is fresh → pop the Mini App via Telegram's
	// native popup within the 10-second window. This works on both fresh
	// and reused sessions.
	if cjr.QueryID != "" {
		webAppURL := b.cfg.MiniAppPublicURL + "/v?n=" + sess.Nonce
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		if err := b.sendChatJoinRequestWebApp(ctx, cjr.QueryID, webAppURL); err != nil {
			log.Printf("[ERROR] sendChatJoinRequestWebApp chat=%d user=%d: %v", chatID, user.ID, err)
			if fresh {
				if _, mErr := b.db.MarkMiniAppSession(sess.Nonce, "expired"); mErr != nil {
					log.Printf("[ERROR] mark session expired after send failure: %v", mErr)
				}
			}
			return
		}
		log.Printf("[INFO] Opened Mini App for chat=%d user=%d nonce=%s (query_id=%s, ttl=%ds, fresh=%v)",
			chatID, user.ID, sess.Nonce, cjr.QueryID, b.cfg.MiniAppSessionTTLSeconds, fresh)
		// Belt-and-suspenders: also DM the user with the same Mini App
		// button so there's a persistent entry point even if the native
		// popup is ephemeral / never rendered / dismissed too fast on
		// the user's client (observed in production: bot returns OK for
		// sendChatJoinRequestWebApp but the user reports never seeing
		// a verification prompt). The DM uses the same nonce — once
		// either button is solved, the session transitions and the
		// other button becomes inert.
		if cjr.UserChatID != 0 {
			b.tryProactiveDMDelivery(sess, cjr.UserChatID)
		}
		return
	}

	// DM fallback: no query_id (either Telegram didn't enrich, or this is
	// a retry after the original popup disappeared). Use user_chat_id —
	// the 5-minute pre-approve channel Bot API 6.5 opens for every join
	// request, even for users who've never messaged the bot.
	if cjr.UserChatID == 0 {
		// Field absent (legacy chat infrastructure predating 6.5). Falling
		// back to user.ID would hit "bot can't initiate conversation";
		// skip the proactive attempt and let /start handle it.
		log.Printf("[WARN] chat_join_request lacks user_chat_id chat=%d user=%d — skipping proactive DM, waiting for /start (nonce=%s)",
			chatID, user.ID, sess.Nonce)
		return
	}
	if fresh {
		log.Printf("[INFO] chat_join_request without query_id: chat=%d user=%d user_chat_id=%d → awaiting_dm (nonce=%s, ttl=%ds)",
			chatID, user.ID, cjr.UserChatID, sess.Nonce, b.cfg.MiniAppSessionTTLSeconds)
	}
	b.tryProactiveDMDelivery(sess, cjr.UserChatID)
}

// approveVerifiedJoinRequest admits the applicant straight away when they are
// on the global pass list, and reports whether it did. No session is created:
// the chat_member 'left → member' update that follows the approval finds the
// same pass-list entry and skips the in-group captcha too.
//
// If the approve call fails the request is left untouched and false is
// returned, so the caller carries on with the Mini App flow and the user can
// still get in by solving it. The timeout is short for that reason: the
// query_id is only good for 10 seconds and the fallback needs what is left.
func (b *Bot) approveVerifiedJoinRequest(cjr *rawChatJoinRequest) bool {
	chatID := cjr.Chat.ID
	userID := cjr.From.ID
	if !b.isVerifiedUser(userID) {
		return false
	}

	var err error
	if cjr.QueryID != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		err = b.answerChatJoinRequestQuery(ctx, cjr.QueryID, "approve")
	} else {
		_, err = b.api.Request(tgbotapi.ApproveChatJoinRequestConfig{
			ChatConfig: tgbotapi.ChatConfig{ChatID: chatID},
			UserID:     userID,
		})
	}
	if err != nil {
		log.Printf("[WARN] approve join request for verified user failed, falling back to Mini App: chat=%d user=%d: %v", chatID, userID, err)
		return false
	}
	log.Printf("[INFO] Approved join request without captcha: chat=%d user=%d — already verified in another chat", chatID, userID)
	return true
}

// tryProactiveDMDelivery attempts to DM the user a Mini App button at
// chat_join_request time, using user_chat_id (the 5-minute pre-approve
// window opened by Telegram for every join request) as the destination.
//
// On success the session is promoted awaiting_dm → pending; the Mini App
// server only accepts pending sessions on /v/init and /v/answer.
//
// On failure (extremely rare with user_chat_id — typically only when the
// applicant has blocked the bot or the field was absent on a legacy
// chat) the session stays awaiting_dm; the /start handler will pick it
// up if the user ever messages us, and the policy sweeper applies the
// chat's fallback on TTL expiry.
func (b *Bot) tryProactiveDMDelivery(sess *database.MiniAppSession, dmChatID int64) {
	groupTitle := "the group"
	if chat, err := b.api.GetChat(tgbotapi.ChatInfoConfig{ChatConfig: tgbotapi.ChatConfig{ChatID: sess.ChatID}}); err == nil && chat.Title != "" {
		groupTitle = chat.Title
	}
	webAppURL := b.cfg.MiniAppPublicURL + "/v?n=" + sess.Nonce

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	err := b.sendMessageWithMiniAppButton(
		ctx,
		dmChatID,
		"👋 To finish your request to join <b>"+escapeHTML(groupTitle)+"</b>, tap the button below and solve the verification.",
		"Open verification",
		webAppURL,
	)
	if err != nil {
		log.Printf("[INFO] proactive DM unavailable (will wait for /start or TTL): chat=%d user=%d dm_chat=%d nonce=%s reason=%v",
			sess.ChatID, sess.UserID, dmChatID, sess.Nonce, err)
		return
	}

	// Extend expires_at so the user gets a fresh solve window from "now"
	// (i.e. when the DM lands), not from when chat_join_request fired.
	// Telegram's 5-minute DM window and our session TTL are both
	// anchored to join-request arrival — if Telegram delivers the DM
	// late, the user could otherwise have only seconds to solve.
	newExpires := time.Now().Unix() + int64(b.cfg.MiniAppSessionTTLSeconds)
	if ok, mErr := b.db.PromoteAwaitingToPending(sess.Nonce, newExpires); mErr != nil {
		log.Printf("[ERROR] promote awaiting_dm → pending after proactive DM: chat=%d nonce=%s: %v",
			sess.ChatID, sess.Nonce, mErr)
	} else if !ok {
		log.Printf("[DEBUG] proactive DM delivered, session already promoted by another path: nonce=%s", sess.Nonce)
	}
	log.Printf("[INFO] proactive DM mini-app button delivered: chat=%d user=%d dm_chat=%d nonce=%s expires_at=%d",
		sess.ChatID, sess.UserID, dmChatID, sess.Nonce, newExpires)
}

// deliverAwaitingDMSessions claims every active awaiting_dm session for the
// given user and replies with a DM containing a Mini App keyboard button
// for each one. Called from /start in private chat.
//
// Per-session behaviour:
//   - Claim promotes awaiting_dm → pending AND extends expires_at by the
//     configured TTL so the user has fresh solve time from /start (Codex
//     fixes #2 + #6).
//   - On DM send failure (typically the "bot can't initiate conversation"
//     error for users we've never DMed before — though that shouldn't
//     happen at /start because /start IS the initiation), the chat's
//     fallback policy fires immediately rather than waiting for the
//     sweeper, which would never see the session again now that it's in
//     'pending' state (Codex fix #1).
//
// Multiple sessions can be in flight if a user has join requests pending in
// several groups; each gets its own DM so the user can pick. Loop bounded
// by maxDeliveries to avoid pathological pile-ups; overflow is logged so
// it's observable in production (Codex fix #4).
func (b *Bot) deliverAwaitingDMSessions(userID int64) {
	const maxDeliveries = 10
	ttlSeconds := int64(b.cfg.MiniAppSessionTTLSeconds)

	for i := 0; i < maxDeliveries; i++ {
		newExpires := time.Now().Unix() + ttlSeconds
		sess, ok, err := b.db.ClaimAwaitingDMByUser(userID, newExpires)
		if err != nil {
			log.Printf("[ERROR] claim awaiting_dm session for user=%d: %v", userID, err)
			return
		}
		if !ok {
			return
		}

		groupTitle := "the group"
		if chat, err := b.api.GetChat(tgbotapi.ChatInfoConfig{ChatConfig: tgbotapi.ChatConfig{ChatID: sess.ChatID}}); err == nil && chat.Title != "" {
			groupTitle = chat.Title
		}

		webAppURL := b.cfg.MiniAppPublicURL + "/v?n=" + sess.Nonce
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		err = b.sendMessageWithMiniAppButton(
			ctx,
			userID,
			"👋 To finish your request to join <b>"+escapeHTML(groupTitle)+"</b>, tap the button below and solve the verification.",
			"Open verification",
			webAppURL,
		)
		cancel()
		if err != nil {
			log.Printf("[ERROR] DM mini-app button to user=%d chat=%d: %v", userID, sess.ChatID, err)
			// The session is in 'pending' state (we just promoted it), but
			// the sweeper only acts on 'awaiting_dm'. If we just mark it
			// 'expired' the user's join request stays pending in Telegram
			// forever. Apply the chat's fallback policy synchronously so
			// the request gets resolved one way or the other.
			b.applyJoinPolicyFallback(sess.ChatID, sess.UserID, sess.Nonce, "dm-send-failed")
			continue
		}
		log.Printf("[INFO] DM mini-app button delivered to user=%d for chat=%d (nonce=%s, expires=%d)",
			userID, sess.ChatID, sess.Nonce, sess.ExpiresAt)
	}

	// Overflow: if there are still awaiting_dm sessions beyond the cap,
	// the user will need to send /start again. Log it so operators can
	// notice unusual pile-ups.
	if remaining, err := b.db.CountAwaitingDMByUser(userID); err == nil && remaining > 0 {
		log.Printf("[WARN] user=%d hit DM delivery cap (%d); %d awaiting_dm session(s) still pending — they will be delivered on next /start or via TTL fallback",
			userID, maxDeliveries, remaining)
	}
}
