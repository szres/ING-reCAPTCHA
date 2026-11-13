package bot

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"github.com/szres/ing-recaptcha/internal/bot/miniapp"
	"github.com/szres/ing-recaptcha/internal/config"
	"github.com/szres/ing-recaptcha/internal/database"
	"github.com/szres/ing-recaptcha/internal/i18n"
	"github.com/szres/ing-recaptcha/internal/imaging"
)

type Bot struct {
	api      TelegramAPI
	self     tgbotapi.User
	token    string
	db       *database.DB
	cfg      *config.Config
	composer *imaging.Composer
	i18n     *i18n.Translator

	// joinMsgCache buffers join service message IDs for the race condition where
	// new_chat_members arrives before handleChatMemberUpdate creates the DB record.
	// Key: "chatID:userID", Value: int (message ID)
	joinMsgCache sync.Map

	workerPool chan struct{}
	updatePool chan struct{}
	stopChan   chan struct{}
	wg         sync.WaitGroup

	// Optional Mini App server. Non-nil only when cfg.MiniAppEnabled is true.
	miniappSrv    *miniapp.Server
	miniappPoller *rawPoller
}

func New(cfg *config.Config, db *database.DB) (*Bot, error) {
	// Keep timeout above long-poll update timeout (60s) to avoid cancelling getUpdates early.
	httpClient := &http.Client{Timeout: 90 * time.Second}
	rawAPI, err := tgbotapi.NewBotAPIWithClient(cfg.TelegramBotToken, tgbotapi.APIEndpoint, httpClient)
	if err != nil {
		return nil, fmt.Errorf("failed to create bot API: %w", err)
	}

	log.Printf("Authorized on account %s", rawAPI.Self.UserName)

	translator, err := i18n.New(cfg.DefaultLanguage)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize i18n: %w", err)
	}

	return &Bot{
		api:        rawAPI,
		self:       rawAPI.Self,
		token:      rawAPI.Token,
		db:         db,
		cfg:        cfg,
		composer:   imaging.NewComposer(cfg.ImageCachePath),
		i18n:       translator,
		workerPool: make(chan struct{}, 5),
		updatePool: make(chan struct{}, 64),
		stopChan:   make(chan struct{}),
	}, nil
}

func (b *Bot) Start() error {
	if err := b.healthCheck(); err != nil {
		return fmt.Errorf("health check failed: %w", err)
	}

	// Check bot permissions
	log.Println("Checking bot permissions...")
	me, err := b.api.GetMe()
	if err != nil {
		return fmt.Errorf("failed to get bot info: %w", err)
	}
	log.Printf("Bot started as: @%s (ID: %d, Name: %s)", me.UserName, me.ID, me.FirstName)

	if err := b.registerCommands(); err != nil {
		log.Printf("[WARN] Failed to register bot commands with Telegram: %v", err)
	}

	allowed := []string{"message", "callback_query", "chat_member"}
	if b.cfg.MiniAppEnabled {
		allowed = append(allowed, "chat_join_request")
		if err := b.startMiniAppServer(); err != nil {
			return fmt.Errorf("start mini app server: %w", err)
		}
	}

	b.wg.Add(1)
	go b.cleanupExpiredVerifications()

	b.wg.Add(1)
	go b.cleanupJoinHistory()

	b.wg.Add(1)
	go b.cleanupFailureHistory()

	b.wg.Add(1)
	go b.ephemeralFallbackLoop()

	b.wg.Add(1)
	go b.autoDeleteLoop()

	b.wg.Add(1)
	go b.dailyStatsLoop()

	if b.cfg.MiniAppEnabled {
		b.wg.Add(1)
		go b.cleanupMiniAppSessions()
	}

	log.Println("Bot started, listening for updates...")
	log.Printf("Configuration: Images=%d, Required=%d, Timeout=%ds, Distractors=%d",
		b.cfg.VerifyImageCount, b.cfg.VerifyRequiredCorrect,
		b.cfg.VerifyTimeoutSeconds, b.cfg.VerifyDistractorCount)
	if b.cfg.MiniAppEnabled {
		log.Printf("Mini App enabled: listen=%s public=%s ttl=%ds", b.cfg.MiniAppListenAddr, b.cfg.MiniAppPublicURL, b.cfg.MiniAppSessionTTLSeconds)
	}

	// When Mini App is on we replace the v5 updates channel with our own
	// raw poller, because the v5 lib's ChatJoinRequest struct doesn't
	// expose query_id. Otherwise stay on the proven v5 long-poll path.
	if b.cfg.MiniAppEnabled {
		b.runRawUpdateLoop(allowed)
	} else {
		u := tgbotapi.NewUpdate(0)
		u.Timeout = 60
		u.AllowedUpdates = allowed
		updates := b.api.GetUpdatesChan(u)
		for update := range updates {
			b.updatePool <- struct{}{}
			go func(update tgbotapi.Update) {
				defer func() {
					if r := recover(); r != nil {
						log.Printf("[ERROR] Panic in update handler: %v", r)
					}
					<-b.updatePool
				}()
				b.handleUpdate(update)
			}(update)
		}
	}

	return nil
}

