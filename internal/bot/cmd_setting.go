package bot

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// cmdSetting handles `/setting [subcommand] [value]` inside a group. It
// exposes per-group verification toggles that make sense to configure
// independently — right now:
//
//	/setting                       — show current settings
//	/setting ephemeral on|off|default   — override global EPHEMERAL_VERIFY_ENABLED
//	/setting adminview on|off      — also broadcast to group admins
//	/setting autodelete <seconds>|off   — remove the "passed verification" notice after a delay
//
// Authorization matches /joinpolicy: bot admin OR chat administrator/creator.
//
// The welcome message is intentionally NEVER ephemeral — it's a group
// event ("X passed verification in N seconds") that everyone sees
// regardless of the ephemeral toggles. Groups that don't want it to
// linger use autodelete.
func (b *Bot) cmdSetting(msg *tgbotapi.Message, args string) {
	if msg.Chat.Type != "group" && msg.Chat.Type != "supergroup" {
		b.replyHTML(msg, "❌ /setting can only be used inside a group.")
		return
	}
	// Per-group config is scoped to actual admins of THIS group. The
	// global bot-admin role (BOT_ADMIN_IDS) intentionally does NOT
	// bypass this check — it's an ops role, not a chat-membership
	// substitute. A bot operator who wants to configure a group they
	// don't administer should get added as a group admin first.
	if !b.isGroupAdmin(msg.Chat.ID, msg.From.ID) {
		b.replyHTML(msg, "❌ Only administrators of this group can change its settings.")
		return
	}

	fields := strings.Fields(strings.ToLower(strings.TrimSpace(args)))
	if len(fields) == 0 {
		b.showSettings(msg)
		return
	}

	switch fields[0] {
	case "ephemeral":
		if len(fields) < 2 {
			b.replyHTML(msg, "Usage: <code>/setting ephemeral on|off|default</code>")
			return
		}
		b.handleEphemeralSetting(msg, fields[1])
	case "adminview", "admin-view", "admin_visible":
		if len(fields) < 2 {
			b.replyHTML(msg, "Usage: <code>/setting adminview on|off</code>")
			return
		}
		b.handleAdminViewSetting(msg, fields[1])
	case "autodelete", "auto-delete", "auto_delete":
		if len(fields) < 2 {
			b.replyHTML(msg, "Usage: <code>/setting autodelete &lt;seconds&gt;|off</code>")
			return
		}
		b.handleAutoDeleteSetting(msg, fields[1])
	default:
		b.replyHTML(msg,
			"❓ Unknown setting. Available:\n\n"+
				"• <code>/setting ephemeral on|off|default</code>\n"+
				"• <code>/setting adminview on|off</code>\n"+
				"• <code>/setting autodelete &lt;seconds&gt;|off</code>",
		)
	}
}

func (b *Bot) showSettings(msg *tgbotapi.Message) {
	// Same resolver the verification path uses, so what's displayed here
	// is exactly what the bot will do.
	r := b.resolveChatSettings(msg.Chat.ID)

	ephemeralDisplay := "default (inherit)"
	if r.EphemeralExplicit {
		if r.EphemeralOn {
			ephemeralDisplay = "on"
		} else {
			ephemeralDisplay = "off"
		}
	}
	adminVisibleDisplay := "off"
	if r.AdminVisible {
		adminVisibleDisplay = "on"
	}
	effectiveDisplay := "off"
	if r.EphemeralOn {
		effectiveDisplay = "on"
	}
	autoDeleteDisplay := "off"
	if r.PassNoticeTTL > 0 {
		autoDeleteDisplay = formatPassNoticeTTL(r.PassNoticeTTL)
	}

	b.replyHTML(msg, fmt.Sprintf(
		"⚙️ <b>Group settings</b>\n\n"+
			"• <code>ephemeral</code>: <code>%s</code> (effective: <code>%s</code>)\n"+
			"    In-group captcha shown only to the joining user via Bot API 10.2\n"+
			"    ephemeral messages. Default inherits the bot-wide env config.\n\n"+
			"• <code>adminview</code>: <code>%s</code>\n"+
			"    When ephemeral is on, also broadcast the challenge to every\n"+
			"    group admin as their own ephemeral message (admins can watch\n"+
			"    attempts happen).\n\n"+
			"• <code>autodelete</code>: <code>%s</code>\n"+
			"    Remove the \"passed verification\" notice from the group this\n"+
			"    long after it is posted. Off keeps it.\n\n"+
			"Change with:\n"+
			"<code>/setting ephemeral on|off|default</code>\n"+
			"<code>/setting adminview on|off</code>\n"+
			"<code>/setting autodelete &lt;seconds&gt;|off</code>",
		ephemeralDisplay, effectiveDisplay, adminVisibleDisplay, autoDeleteDisplay,
	))
}

