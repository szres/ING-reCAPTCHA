package bot

import (
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// cmdSetup is a read-only diagnostic: it reports whether the bot has the
// admin rights it needs in this group and prints a short how-to so a group
// admin can fix anything that's missing. Unlike /setting it writes nothing,
// so it's safe for either a group admin OR a bot operator to run.
//
//	/setup   — check bot admin status + required permissions in this group
//
// All output goes through b.t so the report renders in the calling
// admin's language (stored preference → Telegram client locale →
// DEFAULT_LANGUAGE).
func (b *Bot) cmdSetup(msg *tgbotapi.Message, _ string) {
	if msg.Chat.Type != "group" && msg.Chat.Type != "supergroup" {
		b.replyHTML(msg, b.t(msg.From, "setup_group_only", b.self.UserName))
		return
	}
	// Read-only diagnostic — allow group admins or bot operators. No writes
	// happen here, so the stricter /setting gate isn't warranted.
	if !b.isGroupAdmin(msg.Chat.ID, msg.From.ID) && !b.isBotAdmin(msg.From.ID) {
		b.replyHTML(msg, b.t(msg.From, "setup_admin_only"))
		return
	}

	member, err := b.api.GetChatMember(tgbotapi.GetChatMemberConfig{
		ChatConfigWithUser: tgbotapi.ChatConfigWithUser{
			ChatID: msg.Chat.ID,
			UserID: b.self.ID,
		},
	})
	if err != nil {
		b.replyHTML(msg, b.t(msg.From, "setup_member_lookup_failed"))
		return
	}

	user := msg.From
	var lines []string
	add := func(key string, args ...interface{}) {
		lines = append(lines, b.t(user, key, args...))
	}

	title := msg.Chat.Title
	if title == "" {
		title = b.t(user, "setup_title_fallback_chat")
	}
	add("setup_title", escapeHTML(title))
	lines = append(lines, "")

	isAdmin := member.Status == "administrator" || member.Status == "creator"
	if isAdmin {
		add("setup_status_ok")
	} else {
		add("setup_status_not_admin", escapeHTML(member.Status), b.self.UserName)
	}
	lines = append(lines, "")

	// Required + recommended permissions. can_restrict_members and
	// can_delete_messages are hard requirements for the in-group flow;
	// can_invite_users is what lets the bot manage Join Requests (the
	// pre-join Mini App flow) and is recommended for approval-mode groups.
	check := func(ok bool) string {
		if ok {
			return "✅"
		}
		return "❌"
	}
	// When the bot isn't an admin, all the can_* flags come back false and
	// would read as a wall of ❌; only render the per-permission block when
	// admin so the message stays actionable.
	if isAdmin {
		add("setup_perms_header")
		add("setup_perm_delete", check(member.CanDeleteMessages))
		add("setup_perm_restrict", check(member.CanRestrictMembers))
		add("setup_perm_invite", check(member.CanInviteUsers))
		lines = append(lines, "")

		var missing []string
		if !member.CanDeleteMessages {
			missing = append(missing, b.t(user, "setup_perm_name_delete"))
		}
		if !member.CanRestrictMembers {
			missing = append(missing, b.t(user, "setup_perm_name_restrict"))
		}
		if len(missing) > 0 {
			add("setup_perms_missing",
				escapeHTML(strings.Join(missing, ", ")), b.self.UserName, b.self.UserName)
		} else {
			add("setup_perms_all_ok")
		}
		lines = append(lines, "")
	}

	// Current per-group verification settings, resolved to effective values
	// (mirrors /setting so admins get one place to see everything).
	ephLabel, adminLabel := b.effectiveEphemeralLabels(user, msg.Chat.ID)
	add("setup_settings_header")
	add("setup_setting_join_policy", b.effectiveJoinPolicyLabel(user, msg.Chat.ID))
	add("setup_setting_ephemeral", ephLabel)
	add("setup_setting_adminview", adminLabel)
	add("setup_setting_autodelete", b.autoDeleteLabel(user, msg.Chat.ID))
	lines = append(lines, "")

	add("setup_how_header")
	add("setup_how_approval")
	add("setup_how_open")
	lines = append(lines, "")
	add("setup_configure_hint", b.self.UserName, b.self.UserName)

	b.replyHTML(msg, strings.Join(lines, "\n"))
}

// effectiveJoinPolicyLabel resolves the display label for a chat's
// join-request fallback policy, showing the localized "(default)" marker
// when unset.
func (b *Bot) effectiveJoinPolicyLabel(user *tgbotapi.User, chatID int64) string {
	policy, err := b.db.GetChatJoinPolicy(chatID)
	if err != nil || policy == "" {
		return b.t(user, "setup_label_policy_default")
	}
	return policy
}

// resolvedChatSettings is the effective verification config for a chat
// after layering the per-group override (if any) over the global env
// config. Shared by /setting and /setup so the two can never disagree
// about what the bot will actually do.
type resolvedChatSettings struct {
	// EphemeralOn is what the bot will actually do.
	EphemeralOn bool
	// EphemeralExplicit is true when a group admin set the value
	// explicitly; false means it's inherited from the global config.
	EphemeralExplicit bool
	AdminVisible      bool
	// PassNoticeTTL is how long the "passed verification" notice stays in
	// the group before the bot deletes it. Zero means it is kept.
	PassNoticeTTL time.Duration
}

// resolveChatSettings layers the chat's stored override over the global
// config. A read error is treated as "no override" so verification and
// diagnostics both degrade to config defaults rather than failing.
func (b *Bot) resolveChatSettings(chatID int64) resolvedChatSettings {
	out := resolvedChatSettings{EphemeralOn: b.cfg.EphemeralVerifyEnabled}
	cs, err := b.db.GetChatSettings(chatID)
	if err != nil || cs == nil {
		return out
	}
	if cs.EphemeralVerify != nil {
		out.EphemeralOn = *cs.EphemeralVerify
		out.EphemeralExplicit = true
	}
	out.AdminVisible = cs.EphemeralAdminVisible
	out.PassNoticeTTL = time.Duration(cs.PassNoticeTTLSeconds) * time.Second
	return out
}

// autoDeleteLabel renders the autodelete setting as shown by /setting and
// /setup: the delay in the same form an admin types it, or the localized
// "off".
func (b *Bot) autoDeleteLabel(user *tgbotapi.User, chatID int64) string {
	ttl := b.resolveChatSettings(chatID).PassNoticeTTL
	if ttl <= 0 {
		return b.t(user, "setup_label_off")
	}
	return formatPassNoticeTTL(ttl)
}

// effectiveEphemeralLabels renders the resolved settings for /setup's
// compact one-line-per-setting layout, localized for the given user.
// Returned as (ephemeralLabel, adminViewLabel).
func (b *Bot) effectiveEphemeralLabels(user *tgbotapi.User, chatID int64) (string, string) {
	r := b.resolveChatSettings(chatID)
	onLabel := b.t(user, "setup_label_on")
	offLabel := b.t(user, "setup_label_off")

	ephemeralLabel := offLabel
	if r.EphemeralOn {
		ephemeralLabel = onLabel
	}
	if !r.EphemeralExplicit {
		ephemeralLabel += b.t(user, "setup_label_default_suffix")
	}
	adminLabel := offLabel
	if r.AdminVisible {
		adminLabel = onLabel
	}
	return ephemeralLabel, adminLabel
}