// runRawUpdateLoop dispatches updates from the raw poller. chat_join_request
// is dispatched synchronously inside the poller goroutine (so a crash can't
// lose updates with a live 10-second query_id window); every other update
// type is re-packed into v5.Update and handed to the existing fan-out.
func (b *Bot) runRawUpdateLoop(allowed []string) {
	b.miniappPoller = newRawPoller(b.token, allowed, b.handleChatJoinRequest)
	defer b.miniappPoller.Stop()
	for ru := range b.miniappPoller.Updates() {
		ru := ru
		b.updatePool <- struct{}{}
		go func() {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("[ERROR] Panic in update handler: %v", r)
				}
				<-b.updatePool
			}()
			b.handleRawUpdate(ru)
		}()
	}
}

// handleRawUpdate handles non-join-request updates from the raw poller by
// re-packing them into v5.Update so the existing typed handlers can run
// unchanged. chat_join_request updates never reach here — the poller
// dispatches them synchronously instead.
func (b *Bot) handleRawUpdate(ru rawUpdate) {
	if ru.ChatJoinRequest != nil {
		// Defense in depth: should not happen because the poller dispatches
		// join requests synchronously. If it ever does, route it to the
		// real handler so the update isn't dropped.
		b.handleChatJoinRequest(ru.ChatJoinRequest)
		return
	}
	u := tgbotapi.Update{
		UpdateID:      ru.UpdateID,
		Message:       ru.Message,
		EditedMessage: ru.EditedMessage,
		CallbackQuery: ru.CallbackQuery,
		ChatMember:    ru.ChatMember,
		MyChatMember:  ru.MyChatMember,
	}
	b.handleUpdate(u)
}

// startMiniAppServer constructs and launches the Mini App HTTP server.
// Closures inject bot-side capabilities (challenge composition, Telegram
// API calls, welcome message) without dragging the bot package into miniapp.
func (b *Bot) startMiniAppServer() error {
	if b.cfg.MiniAppPublicURL == "" {
		return fmt.Errorf("MINIAPP_PUBLIC_URL is required when MINIAPP_ENABLED=true")
	}
	deps := miniapp.Deps{
		DB:              b.db,
		BotToken:        b.token,
		SessionTTL:      time.Duration(b.cfg.MiniAppSessionTTLSeconds) * time.Second,
		RequiredCorrect: b.cfg.VerifyRequiredCorrect,
		ComposeChallenge: func() (string, []string, []string, error) {
			return b.composeChallenge()
		},
		CleanupTempFile: func(path string) {
			if err := b.composer.CleanupTempFile(path); err != nil {
				log.Printf("[DEBUG] cleanup mini app temp file %s: %v", path, err)
			}
		},
		ApproveJoinRequest: func(ctx context.Context, queryID string) error {
			return b.answerChatJoinRequestQuery(ctx, queryID, "approve")
		},
		DeclineJoinRequest: func(ctx context.Context, queryID string) error {
			return b.answerChatJoinRequestQuery(ctx, queryID, "decline")
		},
		ApproveJoinLegacy: func(_ context.Context, chatID, userID int64) error {
			_, err := b.api.Request(tgbotapi.ApproveChatJoinRequestConfig{
				ChatConfig: tgbotapi.ChatConfig{ChatID: chatID},
				UserID:     userID,
			})
			return err
		},
		DeclineJoinLegacy: func(_ context.Context, chatID, userID int64) error {
			_, err := b.api.Request(tgbotapi.DeclineChatJoinRequest{
				ChatConfig: tgbotapi.ChatConfig{ChatID: chatID},
				UserID:     userID,
			})
			return err
		},
		OnApproved: b.onMiniAppApproved,
		OnDeclined: func(chatID, userID int64, reason string) {
			b.notifyVerificationFailure(chatID, userID, reason)
		},
	}
	b.miniappSrv = miniapp.New(b.cfg.MiniAppListenAddr, deps)
	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		if err := b.miniappSrv.Start(); err != nil {
			log.Printf("[ERROR] mini app server exited: %v", err)
		}
	}()
	return nil
}

