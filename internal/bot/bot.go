package bot

import (
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

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

	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60
	u.AllowedUpdates = []string{"message", "callback_query", "chat_member"}

	updates := b.api.GetUpdatesChan(u)

	b.wg.Add(1)
	go b.cleanupExpiredVerifications()

	b.wg.Add(1)
	go b.cleanupJoinHistory()

	b.wg.Add(1)
	go b.cleanupFailureHistory()

	b.wg.Add(1)
	go b.dailyStatsLoop()

	log.Println("Bot started, listening for updates...")
	log.Printf("Configuration: Images=%d, Required=%d, Timeout=%ds, Distractors=%d",
		b.cfg.VerifyImageCount, b.cfg.VerifyRequiredCorrect,
		b.cfg.VerifyTimeoutSeconds, b.cfg.VerifyDistractorCount)

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

	return nil
}

func (b *Bot) Stop() {
	log.Println("Stopping bot...")
	b.api.StopReceivingUpdates()
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

	// Delete verification message if exists
	if pv.MessageID.Valid {
		messageID := int(pv.MessageID.Int64)
		log.Printf("[DEBUG] Attempting to delete message %d in chat %d", messageID, pv.ChatID)
		deleteMsg := tgbotapi.NewDeleteMessage(pv.ChatID, messageID)
		resp, err := b.api.Request(deleteMsg)
		if err != nil {
			log.Printf("[ERROR] Failed to delete verification message %d in chat %d: %v", messageID, pv.ChatID, err)
		} else {
			log.Printf("[DEBUG] Successfully deleted message %d, response: %+v", messageID, resp)
		}
	} else {
		log.Printf("[DEBUG] No message ID to delete for user %d in chat %d", pv.UserID, pv.ChatID)
	}

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
			log.Printf("[ERROR] Failed to kick and ban user %d: %v", pv.UserID, err)
		} else {
			log.Printf("[DEBUG] Successfully kicked and banned user %d, response: %+v", pv.UserID, resp)

			// Only delete join history if kick succeeded
			if err := b.db.DeleteUserJoinHistory(pv.ChatID, pv.UserID); err != nil {
				log.Printf("[ERROR] Failed to delete join history: %v", err)
			}
		}

		// Clear failure record after permanent ban
		if err := b.db.ClearVerificationFailure(pv.ChatID, pv.UserID); err != nil {
			log.Printf("[ERROR] Failed to clear failure record: %v", err)
		}

		log.Printf("[INFO] User %d timed out (2nd time) in chat %d and was permanently banned", pv.UserID, pv.ChatID)
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
