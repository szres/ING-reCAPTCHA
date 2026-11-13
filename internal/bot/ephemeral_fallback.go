package bot

import (
	"log"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"github.com/szres/ing-recaptcha/internal/database"
)

// ephemeral_fallback.go covers the case an ephemeral challenge can't cover on
// its own: the joiner never receiving it. Telegram's contract for ephemeral
// messages is "it is not guaranteed that the user will receive the message,
// especially if they are offline" — and a user is often offline at the very
// moment they are admitted, e.g. when an admin approves their join request
// some time after they sent it. With nothing on their screen the challenge
// would simply run out, and a timeout means a ban and a strike.
//
// So an ephemeral challenge that has gone EphemeralFallbackSeconds without a
// single tap is replaced by a regular group message, which Telegram does
// deliver and store, with a full fresh timeout. A joiner who has tapped
// anything has demonstrably received the ephemeral message and keeps it.
//
// The cost is that a joiner who never responds at all (most spam accounts)
// ends up with a public challenge after the grace period, as they would have
// without ephemeral mode.

const ephemeralFallbackInterval = 5 * time.Second

func (b *Bot) ephemeralFallbackLoop() {
	defer b.wg.Done()

	ticker := time.NewTicker(ephemeralFallbackInterval)
	defer ticker.Stop()

	for {
		select {
		case <-b.stopChan:
			return
		case <-ticker.C:
			b.replaceUntouchedEphemeralChallenges(time.Now())
		}
	}
}

// replaceUntouchedEphemeralChallenges handles every challenge that is due,
// one at a time on the caller's goroutine. Running them in sequence means a
// slow send can never overlap the next tick and replace the same challenge
// twice.
func (b *Bot) replaceUntouchedEphemeralChallenges(now time.Time) {
	grace := b.cfg.EphemeralFallbackSeconds
	if grace <= 0 {
		return
	}
	// A challenge sent at T expires at T+timeout, so "untouched for at least
	// grace seconds" is "expires within timeout-grace seconds from now".
	expiresBy := now.Add(time.Duration(b.cfg.VerifyTimeoutSeconds-grace) * time.Second)

	untouched, err := b.db.GetUntouchedEphemeralChallenges(now, expiresBy)
	if err != nil {
		log.Printf("[ERROR] ephemeral fallback: %v", err)
		return
	}
	for _, pv := range untouched {
		b.replaceWithRegularChallenge(pv)
	}
}

// replaceWithRegularChallenge sends a newly composed challenge as a regular
// message and then swaps it in for the ephemeral one. The message goes out
// before the swap so that a failure at any point leaves the joiner with a
// challenge they can still answer: if the send fails the ephemeral one stays
// in force and is retried on the next tick, and if the joiner taps the
// ephemeral one in the meantime the swap is refused and the regular message
// is taken down again.
func (b *Bot) replaceWithRegularChallenge(pv *database.PendingVerification) {
	chatID, userID := pv.ChatID, pv.UserID

	member, err := b.api.GetChatMember(tgbotapi.GetChatMemberConfig{
		ChatConfigWithUser: tgbotapi.ChatConfigWithUser{ChatID: chatID, UserID: userID},
	})
	if err != nil {
		log.Printf("[WARN] ephemeral fallback: member lookup failed for user %d in chat %d: %v", userID, chatID, err)
		return
	}
	if member.User == nil || member.Status == "left" || member.Status == "kicked" {
		// Gone already; the chat_member handler cleans the challenge up.
		return
	}

	composedPath, correctLabels, allOptions, err := b.composeChallenge()
	if err != nil {
		log.Printf("[ERROR] ephemeral fallback: compose failed for user %d in chat %d: %v", userID, chatID, err)
		return
	}
	defer b.composer.CleanupTempFile(composedPath)

	version := pv.ChallengeVersion + 1
	photo := tgbotapi.NewPhoto(chatID, tgbotapi.FilePath(composedPath))
	photo.Caption = b.i18n.T(pv.UserLang, "verify_welcome",
		formatUserMention(member.User),
		b.cfg.VerifyTimeoutSeconds,
		b.cfg.VerifyImageCount,
	)
	photo.ParseMode = "HTML"
	// A regular message is visible to admins, so it carries their controls.
	photo.ReplyMarkup = b.buildVerificationKeyboard(userID, 0, version, allOptions, pv.UserLang, true, true)
	photo.DisableNotification = b.cfg.VerifySilentNotifications

	sent, err := b.api.Send(photo)
	if err != nil {
		log.Printf("[ERROR] ephemeral fallback: send failed for user %d in chat %d: %v", userID, chatID, err)
		return
	}

	expiresAt := time.Now().Add(time.Duration(b.cfg.VerifyTimeoutSeconds) * time.Second)
	replaced, err := b.db.ReplaceUntouchedEphemeralChallenge(chatID, userID, pv.ChallengeVersion, correctLabels, expiresAt, allOptions, sent.MessageID)
	if err != nil || !replaced {
		if err != nil {
			log.Printf("[ERROR] ephemeral fallback: swap failed for user %d in chat %d: %v", userID, chatID, err)
		} else {
			log.Printf("[INFO] ephemeral fallback: user %d in chat %d responded to the ephemeral challenge, keeping it", userID, chatID)
		}
		if _, delErr := b.api.Request(tgbotapi.NewDeleteMessage(chatID, sent.MessageID)); delErr != nil {
			log.Printf("[WARN] ephemeral fallback: could not remove unused message %d in chat %d: %v", sent.MessageID, chatID, delErr)
		}
		return
	}

	// pv still describes the ephemeral challenge, so this removes that
	// message and any copies broadcast to admins.
	b.deleteVerificationMessageFor(pv, "ephemeral-fallback")
	log.Printf("[INFO] ephemeral fallback: challenge for user %d in chat %d untouched for %ds, re-sent as regular message %d",
		userID, chatID, b.cfg.EphemeralFallbackSeconds, sent.MessageID)
}