// onMiniAppApproved is invoked from the Mini App server immediately after a
// successful answerChatJoinRequestQuery(approve). It sends the optional
// silent welcome message in the group, mirroring the in-group flow.
func (b *Bot) onMiniAppApproved(chatID, userID int64) {
	if !b.cfg.WelcomeMessageInGroup {
		return
	}
	user, err := b.api.GetChatMember(tgbotapi.GetChatMemberConfig{
		ChatConfigWithUser: tgbotapi.ChatConfigWithUser{ChatID: chatID, UserID: userID},
	})
	var mention string
	if err == nil {
		mention = formatUserMention(user.User)
	} else {
		mention = fmt.Sprintf("<code>%d</code>", userID)
	}
	text := b.i18n.T(b.cfg.DefaultLanguage, "verify_passed", mention)
	msg := tgbotapi.NewMessage(chatID, text)
	msg.ParseMode = "HTML"
	msg.DisableNotification = b.cfg.VerifySilentNotifications
	b.sendPassNotice(msg)
}

// cleanupMiniAppSessions ticks alongside the other DB cleanup loops to:
//  1. Apply each chat's join_policy to any awaiting_dm session whose 5-min
//     TTL has expired (the user didn't /start the bot in time).
//  2. Expire stale standard pending sessions whose Mini App was opened
//     but never solved.
//  3. Drop in-memory challenges that outlived their session row.
//  4. Delete session rows older than a day so the table stays small.
func (b *Bot) cleanupMiniAppSessions() {
	defer b.wg.Done()
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-b.stopChan:
			return
		case <-ticker.C:
			b.applyExpiredAwaitingDMPolicies()
			if n, err := b.db.ExpireStaleMiniAppSessions(); err != nil {
				log.Printf("[WARN] expire stale mini app sessions: %v", err)
			} else if n > 0 {
				log.Printf("[INFO] expired %d stale mini app session(s)", n)
			}
			if err := b.db.CleanupOldMiniAppSessions(24 * 3600); err != nil {
				log.Printf("[WARN] cleanup mini app sessions: %v", err)
			}
			if b.miniappSrv != nil {
				b.miniappSrv.ExpireStaleChallenges()
			}
		}
	}
}

// applyExpiredAwaitingDMPolicies walks every session whose TTL has
// passed AND whose join request can only be resolved by the bot
// (awaiting_dm + pending-with-empty-query_id) and dispatches the
// chat's configured fallback policy. Modern-flow pending sessions
// (Telegram-native popup) are NOT included; those expire silently
// via ExpireStaleMiniAppSessions and the request stays in Telegram's
// queue for a human admin.
func (b *Bot) applyExpiredAwaitingDMPolicies() {
	stale, err := b.db.ListExpiredForPolicyFallback()
	if err != nil {
		log.Printf("[WARN] list expired for policy fallback: %v", err)
		return
	}
	for _, s := range stale {
		b.applyJoinPolicyFallback(s.ChatID, s.UserID, s.Nonce, "ttl-expired")
	}
}