func (b *Bot) handleEphemeralSetting(msg *tgbotapi.Message, value string) {
	var newValue *bool
	switch value {
	case "on", "true", "1", "enable", "enabled":
		v := true
		newValue = &v
	case "off", "false", "0", "disable", "disabled":
		v := false
		newValue = &v
	case "default", "reset", "inherit", "clear":
		newValue = nil
	default:
		b.replyHTML(msg, "❌ Value must be <code>on</code>, <code>off</code>, or <code>default</code>.")
		return
	}
	if err := b.db.SetChatEphemeralVerify(msg.Chat.ID, newValue, msg.From.ID); err != nil {
		b.replyHTML(msg, "❌ Failed to update setting.")
		return
	}
	switch {
	case newValue == nil:
		b.replyHTML(msg, "✅ <code>ephemeral</code> reset to <code>default</code> — inherits the global config.")
	case *newValue:
		b.replyHTML(msg, "✅ <code>ephemeral</code> set to <code>on</code>. New verifications in this group will send the challenge as an ephemeral message visible only to the joining user.")
	default:
		b.replyHTML(msg, "✅ <code>ephemeral</code> set to <code>off</code>. Verification will use the regular public captcha.")
	}
}

func (b *Bot) handleAdminViewSetting(msg *tgbotapi.Message, value string) {
	var v bool
	switch value {
	case "on", "true", "1", "enable", "enabled":
		v = true
	case "off", "false", "0", "disable", "disabled":
		v = false
	default:
		b.replyHTML(msg, "❌ Value must be <code>on</code> or <code>off</code>.")
		return
	}
	if err := b.db.SetChatEphemeralAdminVisible(msg.Chat.ID, v, msg.From.ID); err != nil {
		b.replyHTML(msg, "❌ Failed to update setting.")
		return
	}
	if v {
		b.replyHTML(msg,
			"✅ <code>adminview</code> set to <code>on</code>. When ephemeral verification runs, "+
				"each group admin will also receive a private-in-chat copy of the challenge so "+
				"they can watch. (Requires <code>ephemeral</code> to also be on.)")
	} else {
		b.replyHTML(msg, "✅ <code>adminview</code> set to <code>off</code>. Only the joining user sees the challenge.")
	}
}

func (b *Bot) handleAutoDeleteSetting(msg *tgbotapi.Message, value string) {
	ttl, ok := parsePassNoticeTTL(value)
	if !ok {
		b.replyHTML(msg, fmt.Sprintf(
			"❌ Value must be <code>off</code> or a delay between <code>%s</code> and <code>%s</code>, "+
				"in seconds (<code>30</code>) or with a unit (<code>30s</code>, <code>5m</code>, <code>1h</code>).",
			formatPassNoticeTTL(minPassNoticeTTL), formatPassNoticeTTL(maxPassNoticeTTL),
		))
		return
	}
	if err := b.db.SetChatPassNoticeTTL(msg.Chat.ID, int(ttl/time.Second), msg.From.ID); err != nil {
		b.replyHTML(msg, "❌ Failed to update setting.")
		return
	}
	if ttl == 0 {
		b.replyHTML(msg, "✅ <code>autodelete</code> set to <code>off</code>. The \"passed verification\" notice stays in the group.")
		return
	}
	b.replyHTML(msg, fmt.Sprintf(
		"✅ <code>autodelete</code> set to <code>%s</code>. The \"passed verification\" notice will be removed "+
			"that long after it is posted. Notices already in the group are not affected.",
		formatPassNoticeTTL(ttl),
	))
}

// parsePassNoticeTTL reads an autodelete value: an off word, a bare number
// of seconds, or a duration with a unit ("90s", "5m", "1h"). Zero means off.
// Anything else — including a delay outside the allowed range or with a
// fraction of a second — is rejected.
func parsePassNoticeTTL(value string) (time.Duration, bool) {
	switch value {
	case "off", "false", "0", "disable", "disabled":
		return 0, true
	}
	var ttl time.Duration
	if seconds, err := strconv.Atoi(value); err == nil {
		if seconds < 0 || seconds > int(maxPassNoticeTTL/time.Second) {
			return 0, false
		}
		ttl = time.Duration(seconds) * time.Second
	} else if ttl, err = time.ParseDuration(value); err != nil {
		return 0, false
	}
	if ttl < minPassNoticeTTL || ttl > maxPassNoticeTTL || ttl%time.Second != 0 {
		return 0, false
	}
	return ttl, true
}

// formatPassNoticeTTL renders a delay in whole seconds, the form
// `/setting autodelete` accepts back.
func formatPassNoticeTTL(ttl time.Duration) string {
	return strconv.Itoa(int(ttl/time.Second)) + "s"
}
