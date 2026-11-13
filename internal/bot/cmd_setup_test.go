package bot

import (
	"testing"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setupTestBot wires a bot whose self identity is populated (cmdSetup
// prints b.self.UserName in its instructions) and whose GetChatMember
// answers can be steered per-call so we can simulate the bot being a
// plain member, an admin missing rights, or a fully-provisioned admin.
func setupTestBot(t *testing.T) (*Bot, *mockTelegramAPI) {
	t.Helper()
	b, mock := newTestBot(t)
	b.self = tgbotapi.User{ID: 999, UserName: "ING_reCAPTCHA_bot"}
	return b, mock
}

// botMemberFn returns a chatMemberFn that answers for the bot's own
// user ID with the given member, and treats every other user as a
// group administrator (so the caller passes the /setup auth gate).
func botMemberFn(botID int64, botMember tgbotapi.ChatMember) func(tgbotapi.GetChatMemberConfig) (tgbotapi.ChatMember, error) {
	return func(cfg tgbotapi.GetChatMemberConfig) (tgbotapi.ChatMember, error) {
		if cfg.UserID == botID {
			return botMember, nil
		}
		return tgbotapi.ChatMember{Status: "administrator"}, nil
	}
}

func TestCmdSetup_ReportsAllPermissionsGranted(t *testing.T) {
	b, mock := setupTestBot(t)
	mock.chatMemberFn = botMemberFn(b.self.ID, tgbotapi.ChatMember{
		Status:             "administrator",
		CanRestrictMembers: true,
		CanDeleteMessages:  true,
		CanInviteUsers:     true,
	})

	b.cmdSetup(commandMsg("supergroup"), "")
	reply := lastSentMessage(t, mock)

	assert.Equal(t, "HTML", reply.ParseMode)
	assert.Equal(t, 10, reply.ReplyToMessageID)
	assert.Contains(t, reply.Text, "administrator</b> ✅")
	assert.Contains(t, reply.Text, "All required permissions granted")
	assert.NotContains(t, reply.Text, "Missing required",
		"a fully-provisioned admin must not be told anything is missing")
	// Settings block should render the inherited defaults.
	assert.Contains(t, reply.Text, "approve (default)")
	assert.Contains(t, reply.Text, "off (default)")
}

func TestCmdSetup_FlagsMissingRequiredPermissions(t *testing.T) {
	b, mock := setupTestBot(t)
	// Admin, but the two rights the verification flow depends on are off.
	mock.chatMemberFn = botMemberFn(b.self.ID, tgbotapi.ChatMember{
		Status:             "administrator",
		CanRestrictMembers: false,
		CanDeleteMessages:  false,
		CanInviteUsers:     true,
	})

	b.cmdSetup(commandMsg("supergroup"), "")
	reply := lastSentMessage(t, mock)

	assert.Contains(t, reply.Text, "Missing required")
	assert.Contains(t, reply.Text, "Delete messages")
	assert.Contains(t, reply.Text, "Ban users")
	// Must tell the admin where to go, naming this bot specifically.
	assert.Contains(t, reply.Text, "ING_reCAPTCHA_bot")
	assert.NotContains(t, reply.Text, "All required permissions granted")
}

func TestCmdSetup_TreatsRecommendedPermissionAsNonBlocking(t *testing.T) {
	// can_invite_users is only needed for the Join Request / Mini App
	// flow, so its absence must NOT be reported as a hard failure —
	// open-mode groups work fine without it.
	b, mock := setupTestBot(t)
	mock.chatMemberFn = botMemberFn(b.self.ID, tgbotapi.ChatMember{
		Status:             "administrator",
		CanRestrictMembers: true,
		CanDeleteMessages:  true,
		CanInviteUsers:     false,
	})

	b.cmdSetup(commandMsg("supergroup"), "")
	reply := lastSentMessage(t, mock)

	assert.Contains(t, reply.Text, "All required permissions granted")
	assert.NotContains(t, reply.Text, "Missing required")
}

func TestCmdSetup_NotAdminSkipsPermissionBlock(t *testing.T) {
	// When the bot is a plain member every can_* flag is false; rendering
	// the per-permission list would be a wall of ❌ that buries the real
	// problem. Only the promote instruction should surface.
	b, mock := setupTestBot(t)
	mock.chatMemberFn = botMemberFn(b.self.ID, tgbotapi.ChatMember{Status: "member"})

	b.cmdSetup(commandMsg("supergroup"), "")
	reply := lastSentMessage(t, mock)

	assert.Contains(t, reply.Text, "member</b> ❌")
	assert.Contains(t, reply.Text, "not an administrator")
	assert.NotContains(t, reply.Text, "<b>Permissions</b>")
	assert.NotContains(t, reply.Text, "Missing required")
}

func TestCmdSetup_RejectsPrivateChat(t *testing.T) {
	b, mock := setupTestBot(t)
	b.cmdSetup(commandMsg("private"), "")
	assert.Contains(t, lastSentMessage(t, mock).Text, "can only be used inside a group")
}

func TestCmdSetup_RejectsNonAdminCaller(t *testing.T) {
	b, mock := setupTestBot(t)
	// Caller is a plain member and is not a bot admin.
	mock.chatMemberFn = func(cfg tgbotapi.GetChatMemberConfig) (tgbotapi.ChatMember, error) {
		return tgbotapi.ChatMember{Status: "member"}, nil
	}

	b.cmdSetup(commandMsg("supergroup"), "")
	assert.Contains(t, lastSentMessage(t, mock).Text, "Only group admins can run /setup")
}

func TestCmdSetup_AllowsBotAdminWhoIsNotGroupAdmin(t *testing.T) {
	// /setup is read-only, so unlike /setting a bot operator is allowed
	// to run it for remote diagnostics.
	b, mock := setupTestBot(t)
	require.NoError(t, b.db.AddAdmin(42)) // caller ID from commandMsg
	mock.chatMemberFn = func(cfg tgbotapi.GetChatMemberConfig) (tgbotapi.ChatMember, error) {
		if cfg.UserID == b.self.ID {
			return tgbotapi.ChatMember{
				Status:             "administrator",
				CanRestrictMembers: true,
				CanDeleteMessages:  true,
			}, nil
		}
		return tgbotapi.ChatMember{Status: "member"}, nil
	}

	b.cmdSetup(commandMsg("supergroup"), "")
	reply := lastSentMessage(t, mock)
	assert.Contains(t, reply.Text, "Setup check")
	assert.NotContains(t, reply.Text, "Only group admins")
}

// TestResolveChatSettings covers the shared resolver that /setting,
// /setup, and the verification path all read. Getting this wrong means
// the displayed config diverges from actual behaviour.
func TestResolveChatSettings(t *testing.T) {
	const chatID int64 = -100777

	t.Run("no row inherits global config off", func(t *testing.T) {
		b, _ := setupTestBot(t)
		b.cfg.EphemeralVerifyEnabled = false
		r := b.resolveChatSettings(chatID)
		assert.False(t, r.EphemeralOn)
		assert.False(t, r.EphemeralExplicit)
		assert.False(t, r.AdminVisible)
	})

	t.Run("no row inherits global config on", func(t *testing.T) {
		b, _ := setupTestBot(t)
		b.cfg.EphemeralVerifyEnabled = true
		r := b.resolveChatSettings(chatID)
		assert.True(t, r.EphemeralOn, "must inherit the global default")
		assert.False(t, r.EphemeralExplicit)
	})

	t.Run("explicit off overrides global on", func(t *testing.T) {
		b, _ := setupTestBot(t)
		b.cfg.EphemeralVerifyEnabled = true
		off := false
		require.NoError(t, b.db.SetChatEphemeralVerify(chatID, &off, 42))

		r := b.resolveChatSettings(chatID)
		assert.False(t, r.EphemeralOn, "per-group off must beat global on")
		assert.True(t, r.EphemeralExplicit)
	})

	t.Run("explicit on overrides global off", func(t *testing.T) {
		b, _ := setupTestBot(t)
		b.cfg.EphemeralVerifyEnabled = false
		on := true
		require.NoError(t, b.db.SetChatEphemeralVerify(chatID, &on, 42))

		r := b.resolveChatSettings(chatID)
		assert.True(t, r.EphemeralOn, "per-group on must beat global off")
		assert.True(t, r.EphemeralExplicit)
	})

	t.Run("clearing the override falls back to inherit", func(t *testing.T) {
		b, _ := setupTestBot(t)
		b.cfg.EphemeralVerifyEnabled = true
		off := false
		require.NoError(t, b.db.SetChatEphemeralVerify(chatID, &off, 42))
		require.NoError(t, b.db.SetChatEphemeralVerify(chatID, nil, 42))

		r := b.resolveChatSettings(chatID)
		assert.True(t, r.EphemeralOn, "cleared override must inherit global on")
		assert.False(t, r.EphemeralExplicit)
	})

	t.Run("adminview is independent of ephemeral", func(t *testing.T) {
		b, _ := setupTestBot(t)
		require.NoError(t, b.db.SetChatEphemeralAdminVisible(chatID, true, 42))
		r := b.resolveChatSettings(chatID)
		assert.True(t, r.AdminVisible)
		assert.False(t, r.EphemeralExplicit, "adminview write must not imply an ephemeral override")
	})
}

func TestEffectiveEphemeralLabels(t *testing.T) {
	const chatID int64 = -100888

	b, _ := setupTestBot(t)
	b.cfg.EphemeralVerifyEnabled = false
	user := &tgbotapi.User{ID: 42, LanguageCode: "en"}

	eph, admin := b.effectiveEphemeralLabels(user, chatID)
	assert.Equal(t, "off (default)", eph)
	assert.Equal(t, "off", admin)

	on := true
	require.NoError(t, b.db.SetChatEphemeralVerify(chatID, &on, 42))
	require.NoError(t, b.db.SetChatEphemeralAdminVisible(chatID, true, 42))

	eph, admin = b.effectiveEphemeralLabels(user, chatID)
	assert.Equal(t, "on", eph, "explicit values must not carry the (default) suffix")
	assert.Equal(t, "on", admin)
}

// TestEffectiveEphemeralLabels_Localized proves the labels follow the
// caller's language rather than being hardcoded English. A regression
// here would silently ship English strings to zh users.
func TestEffectiveEphemeralLabels_Localized(t *testing.T) {
	const chatID int64 = -100889

	b, _ := setupTestBot(t)
	b.cfg.EphemeralVerifyEnabled = false
	zhUser := &tgbotapi.User{ID: 77, LanguageCode: "zh"}

	eph, admin := b.effectiveEphemeralLabels(zhUser, chatID)
	assert.Equal(t, "关闭（默认）", eph)
	assert.Equal(t, "关闭", admin)

	on := true
	require.NoError(t, b.db.SetChatEphemeralVerify(chatID, &on, 77))
	require.NoError(t, b.db.SetChatEphemeralAdminVisible(chatID, true, 77))

	eph, admin = b.effectiveEphemeralLabels(zhUser, chatID)
	assert.Equal(t, "开启", eph)
	assert.Equal(t, "开启", admin)
}

// TestCmdSetup_LocalizedForChineseAdmin runs the whole command for a zh
// caller and asserts the rendered report is Chinese end to end — title,
// permission lines, and the how-to block.
func TestCmdSetup_LocalizedForChineseAdmin(t *testing.T) {
	b, mock := setupTestBot(t)
	mock.chatMemberFn = botMemberFn(b.self.ID, tgbotapi.ChatMember{
		Status:             "administrator",
		CanRestrictMembers: true,
		CanDeleteMessages:  true,
		CanInviteUsers:     true,
	})

	msg := commandMsg("supergroup")
	msg.From.LanguageCode = "zh"
	msg.Chat.Title = "北京 IFS"

	b.cmdSetup(msg, "")
	reply := lastSentMessage(t, mock)

	assert.Contains(t, reply.Text, "配置检查")
	assert.Contains(t, reply.Text, "机器人状态")
	assert.Contains(t, reply.Text, "所有必需权限已授予")
	assert.Contains(t, reply.Text, "验证方式说明")
	assert.Contains(t, reply.Text, "审批模式")
	// The group title must survive verbatim.
	assert.Contains(t, reply.Text, "北京 IFS")
	// No English fallback should leak through.
	assert.NotContains(t, reply.Text, "Setup check")
	assert.NotContains(t, reply.Text, "All required permissions")
}

// TestCmdSetup_LocalizedMissingPermissions checks the failure branch is
// translated too — including the permission NAMES inside the "missing"
// list, which are separate keys and easy to forget.
func TestCmdSetup_LocalizedMissingPermissions(t *testing.T) {
	b, mock := setupTestBot(t)
	mock.chatMemberFn = botMemberFn(b.self.ID, tgbotapi.ChatMember{
		Status:             "administrator",
		CanRestrictMembers: false,
		CanDeleteMessages:  false,
	})

	msg := commandMsg("supergroup")
	msg.From.LanguageCode = "zh"

	b.cmdSetup(msg, "")
	reply := lastSentMessage(t, mock)

	assert.Contains(t, reply.Text, "缺少必需权限")
	assert.Contains(t, reply.Text, "删除消息")
	assert.Contains(t, reply.Text, "封禁成员")
	assert.NotContains(t, reply.Text, "Delete messages")
	assert.NotContains(t, reply.Text, "Ban users")
}

// TestCmdSetup_LocalizedRejections covers the two early-exit paths.
func TestCmdSetup_LocalizedRejections(t *testing.T) {
	b, mock := setupTestBot(t)

	privateMsg := commandMsg("private")
	privateMsg.From.LanguageCode = "zh"
	b.cmdSetup(privateMsg, "")
	assert.Contains(t, lastSentMessage(t, mock).Text, "只能在群组内使用")

	mock.chatMemberFn = func(cfg tgbotapi.GetChatMemberConfig) (tgbotapi.ChatMember, error) {
		return tgbotapi.ChatMember{Status: "member"}, nil
	}
	groupMsg := commandMsg("supergroup")
	groupMsg.From.LanguageCode = "zh"
	b.cmdSetup(groupMsg, "")
	assert.Contains(t, lastSentMessage(t, mock).Text, "只有群管理员")
}

// TestCmdSetup_UsesStoredLanguagePreferenceOverClientLocale verifies the
// report honors an explicit /setlang choice, matching how every other
// user-facing string resolves.
func TestCmdSetup_UsesStoredLanguagePreferenceOverClientLocale(t *testing.T) {
	b, mock := setupTestBot(t)
	mock.chatMemberFn = botMemberFn(b.self.ID, tgbotapi.ChatMember{
		Status:             "administrator",
		CanRestrictMembers: true,
		CanDeleteMessages:  true,
	})

	// Client says English, stored preference says Chinese — stored wins.
	require.NoError(t, b.db.SetUserLanguage(42, "zh"))
	msg := commandMsg("supergroup")
	msg.From.LanguageCode = "en"

	b.cmdSetup(msg, "")
	reply := lastSentMessage(t, mock)
	assert.Contains(t, reply.Text, "配置检查")
	assert.NotContains(t, reply.Text, "Setup check")
}