// applyJoinPolicyFallback dispatches the chat's configured join_policy for
// a session that can't be completed via the Mini App flow (either because
// the TTL expired or because we couldn't deliver the DM button). The
// session is transitioned to 'expired' after the dispatch — even on
// failure — so a transient Bot API error doesn't trap the row forever.
//
// reason is a short tag for the log line ("ttl-expired" or "dm-send-failed")
// so operators can tell apart sweeper-driven and synchronous fallbacks.
func (b *Bot) applyJoinPolicyFallback(chatID, userID int64, nonce, reason string) {
	policy, err := b.db.GetChatJoinPolicy(chatID)
	if err != nil {
		log.Printf("[WARN] read join policy chat=%d: %v (using default 'approve')", chatID, err)
		policy = ""
	}
	if policy == "" {
		policy = database.JoinPolicyApprove
	}
	log.Printf("[INFO] applying join policy: chat=%d user=%d nonce=%s reason=%s policy=%s",
		chatID, userID, nonce, reason, policy)
	switch policy {
	case database.JoinPolicyApprove:
		if _, err := b.api.Request(tgbotapi.ApproveChatJoinRequestConfig{
			ChatConfig: tgbotapi.ChatConfig{ChatID: chatID},
			UserID:     userID,
		}); err != nil {
			log.Printf("[ERROR] policy=approve approveChatJoinRequest chat=%d user=%d: %v", chatID, userID, err)
		}
	case database.JoinPolicyDecline:
		if _, err := b.api.Request(tgbotapi.DeclineChatJoinRequest{
			ChatConfig: tgbotapi.ChatConfig{ChatID: chatID},
			UserID:     userID,
		}); err != nil {
			log.Printf("[ERROR] policy=decline declineChatJoinRequest chat=%d user=%d: %v", chatID, userID, err)
		}
	case database.JoinPolicyDismiss:
		// Intentionally do nothing: the request stays pending for admins.
	}
	if _, mErr := b.db.MarkMiniAppSession(nonce, "expired"); mErr != nil {
		log.Printf("[ERROR] mark session expired chat=%d nonce=%s: %v", chatID, nonce, mErr)
	}
	// Timeouts are "verification never completed" — surface to the same
	// notification hook as an outright decline so an admin watching this
	// chat can see abandoned attempts.
	b.notifyVerificationFailure(chatID, userID, "ttl-"+reason)
}

// notifyVerificationFailure DMs each user in FailNotifyUserIDs about a
// failed/timed-out verification, but only if chatID is one of the
// watched groups (FailNotifyChatIDs). No-op when either list is empty.
//
// The applicant's username is looked up via getChatMember at notify
// time (accurate but costs one API call per notification). If the
// lookup fails we still send the notification with just the user_id
// so the admin at least learns something happened.
//
// Kept intentionally best-effort: notification failures are logged but
// never propagated — they must not affect the join-decision path.
func (b *Bot) notifyVerificationFailure(chatID, userID int64, reason string) {
	if len(b.cfg.FailNotifyChatIDs) == 0 || len(b.cfg.FailNotifyUserIDs) == 0 {
		return
	}
	watched := false
	for _, c := range b.cfg.FailNotifyChatIDs {
		if c == chatID {
			watched = true
			break
		}
	}
	if !watched {
		return
	}

	username := ""
	firstName := ""
	if member, err := b.api.GetChatMember(tgbotapi.GetChatMemberConfig{
		ChatConfigWithUser: tgbotapi.ChatConfigWithUser{
			ChatID: chatID,
			UserID: userID,
		},
	}); err == nil && member.User != nil {
		username = member.User.UserName
		firstName = member.User.FirstName
	}

	chatTitle := ""
	if chat, err := b.api.GetChat(tgbotapi.ChatInfoConfig{ChatConfig: tgbotapi.ChatConfig{ChatID: chatID}}); err == nil {
		chatTitle = chat.Title
	}

	handle := "<no username>"
	if username != "" {
		handle = "@" + username
	}
	nameLine := handle
	if firstName != "" {
		nameLine = firstName + " (" + handle + ")"
	}
	titleLine := chatTitle
	if titleLine == "" {
		titleLine = fmt.Sprintf("chat %d", chatID)
	}

	body := fmt.Sprintf(
		"⚠️ Verification %s in <b>%s</b>\n\nUser: %s\nUser ID: <code>%d</code>",
		escapeHTML(reason),
		escapeHTML(titleLine),
		escapeHTML(nameLine),
		userID,
	)
	for _, recipient := range b.cfg.FailNotifyUserIDs {
		msg := tgbotapi.NewMessage(recipient, body)
		msg.ParseMode = "HTML"
		msg.DisableWebPagePreview = true
		if _, err := b.api.Send(msg); err != nil {
			log.Printf("[WARN] failure notify to user=%d for chat=%d applicant=%d: %v",
				recipient, chatID, userID, err)
		}
	}
}

func (b *Bot) Stop() {
	log.Println("Stopping bot...")
	if b.miniappPoller != nil {
		b.miniappPoller.Stop()
	} else {
		b.api.StopReceivingUpdates()
	}
	if b.miniappSrv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := b.miniappSrv.Stop(ctx); err != nil {
			log.Printf("[WARN] mini app shutdown: %v", err)
		}
		cancel()
	}
	close(b.stopChan)
	b.wg.Wait()
	log.Println("Bot stopped")
}

