package bot

import (
	"fmt"
	"strings"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"github.com/szres/ing-recaptcha/internal/database"
)

// cmdJoinPolicy handles `/joinpolicy [approve|decline|dismiss]` inside a
// group. It picks the FALLBACK action when a chat_join_request arrives
// without query_id AND the user doesn't /start the bot in DM within the
// 5-minute window (so the Mini App can't reach them either way).
//
// Usage:
//
//	/joinpolicy            — show current policy
//	/joinpolicy approve    — after timeout: admit + run in-group captcha (default)
//	/joinpolicy decline    — after timeout: reject the request
//	/joinpolicy dismiss    — after timeout: leave the request for admins
//
// Authorization: the caller must be either a bot admin (BOT_ADMIN_IDS) or
// an administrator/creator of the chat itself. Telegram-side group admins
// are checked via getChatMember to avoid stale local caches.
func (b *Bot) cmdJoinPolicy(msg *tgbotapi.Message, args string) {
	if msg.Chat.Type != "group" && msg.Chat.Type != "supergroup" {
		b.replyHTML(msg, "❌ /joinpolicy can only be used inside a group.")
		return
	}
	if !b.isBotAdmin(msg.From.ID) && !b.isGroupAdmin(msg.Chat.ID, msg.From.ID) {
		b.replyHTML(msg, "❌ Only group admins (or bot admins) can change the join policy.")
		return
	}

	arg := strings.ToLower(strings.TrimSpace(args))

	if arg == "" {
		current, err := b.db.GetChatJoinPolicy(msg.Chat.ID)
		if err != nil {
			b.replyHTML(msg, "❌ Failed to read current policy.")
			return
		}
		if current == "" {
			current = database.JoinPolicyApprove + " (default)"
		}
		b.replyHTML(msg, fmt.Sprintf(
			"📜 <b>Join policy</b> (fallback after the 5-min DM Mini App window expires):\n\n"+
				"Current: <code>%s</code>\n\n"+
				"When a chat_join_request arrives without query_id, the bot waits "+
				"up to 5 minutes for the user to /start it in DM and solve the "+
				"Mini App. If that doesn't happen, the bot applies this policy:\n\n"+
				"• <code>/joinpolicy approve</code> — admit and run in-group captcha (default)\n"+
				"• <code>/joinpolicy decline</code> — reject without verification\n"+
				"• <code>/joinpolicy dismiss</code> — leave pending for admins\n\n"+
				"Modern join requests that come with query_id always go straight "+
				"through the Mini App and ignore this setting.",
			current,
		))
		return
	}

	if !database.IsValidJoinPolicy(arg) {
		b.replyHTML(msg, "❌ Invalid policy. Use one of: <code>approve</code>, <code>decline</code>, <code>dismiss</code>.")
		return
	}

	if err := b.db.SetChatJoinPolicy(msg.Chat.ID, arg, msg.From.ID); err != nil {
		b.replyHTML(msg, "❌ Failed to update policy.")
		return
	}

	var note string
	switch arg {
	case database.JoinPolicyApprove:
		note = "After the 5-min DM window expires, joiners will be admitted and asked to solve the in-group captcha."
	case database.JoinPolicyDecline:
		note = "After the 5-min DM window expires, joiners will be rejected outright."
	case database.JoinPolicyDismiss:
		note = "After the 5-min DM window expires, joiners will sit in the pending queue for admins to handle."
	}
	b.replyHTML(msg, fmt.Sprintf("✅ Join policy set to <code>%s</code>.\n\n%s", arg, note))
}

// replyHTML is a tiny convenience for sending an HTML-formatted reply.
func (b *Bot) replyHTML(orig *tgbotapi.Message, text string) {
	reply := tgbotapi.NewMessage(orig.Chat.ID, text)
	reply.ParseMode = "HTML"
	reply.ReplyToMessageID = orig.MessageID
	if _, err := b.api.Send(reply); err != nil {
		// best-effort: replyHTML is used for ephemeral admin feedback
		_ = err
	}
}
