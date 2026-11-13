package bot

import (
	"fmt"
	"log"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

func (b *Bot) handleMessage(msg *tgbotapi.Message) {
	// Debug: log all message types to diagnose join message handling
	if len(msg.NewChatMembers) > 0 {
		log.Printf("[DEBUG] Received new_chat_members message: %d members, message ID: %d", len(msg.NewChatMembers), msg.MessageID)
	}

	if msg.IsCommand() {
		b.handleCommand(msg)
		return
	}

	if msg.Chat.Type != "group" && msg.Chat.Type != "supergroup" {
		return
	}

	// Store join service message ID so it can be deleted on verification failure.
	// Do not delete immediately — the message is kept if the user passes.
	// Due to a race condition (new_chat_members may arrive before the pending
	// verification DB record is created by handleChatMemberUpdate), we always
	// buffer the ID in joinMsgCache. The DB update is also attempted; if the
	// record doesn't exist yet, handleChatMemberUpdate will drain the cache.
	if len(msg.NewChatMembers) > 0 {
		for _, member := range msg.NewChatMembers {
			log.Printf("[DEBUG] Processing new_chat_member: user %d (%s) in chat %d", member.ID, member.FirstName, msg.Chat.ID)
			key := fmt.Sprintf("%d:%d", msg.Chat.ID, member.ID)
			b.joinMsgCache.Store(key, msg.MessageID)
			log.Printf("[DEBUG] Stored join message ID %d in cache for key %s", msg.MessageID, key)
			if err := b.db.UpdateJoinMessageID(msg.Chat.ID, member.ID, msg.MessageID); err != nil {
				log.Printf("[WARN] Failed to store join message ID for user %d: %v", member.ID, err)
			} else {
				log.Printf("[DEBUG] Successfully stored join message ID %d in DB for user %d", msg.MessageID, member.ID)
			}
		}
		return
	}

	// Anonymous channel post (sender_chat). Telegram routes channel admins
	// posting as the channel here, not as a user. Log distinctly so post-hoc
	// forensics can tell user-spam from channel-spam.
	if msg.From == nil {
		if msg.SenderChat != nil {
			log.Printf("[DEBUG] Anonymous/channel post in chat %d from sender_chat=%d (%s) msg_id=%d",
				msg.Chat.ID, msg.SenderChat.ID, msg.SenderChat.Title, msg.MessageID)
		}
		return
	}

	// Check pending verification first — those messages are mid-CAPTCHA chatter
	// and just need silent deletion. Tombstone re-tempban below would otherwise
	// re-kick a user who's legitimately trying again after a first failure.
	pv, err := b.db.GetPendingVerification(msg.Chat.ID, msg.From.ID)
	if err != nil {
		log.Printf("Error checking pending verification: %v", err)
		return
	}
	if pv != nil {
		deleteMsg := tgbotapi.NewDeleteMessage(msg.Chat.ID, msg.MessageID)
		b.api.Request(deleteMsg)
		return
	}

	// Tombstone filter: a user with a verification_failures row should be
	// tempbanned at the Telegram level right now. If a message still arrives,
	// it's either a ghost-fire of a scheduled message that survived the
	// tempban, a silently-rejoined member whose chat_member update was
	// dropped, or a Telegram delivery race. Delete + re-tempban, and log
	// enough state to disambiguate after the fact.
	//
	// Kill-switch: groups running in Join Request mode never accumulate
	// verification_failures rows through the pre-join flow, and the ones
	// they carry from earlier legacy-flow tests can misfire on admins.
	// ANTISPAM_TOMBSTONE_ENABLED=false skips the filter entirely.
	if !b.cfg.AntispamTombstoneEnabled {
		return
	}
	hasFailure, err := b.db.HasPreviousFailure(msg.Chat.ID, msg.From.ID)
	if err != nil {
		log.Printf("[WARN] Tombstone check failed for user %d: %v", msg.From.ID, err)
	} else if hasFailure {
		var memberStatus string
		var memberUntilDate int64
		if member, mErr := b.api.GetChatMember(tgbotapi.GetChatMemberConfig{
			ChatConfigWithUser: tgbotapi.ChatConfigWithUser{ChatID: msg.Chat.ID, UserID: msg.From.ID},
		}); mErr != nil {
			memberStatus = "<getChatMember error: " + mErr.Error() + ">"
		} else {
			memberStatus = member.Status
			memberUntilDate = member.UntilDate
		}
		previewLen := 80
		if len(msg.Text) < previewLen {
			previewLen = len(msg.Text)
		}
		log.Printf("[WARN] Tombstone hit: msg from user %d in chat %d (status=%s until=%d) msg_id=%d date=%d text_preview=%q",
			msg.From.ID, msg.Chat.ID, memberStatus, memberUntilDate, msg.MessageID, msg.Date, msg.Text[:previewLen])

		// Optional delegation to an in-group anti-spam bot. We can't pre-check
		// whether the bot is a member (Telegram's getChatAdministrators excludes
		// bots), so we send unconditionally and clean up our own command after
		// 2s. If the bot is present it acts on its own /ban command; if absent
		// the message is just litter that we delete ourselves.
		var delegateMsgID int
		delegated := false
		if b.cfg.AntispamBanBot != "" {
			delegateMsg := tgbotapi.NewMessage(msg.Chat.ID, "/ban@"+b.cfg.AntispamBanBot)
			delegateMsg.ReplyToMessageID = msg.MessageID
			delegateMsg.DisableNotification = true
			if sent, sErr := b.api.Send(delegateMsg); sErr != nil {
				log.Printf("[WARN] Tombstone /ban@%s send failed for chat %d: %v", b.cfg.AntispamBanBot, msg.Chat.ID, sErr)
			} else {
				delegated = true
				delegateMsgID = sent.MessageID
				log.Printf("[INFO] Tombstone delegated to @%s for user %d in chat %d (cmd msg %d)",
					b.cfg.AntispamBanBot, msg.From.ID, msg.Chat.ID, delegateMsgID)
			}
		}

		// If we delegated, give the anti-spam bot a 2-second head start before
		// our own delete+tempban so it can ban/delete first. Re-fetch member
		// status afterward — the bot may have just permanently banned the user.
		if delegated {
			time.Sleep(2 * time.Second)
			if member, mErr := b.api.GetChatMember(tgbotapi.GetChatMemberConfig{
				ChatConfigWithUser: tgbotapi.ChatConfigWithUser{ChatID: msg.Chat.ID, UserID: msg.From.ID},
			}); mErr == nil {
				memberStatus = member.Status
				memberUntilDate = member.UntilDate
			}
			// Clean up our own /ban command so groups without the anti-spam bot
			// don't see lingering command litter. If the bot processed it,
			// removing the trigger keeps the chat tidy too.
			if _, dErr := b.api.Request(tgbotapi.NewDeleteMessage(msg.Chat.ID, delegateMsgID)); dErr != nil {
				log.Printf("[DEBUG] Tombstone delegate-cmd cleanup returned err (already gone?): %v", dErr)
			}
		}

		// Delete the offending message. No-op if the anti-spam bot already did.
		if _, dErr := b.api.Request(tgbotapi.NewDeleteMessage(msg.Chat.ID, msg.MessageID)); dErr != nil {
			log.Printf("[DEBUG] Tombstone spam delete returned err (may be already gone): %v", dErr)
		}

		// Policy: a user with only a first-failure tempban who manages to land
		// a message (almost always a pre-queued scheduled message firing from
		// Telegram's side after their tempban started) has produced definitive
		// spam evidence. Skip the second-chance retry and permanently ban
		// them. Matches the second-failure cleanup in handleVerificationFailure.
		if memberStatus == "kicked" && memberUntilDate == 0 {
			log.Printf("[INFO] User %d already permanently banned in chat %d (anti-spam bot or prior path); clearing residual state", msg.From.ID, msg.Chat.ID)
		} else {
			if _, bErr := b.api.Request(tgbotapi.KickChatMemberConfig{
				ChatMemberConfig: tgbotapi.ChatMemberConfig{ChatID: msg.Chat.ID, UserID: msg.From.ID},
				// UntilDate omitted → permanent ban.
			}); bErr != nil {
				log.Printf("[ERROR] Tombstone permanent ban failed for user %d: %v", msg.From.ID, bErr)
			} else {
				log.Printf("[INFO] Tombstone permanently banned user %d in chat %d (scheduled-message spam evidence)", msg.From.ID, msg.Chat.ID)
			}
		}

		// Clear residual DB state to match the second-failure permanent-ban
		// cleanup path: a permanent ban means the user is gone for good, so
		// the failure row and join history shouldn't linger.
		if err := b.db.ClearVerificationFailure(msg.Chat.ID, msg.From.ID); err != nil {
			log.Printf("[ERROR] Tombstone: failed to clear failure record for user %d: %v", msg.From.ID, err)
		}
		if err := b.db.DeleteUserJoinHistory(msg.Chat.ID, msg.From.ID); err != nil {
			log.Printf("[ERROR] Tombstone: failed to delete join history for user %d: %v", msg.From.ID, err)
		}
	}
}

func (b *Bot) handleCommand(msg *tgbotapi.Message) {
	cmd := msg.Command()
	args := msg.CommandArguments()

	// In groups, generic-name commands only respond when explicitly addressed
	// to this bot (e.g., /start@ING_reCAPTCHA_bot). Plain /start is a
	// wildcard that every bot in the group sees, and replying to it pollutes
	// the chat. /joinpolicy is also on this list because admins of groups
	// that host multiple anti-spam bots will type the bare name out of habit
	// and we don't want it interpreted by us when they meant someone else.
	// Bare /start, /help, /joinpolicy in groups are wildcard-shaped: they
	// exist on every bot and typing them without an @bot suffix pollutes
	// multi-bot chats. /setting is per-bot config by nature and admin-only,
	// so the collision risk is low — bare /setting in a group is treated
	// as intent to configure THIS bot. That matches operator muscle memory
	// (they just typed /setting@bot a moment ago; typing @bot on every
	// follow-up is friction).
	if (cmd == "start" || cmd == "help" || cmd == "joinpolicy") &&
		(msg.Chat.Type == "group" || msg.Chat.Type == "supergroup") &&
		!b.commandTargetsThisBot(msg) {
		return
	}

	switch cmd {
	case "start":
		b.cmdStart(msg, args)
	case "help":
		b.cmdHelp(msg)
	case "addset":
		b.cmdAddSet(msg, args)
	case "addimage":
		b.cmdAddImage(msg, args)
	case "listsets":
		b.cmdListSets(msg)
	case "delset":
		b.cmdDelSet(msg, args)
	case "setstats":
		b.cmdSetStats(msg)
	case "test":
		b.cmdTest(msg)
	case "addadmin":
		b.cmdAddAdmin(msg, args)
	case "removeadmin":
		b.cmdRemoveAdmin(msg, args)
	case "listadmins":
		b.cmdListAdmins(msg)
	case "myid":
		b.cmdMyID(msg)
	case "setlang":
		b.cmdSetLang(msg, args)
	case "addlabel":
		b.cmdAddLabel(msg, args)
	case "listlabels":
		b.cmdListLabels(msg, args)
	case "dellabel":
		b.cmdDelLabel(msg, args)
	case "cachemissing":
		b.cmdCacheMissing(msg, args)
	case "joinpolicy":
		b.cmdJoinPolicy(msg, args)
	case "setting", "settings":
		b.cmdSetting(msg, args)
	case "setup":
		b.cmdSetup(msg, args)
	}
}

func (b *Bot) handleChatMemberUpdate(update *tgbotapi.ChatMemberUpdated) {
	log.Printf("[DEBUG] Chat member update: old status=%s, new status=%s, user=%d, chat=%d",
		update.OldChatMember.Status, update.NewChatMember.Status,
		update.NewChatMember.User.ID, update.Chat.ID)

	chatID := update.Chat.ID
	userID := update.NewChatMember.User.ID
	newStatus := update.NewChatMember.Status
	oldStatus := update.OldChatMember.Status

	// If user was removed by another anti-spam bot before finishing verification,
	// clear pending state immediately to avoid stale timeout/failure side effects.
	if (newStatus == "left" || newStatus == "kicked") &&
		(oldStatus == "member" || oldStatus == "restricted" || oldStatus == "administrator" || oldStatus == "creator") {
		log.Printf("[INFO] User %d left/was removed in chat %d (old=%s,new=%s), cleaning verification state",
			userID, chatID, oldStatus, newStatus)
		b.cleanupVerificationState(chatID, userID)
		return
	}

	// Only handle new members joining
	if newStatus != "member" {
		log.Printf("[DEBUG] New status is not 'member', ignoring")
		return
	}

	// Only treat truly fresh joins as new joins. A "restricted → member" transition
	// is just the unrestrict flow (admin approve, successful CAPTCHA, auto-approve)
	// and must not kick off a second verification.
	if oldStatus != "left" && oldStatus != "kicked" {
		log.Printf("[DEBUG] Old status %q is not left/kicked, ignoring (not a fresh join)", oldStatus)
		return
	}

	// Skip bots
	if update.NewChatMember.User.IsBot {
		log.Printf("[DEBUG] User is a bot, ignoring")
		return
	}

	log.Printf("[INFO] New member joined: user %d in chat %d", userID, chatID)

	// Mini App dual-flow race guard.
	//
	// When the Mini App approves a user via answerChatJoinRequestQuery,
	// Telegram delivers a chat_member 'left → member' transition right
	// after admitting them. Without this guard the bot would interpret
	// that transition as a fresh join and start a SECOND captcha on top
	// of the user we already approved.
	//
	// HasRecentMiniAppApproval looks up approved_prejoin sessions within
	// the past 5 minutes (matches the Mini App session TTL). If found,
	// skip the in-group flow entirely — the user already proved they're
	// human and the silent welcome message (if enabled) was sent from
	// the Mini App's approve path.
	if b.cfg.MiniAppEnabled {
		approved, err := b.db.HasRecentMiniAppApproval(chatID, userID, b.cfg.MiniAppSessionTTLSeconds)
		if err != nil {
			log.Printf("[ERROR] check mini app approval for user %d in chat %d: %v", userID, chatID, err)
		} else if approved {
			log.Printf("[INFO] Skipping in-group verification for user %d in chat %d — recently approved via Mini App", userID, chatID)
			return
		}
	}

	if b.isVerifiedUser(userID) {
		// A strike left over from an earlier failed attempt in this chat
		// would otherwise make the tombstone filter ban them on their first
		// message, now that no captcha is going to clear it.
		if err := b.db.ClearVerificationFailure(chatID, userID); err != nil {
			log.Printf("[ERROR] Failed to clear failure record for verified user %d in chat %d: %v", userID, chatID, err)
		}
		log.Printf("[INFO] Skipping verification for user %d in chat %d — already verified in another chat", userID, chatID)
		return
	}

	// Check bot permissions in this chat
	if !b.checkBotPermissions(chatID) {
		log.Printf("[ERROR] Bot lacks required permissions in chat %d, cannot verify users", chatID)
		return
	}

	// If user already has a failure record, this is the expected second-attempt flow.
	hasPreviousFailure, err := b.db.HasPreviousFailure(chatID, userID)
	if err != nil {
		log.Printf("[ERROR] Error checking failure history: %v", err)
		hasPreviousFailure = false
	}

	// Check for spam (multiple joins in short time) only for users without failure history.
	if !hasPreviousFailure && b.cfg.RejoinCooldownSeconds > 0 {
		recentJoins, err := b.db.GetRecentJoinCount(chatID, userID, b.cfg.RejoinCooldownSeconds)
		if err != nil {
			log.Printf("[ERROR] Error checking recent joins: %v", err)
		}
		if recentJoins > 0 {
			until := time.Now().Add(time.Duration(b.cfg.RejoinCooldownSeconds) * time.Second).Unix()
			log.Printf("[WARN] User %d is rejoining too quickly (%d recent joins), temporary ban until %d", userID, recentJoins, until)
			kickConfig := tgbotapi.KickChatMemberConfig{
				ChatMemberConfig: tgbotapi.ChatMemberConfig{
					ChatID: chatID,
					UserID: userID,
				},
				UntilDate: until,
			}
			if _, err := b.api.Request(kickConfig); err != nil {
				log.Printf("[ERROR] Failed to temporary-ban spam rejoiner: %v", err)
			}
			return
		}
	}

	// Record join
	log.Printf("[DEBUG] Recording user join in database")
	if err := b.db.RecordUserJoin(chatID, userID); err != nil {
		log.Printf("[ERROR] Error recording user join: %v", err)
	}

	// Restrict user permissions
	log.Printf("[DEBUG] Restricting user %d permissions", userID)
	if err := b.restrictUser(chatID, userID); err != nil {
		log.Printf("[ERROR] Error restricting user: %v", err)
		return
	}

	// Start verification
	log.Printf("[DEBUG] Starting verification for user %d", userID)
	if err := b.startVerification(chatID, userID, update.NewChatMember.User); err != nil {
		log.Printf("[ERROR] Error starting verification: %v", err)
		// Unrestrict user if verification fails to start
		log.Printf("[DEBUG] Unrestricting user due to verification start failure")
		b.unrestrictUser(chatID, userID)
		return
	}

	// Drain cached join message ID that may have arrived before the DB record was created.
	key := fmt.Sprintf("%d:%d", chatID, userID)
	if msgID, ok := b.joinMsgCache.LoadAndDelete(key); ok {
		if err := b.db.UpdateJoinMessageID(chatID, userID, msgID.(int)); err != nil {
			log.Printf("[WARN] Failed to drain cached join message ID for user %d: %v", userID, err)
		} else {
			log.Printf("[DEBUG] Drained cached join message ID %v for user %d", msgID, userID)
		}
	}
}

// isVerifiedUser reports whether the user is on the global pass list — they
// solved a captcha themselves in some chat, or an operator imported them —
// and so joins every chat without another one. If the lookup fails the user
// is verified as usual: an unnecessary captcha is the safe mistake.
func (b *Bot) isVerifiedUser(userID int64) bool {
	verified, err := b.db.IsUserVerified(userID)
	if err != nil {
		log.Printf("[ERROR] Pass list lookup failed for user %d, verifying as usual: %v", userID, err)
		return false
	}
	return verified
}

func (b *Bot) cleanupVerificationState(chatID, userID int64) {
	key := fmt.Sprintf("%d:%d", chatID, userID)

	// Load and remove from cache (but keep the value for fallback)
	cachedJoinMsgID, hasCached := b.joinMsgCache.LoadAndDelete(key)

	// Get pending verification to retrieve message IDs before deletion
	pv, err := b.db.GetPendingVerification(chatID, userID)
	if err != nil {
		log.Printf("[WARN] Failed to get pending verification for user %d: %v", userID, err)
	}

	// If no pending verification exists, nothing to clean up
	if pv == nil {
		log.Printf("[DEBUG] No pending verification found for user %d, skipping message cleanup", userID)
		// Still clean up join history in case it exists
		if err := b.db.DeleteUserJoinHistory(chatID, userID); err != nil {
			log.Printf("[WARN] Failed to delete join history for user %d: %v", userID, err)
		}
		return
	}

	// Ephemeral verifications set EphemeralMessageID (MessageID stays
	// NULL) — route them through the shared helper so admin
	// broadcasts get cleaned up in the same call. Regular verifications
	// with MessageID set take the same helper. Only the "message not
	// yet sent at all" case (neither ID present) falls to the delayed
	// cleanup, because the send is still in-flight.
	if pv.EphemeralMessageID != 0 || pv.MessageID.Valid {
		b.deleteVerificationMessageFor(pv, "member-left")
		if err := b.db.DeletePendingVerification(chatID, userID); err != nil {
			log.Printf("[WARN] Failed to delete pending verification for user %d: %v", userID, err)
		}
	} else {
		log.Printf("[DEBUG] Verification message not yet sent for user %d, scheduling delayed cleanup", userID)
		go func() {
			time.Sleep(10 * time.Second)
			b.cleanupVerificationMessageDelayed(chatID, userID)
		}()
		// Don't delete pending verification yet - let delayed cleanup handle it
	}

	// Delete join message if exists (prefer DB, fallback to cache)
	var joinMsgID int
	if pv.JoinMessageID.Valid {
		joinMsgID = int(pv.JoinMessageID.Int64)
	} else if hasCached {
		if msgID, ok := cachedJoinMsgID.(int); ok {
			joinMsgID = msgID
		}
	}

	if joinMsgID > 0 {
		log.Printf("[DEBUG] Deleting join message %d for removed user %d", joinMsgID, userID)
		deleteMsg := tgbotapi.NewDeleteMessage(chatID, joinMsgID)
		if _, err := b.api.Request(deleteMsg); err != nil {
			log.Printf("[WARN] Failed to delete join message %d: %v", joinMsgID, err)
		} else {
			log.Printf("[DEBUG] Successfully deleted join message %d", joinMsgID)
		}
	}

	// Always clean up join history
	if err := b.db.DeleteUserJoinHistory(chatID, userID); err != nil {
		log.Printf("[WARN] Failed to delete join history for user %d: %v", userID, err)
	}
}

// cleanupVerificationMessageDelayed attempts to delete a verification message
// after a delay, used when the message hasn't been sent yet during initial cleanup
func (b *Bot) cleanupVerificationMessageDelayed(chatID, userID int64) {
	log.Printf("[DEBUG] Delayed cleanup: checking for verification message for user %d", userID)

	// Check if pending verification still exists
	pv, err := b.db.GetPendingVerification(chatID, userID)
	if err != nil {
		log.Printf("[WARN] Delayed cleanup: failed to get pending verification for user %d: %v", userID, err)
		return
	}

	// If no pending verification, it was already cleaned up by another path
	if pv == nil {
		log.Printf("[DEBUG] Delayed cleanup: no pending verification for user %d, already cleaned up", userID)
		return
	}

	// If neither ID is set, give up (message send probably failed).
	if !pv.MessageID.Valid && pv.EphemeralMessageID == 0 {
		log.Printf("[DEBUG] Delayed cleanup: verification message still not sent for user %d after delay, giving up", userID)
		if err := b.db.DeletePendingVerification(chatID, userID); err != nil {
			log.Printf("[WARN] Delayed cleanup: failed to delete pending verification for user %d: %v", userID, err)
		}
		return
	}

	// Route through the shared helper — handles both regular and
	// ephemeral (plus any admin ephemeral broadcasts) uniformly.
	b.deleteVerificationMessageFor(pv, "delayed-cleanup")

	// Clean up pending verification
	if err := b.db.DeletePendingVerification(chatID, userID); err != nil {
		log.Printf("[WARN] Delayed cleanup: failed to delete pending verification for user %d: %v", userID, err)
	}
}

func (b *Bot) restrictUser(chatID, userID int64) error {
	permissions := tgbotapi.ChatPermissions{
		CanSendMessages:       false,
		CanSendMediaMessages:  false,
		CanSendOtherMessages:  false,
		CanAddWebPagePreviews: false,
	}

	restrictConfig := tgbotapi.RestrictChatMemberConfig{
		ChatMemberConfig: tgbotapi.ChatMemberConfig{
			ChatID: chatID,
			UserID: userID,
		},
		Permissions: &permissions,
	}

	_, err := b.api.Request(restrictConfig)
	return err
}

func (b *Bot) unrestrictUser(chatID, userID int64) error {
	permissions := tgbotapi.ChatPermissions{
		CanSendMessages:       true,
		CanSendMediaMessages:  true,
		CanSendOtherMessages:  true,
		CanAddWebPagePreviews: true,
		CanSendPolls:          true,
		CanInviteUsers:        true,
		CanPinMessages:        false,
		CanChangeInfo:         false,
	}

	restrictConfig := tgbotapi.RestrictChatMemberConfig{
		ChatMemberConfig: tgbotapi.ChatMemberConfig{
			ChatID: chatID,
			UserID: userID,
		},
		Permissions: &permissions,
	}

	_, err := b.api.Request(restrictConfig)
	return err
}

func (b *Bot) sendMessage(chatID int64, text string) {
	msg := tgbotapi.NewMessage(chatID, text)
	msg.ParseMode = "HTML"
	b.api.Send(msg)
}

func (b *Bot) isGroupAdmin(chatID, userID int64) bool {
	member, err := b.api.GetChatMember(tgbotapi.GetChatMemberConfig{
		ChatConfigWithUser: tgbotapi.ChatConfigWithUser{
			ChatID: chatID,
			UserID: userID,
		},
	})
	if err != nil {
		return false
	}
	return member.Status == "administrator" || member.Status == "creator"
}

func (b *Bot) isBotAdmin(userID int64) bool {
	isAdmin, err := b.db.IsAdmin(userID)
	if err != nil {
		return false
	}
	return isAdmin
}

func (b *Bot) canManageBot(msg *tgbotapi.Message) bool {
	// Only bot admins can manage image sets (both in private chat and groups)
	// This prevents malicious group admins from modifying the shared image database
	return b.isBotAdmin(msg.From.ID)
}

func (b *Bot) checkBotPermissions(chatID int64) bool {
	status, missing, err := b.botPermissionStatus(chatID)
	if err != nil {
		return false
	}
	if status != "administrator" && status != "creator" {
		log.Printf("[WARN] Bot is not an administrator in chat %d (status: %s)", chatID, status)
		return false
	}
	if len(missing) > 0 {
		log.Printf("[WARN] Bot lacks required permissions in chat %d: %v", chatID, missing)
		return false
	}
	log.Printf("[DEBUG] Bot has all required permissions in chat %d", chatID)
	return true
}

// botPermissionStatus returns the bot's chat-member status and any required
// permissions it is missing. status == "" when the API call failed.
func (b *Bot) botPermissionStatus(chatID int64) (status string, missing []string, err error) {
	botMember, err := b.api.GetChatMember(tgbotapi.GetChatMemberConfig{
		ChatConfigWithUser: tgbotapi.ChatConfigWithUser{
			ChatID: chatID,
			UserID: b.self.ID,
		},
	})
	if err != nil {
		log.Printf("[ERROR] Failed to get bot member status: %v", err)
		return "", nil, err
	}
	status = botMember.Status
	if status != "administrator" && status != "creator" {
		return status, nil, nil
	}
	if !botMember.CanRestrictMembers {
		missing = append(missing, "can_restrict_members")
	}
	if !botMember.CanDeleteMessages {
		missing = append(missing, "can_delete_messages")
	}
	return status, missing, nil
}

// commandTargetsThisBot reports whether the message's command was explicitly
// addressed to this bot via the @username suffix (e.g. /start@ING_reCAPTCHA_bot).
// A plain /start with no @ suffix is a group-wide broadcast and returns false.
func (b *Bot) commandTargetsThisBot(msg *tgbotapi.Message) bool {
	cmdWithAt := msg.CommandWithAt()
	at := strings.IndexByte(cmdWithAt, '@')
	if at < 0 {
		return false
	}
	return strings.EqualFold(cmdWithAt[at+1:], b.self.UserName)
}

func escapeHTML(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}

func formatUserMention(user *tgbotapi.User) string {
	if user == nil {
		return ""
	}

	if user.UserName != "" {
		return "@" + escapeHTML(user.UserName)
	}

	name := strings.TrimSpace(user.FirstName + " " + user.LastName)
	if name == "" {
		name = "user"
	}
	return fmt.Sprintf(`<a href="tg://user?id=%d">%s</a>`, user.ID, escapeHTML(name))
}