func (b *Bot) handleUpdate(update tgbotapi.Update) {
	if update.ChatMember != nil {
		b.handleChatMemberUpdate(update.ChatMember)
		return
	}

	if update.CallbackQuery != nil {
		b.handleCallbackQuery(update.CallbackQuery)
		return
	}

	if update.Message != nil {
		b.handleMessage(update.Message)
		return
	}
}

func (b *Bot) cleanupExpiredVerifications() {
	defer b.wg.Done()

	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-b.stopChan:
			return
		case <-ticker.C:
			expired, err := b.db.GetExpiredVerifications()
			if err != nil {
				log.Printf("Error getting expired verifications: %v", err)
				continue
			}

			for _, pv := range expired {
				b.wg.Add(1)
				go func(pv *database.PendingVerification) {
					defer b.wg.Done()
					b.handleExpiredVerification(pv)
				}(pv)
			}
		}
	}
}

func (b *Bot) handleExpiredVerification(pv *database.PendingVerification) {
	log.Printf("[DEBUG] Handling expired verification for user %d in chat %d", pv.UserID, pv.ChatID)

	// Claim the record first to prevent double-processing if this goroutine
	// takes longer than the cleanup ticker interval (10s).
	claimed, err := b.db.ClaimPendingVerification(pv.ChatID, pv.UserID, pv.ExpiresAt)
	if err != nil {
		log.Printf("[ERROR] Failed to claim pending verification for user %d: %v", pv.UserID, err)
		return
	}
	if !claimed {
		log.Printf("[DEBUG] Pending verification for user %d already claimed by another process, skipping", pv.UserID)
		return
	}
	log.Printf("[DEBUG] Claimed pending verification record for user %d", pv.UserID)

	if err := b.db.RecordVerificationEvent(database.VerificationEvent{
		ChatID:      pv.ChatID,
		UserID:      pv.UserID,
		Outcome:     "expired",
		IsTest:      pv.IsTest,
		UserLang:    pv.UserLang,
		Regenerated: pv.Regenerated,
		OccurredAt:  time.Now().Unix(),
	}); err != nil {
		log.Printf("[ERROR] Failed to record expired verification event: %v", err)
	}

	// Delete verification message (regular or ephemeral) if any.
	b.deleteVerificationMessageFor(pv, "sweeper-expired")

	// Check if user has previous failure
	hasPreviousFailure, err := b.db.HasPreviousFailure(pv.ChatID, pv.UserID)
	if err != nil {
		log.Printf("[ERROR] Failed to check previous failure: %v", err)
		hasPreviousFailure = false
	}

	if hasPreviousFailure {
		// Second failure - kick and ban permanently
		log.Printf("[INFO] User %d has previous failure, kicking with permanent ban", pv.UserID)
		kickConfig := tgbotapi.KickChatMemberConfig{
			ChatMemberConfig: tgbotapi.ChatMemberConfig{
				ChatID: pv.ChatID,
				UserID: pv.UserID,
			},
			// UntilDate = 0 means permanent ban
		}
		resp, err := b.api.Request(kickConfig)
		if err != nil {
			// Keep the strike: it's the only record that this user is due a
			// permanent ban, so their next timeout attempts it again rather
			// than starting over with a tempban.
			log.Printf("[ERROR] Failed to permanently ban user %d in chat %d after 2nd timeout, keeping strike: %v", pv.UserID, pv.ChatID, err)
		} else {
			log.Printf("[DEBUG] Successfully kicked and banned user %d, response: %+v", pv.UserID, resp)
			if err := b.db.DeleteUserJoinHistory(pv.ChatID, pv.UserID); err != nil {
				log.Printf("[ERROR] Failed to delete join history: %v", err)
			}
			if err := b.db.ClearVerificationFailure(pv.ChatID, pv.UserID); err != nil {
				log.Printf("[ERROR] Failed to clear failure record: %v", err)
			}
			log.Printf("[INFO] User %d timed out (2nd time) in chat %d and was permanently banned", pv.UserID, pv.ChatID)
		}
		b.deleteJoinMessage(pv.ChatID, pv)
	} else {
		// First failure - tempban (kick with UntilDate). Telegram auto-lifts
		// the ban when UntilDate passes, so we do NOT call unbanChatMember.
		// Manual unban was found to leave the user's pre-scheduled messages
		// queued and they would fire after the unban, bypassing verification
		// entirely. A real ban (even short) clears the queued scheduled
		// messages, and the auto-expire lets legitimate users rejoin freely.
		tempbanUntil := time.Now().Add(time.Duration(b.cfg.VerifyTempbanSeconds) * time.Second).Unix()
		log.Printf("[INFO] User %d first timeout, tempbanning until %d (%ds)", pv.UserID, tempbanUntil, b.cfg.VerifyTempbanSeconds)

		kickConfig := tgbotapi.KickChatMemberConfig{
			ChatMemberConfig: tgbotapi.ChatMemberConfig{
				ChatID: pv.ChatID,
				UserID: pv.UserID,
			},
			UntilDate: tempbanUntil,
		}
		resp, err := b.api.Request(kickConfig)
		if err != nil {
			log.Printf("[ERROR] Failed to tempban user %d from chat %d: %v", pv.UserID, pv.ChatID, err)
		} else {
			log.Printf("[DEBUG] Successfully tempbanned user %d, response: %+v", pv.UserID, resp)

			// Only record failure after kick succeeds; if user was already removed by
			// another anti-spam bot, do not poison local failure state.
			if err := b.db.RecordVerificationFailure(pv.ChatID, pv.UserID); err != nil {
				log.Printf("[ERROR] Failed to record verification failure: %v", err)
			}

			if err := b.db.DeleteUserJoinHistory(pv.ChatID, pv.UserID); err != nil {
				log.Printf("[ERROR] Failed to delete join history: %v", err)
			}
		}

		log.Printf("[INFO] User %d timed out (1st time) in chat %d and was tempbanned (%ds)", pv.UserID, pv.ChatID, b.cfg.VerifyTempbanSeconds)
		b.deleteJoinMessage(pv.ChatID, pv)
	}
}

