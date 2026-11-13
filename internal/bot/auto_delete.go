package bot

import (
	"errors"
	"log"
	"net/http"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// auto_delete.go removes the bot's "passed verification" notices from groups
// whose admins have set `/setting autodelete`. The notice is the one trace a
// successful verification leaves in the chat; in a busy group they pile up.
//
// Deadlines live in the database (scheduled_message_deletions) and a loop
// works through the ones that are due, so a notice sent just before a restart
// is still removed afterwards.

const (
	autoDeleteInterval = 5 * time.Second
	// autoDeleteBatch bounds one tick's work so a backlog (e.g. after
	// downtime) is spread over several ticks instead of bursting into
	// Telegram's rate limits.
	autoDeleteBatch = 50
	// autoDeleteGiveUp is how long past its deadline a deletion keeps being
	// retried when Telegram can't be reached.
	autoDeleteGiveUp = 10 * time.Minute

	// Bounds for `/setting autodelete`. Telegram stops letting a bot delete
	// its group messages after 48 hours, so the upper bound stays well clear.
	minPassNoticeTTL = 5 * time.Second
	maxPassNoticeTTL = 24 * time.Hour
)

// sendPassNotice posts a "passed verification" notice and, if the chat has
// autodelete set, schedules its removal.
func (b *Bot) sendPassNotice(msg tgbotapi.MessageConfig) {
	sent, err := b.api.Send(msg)
	if err != nil {
		log.Printf("[ERROR] Failed to send pass notice to chat %d: %v", msg.ChatID, err)
		return
	}

	ttl := b.resolveChatSettings(msg.ChatID).PassNoticeTTL
	if ttl <= 0 {
		return
	}
	if err := b.db.ScheduleMessageDeletion(msg.ChatID, sent.MessageID, time.Now().Add(ttl)); err != nil {
		log.Printf("[ERROR] Failed to schedule deletion of pass notice %d in chat %d: %v", sent.MessageID, msg.ChatID, err)
	}
}

func (b *Bot) autoDeleteLoop() {
	defer b.wg.Done()

	ticker := time.NewTicker(autoDeleteInterval)
	defer ticker.Stop()

	for {
		select {
		case <-b.stopChan:
			return
		case <-ticker.C:
			b.deleteDueMessages(time.Now())
		}
	}
}

// deleteDueMessages deletes the messages whose deadline has passed. A
// deletion is forgotten once Telegram has carried it out or refused it; it is
// kept for the next tick only when Telegram could not be reached or asked the
// bot to slow down, and then only until autoDeleteGiveUp.
func (b *Bot) deleteDueMessages(now time.Time) {
	due, err := b.db.GetDueMessageDeletions(now, autoDeleteBatch)
	if err != nil {
		log.Printf("[ERROR] auto-delete: %v", err)
		return
	}
	for _, d := range due {
		_, err := b.api.Request(tgbotapi.NewDeleteMessage(d.ChatID, d.MessageID))
		if err != nil && isRetryable(err) && now.Sub(d.DeleteAt) < autoDeleteGiveUp {
			log.Printf("[WARN] auto-delete: message %d in chat %d not deleted, will retry: %v", d.MessageID, d.ChatID, err)
			continue
		}
		if err != nil {
			log.Printf("[DEBUG] auto-delete: giving up on message %d in chat %d: %v", d.MessageID, d.ChatID, err)
		}
		if err := b.db.RemoveScheduledDeletion(d.ChatID, d.MessageID); err != nil {
			log.Printf("[ERROR] auto-delete: %v", err)
		}
	}
}

// isRetryable reports whether a failed Bot API call is worth repeating.
// A request Telegram understood and rejected (the message is already gone,
// too old, or the bot lost its rights) will be rejected again; a rate limit,
// a server-side error or a network failure may not be.
func isRetryable(err error) bool {
	var apiErr *tgbotapi.Error
	if !errors.As(err, &apiErr) {
		return true
	}
	return apiErr.Code == http.StatusTooManyRequests || apiErr.Code >= http.StatusInternalServerError
}