func (b *Bot) cleanupJoinHistory() {
	defer b.wg.Done()

	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()

	for {
		select {
		case <-b.stopChan:
			return
		case <-ticker.C:
			keepSeconds := b.cfg.RejoinCooldownSeconds * 2
			if keepSeconds < 3600 {
				keepSeconds = 3600
			}
			if err := b.db.CleanupOldJoinHistory(keepSeconds); err != nil {
				log.Printf("Error cleaning up join history: %v", err)
			}
		}
	}
}

func (b *Bot) cleanupFailureHistory() {
	defer b.wg.Done()

	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()

	for {
		select {
		case <-b.stopChan:
			return
		case <-ticker.C:
			// Clean up failure records older than 7 days
			if err := b.db.CleanupOldFailures(7 * 24 * 3600); err != nil {
				log.Printf("[ERROR] Error cleaning up failure history: %v", err)
			} else {
				log.Printf("[DEBUG] Cleaned up old failure records")
			}
		}
	}
}

// registerCommands publishes the bot's command list to Telegram so its
// autocomplete UI offers /start@bot and /help@bot inside groups. Without
// this the client never suggests the @username form and users only ever
// type plain /start, which the bot now ignores in groups.
func (b *Bot) registerCommands() error {
	groupCmds := []tgbotapi.BotCommand{
		{Command: "start", Description: "Show welcome message and check setup"},
		{Command: "help", Description: "Show help"},
		{Command: "joinpolicy", Description: "Configure fallback for join requests without query_id"},
		{Command: "setting", Description: "Configure per-group verification settings (ephemeral, adminview)"},
		{Command: "setup", Description: "Check bot permissions and see setup instructions"},
	}
	privateCmds := []tgbotapi.BotCommand{
		{Command: "start", Description: "Show welcome message"},
		{Command: "help", Description: "Show help"},
		{Command: "test", Description: "Run a test verification"},
		{Command: "myid", Description: "Get your Telegram User ID"},
		{Command: "setlang", Description: "Set interface language"},
	}

	if _, err := b.api.Request(tgbotapi.NewSetMyCommandsWithScope(
		tgbotapi.NewBotCommandScopeAllGroupChats(), groupCmds...)); err != nil {
		return fmt.Errorf("group scope: %w", err)
	}
	if _, err := b.api.Request(tgbotapi.NewSetMyCommandsWithScope(
		tgbotapi.NewBotCommandScopeAllPrivateChats(), privateCmds...)); err != nil {
		return fmt.Errorf("private scope: %w", err)
	}

	log.Printf("[INFO] Registered %d group commands and %d private commands with Telegram",
		len(groupCmds), len(privateCmds))
	return nil
}
