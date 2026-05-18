package bot

import (
	"database/sql"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"github.com/szres/ing-recaptcha/internal/database"
)

var httpClient = &http.Client{
	Timeout: 30 * time.Second,
}

func (b *Bot) startVerification(chatID, userID int64, user *tgbotapi.User) error {
	return b.startVerificationInternal(chatID, userID, user, false)
}

func (b *Bot) startTestVerification(chatID, userID int64, user *tgbotapi.User) error {
	return b.startVerificationInternal(chatID, userID, user, true)
}

func (b *Bot) startVerificationInternal(chatID, userID int64, user *tgbotapi.User, isTest bool) error {
	log.Printf("[DEBUG] Starting verification for user %d in chat %d (test mode: %v)", userID, chatID, isTest)
	userLang := b.getUserLang(user)

	// Check if there's already a pending verification for this user
	// Purpose: Only to get old MessageID for cleanup, data overwrite is handled by CreatePendingVerification's UPSERT
	existingPV, err := b.db.GetPendingVerification(chatID, userID)
	if err != nil {
		log.Printf("[WARN] Failed to check existing verification: %v", err)
	}

	if existingPV != nil && existingPV.MessageID.Valid {
		oldMessageID := int(existingPV.MessageID.Int64)
		log.Printf("[DEBUG] Found existing verification, deleting old message %d", oldMessageID)

		// Delete old message to avoid confusion
		deleteMsg := tgbotapi.NewDeleteMessage(chatID, oldMessageID)
		if _, err := b.api.Request(deleteMsg); err != nil {
			// Ignore "message not found" errors (user may have deleted it)
			log.Printf("[DEBUG] Failed to delete old verification message (may already be deleted): %v", err)
		}
	}

	// Pre-flight: enough image sets? In production, auto-approve on shortage; in test, fail.
	setCount, err := b.db.GetImageSetCount()
	if err != nil {
		return fmt.Errorf("failed to get image set count: %w", err)
	}
	minSets := b.cfg.VerifyImageCount + b.cfg.VerifyDistractorCount
	log.Printf("[DEBUG] Image set count: %d (minimum required: %d)", setCount, minSets)
	if setCount < minSets {
		if isTest {
			return fmt.Errorf(b.t(user, "err_not_enough_sets", setCount, minSets))
		}
		log.Printf("[WARN] Not enough image sets (%d < %d), auto-approving user", setCount, minSets)
		return b.unrestrictUser(chatID, userID)
	}

	composedPath, correctLabels, allOptions, err := b.composeChallenge()
	if err != nil {
		return fmt.Errorf("failed to compose challenge: %w", err)
	}
	defer b.composer.CleanupTempFile(composedPath)

	// Create pending verification record (starts at challenge_version=0, regenerated=0)
	expiresAt := time.Now().Add(time.Duration(b.cfg.VerifyTimeoutSeconds) * time.Second)
	log.Printf("[DEBUG] Creating pending verification, expires at: %v", expiresAt)
	if err := b.db.CreatePendingVerification(chatID, userID, correctLabels, expiresAt, isTest, userLang); err != nil {
		return fmt.Errorf("failed to create pending verification: %w", err)
	}

	// Group chats get admin moderation controls (Approve / Ban). Test verifications
	// run in private chat (or via /test in a group by a bot admin) and never need
	// them — they're not real entries.
	showAdminControls := chatID < 0 && !isTest
	keyboard := b.buildVerificationKeyboard(userID, 0, 0, allOptions, userLang, true, showAdminControls)

	caption := b.t(user, "verify_welcome",
		formatUserMention(user),
		b.cfg.VerifyTimeoutSeconds,
		b.cfg.VerifyImageCount,
	)

	log.Printf("[DEBUG] Sending verification message to chat %d", chatID)
	photo := tgbotapi.NewPhoto(chatID, tgbotapi.FilePath(composedPath))
	photo.Caption = caption
	photo.ParseMode = "HTML"
	photo.ReplyMarkup = keyboard

	sentMsg, err := b.api.Send(photo)
	if err != nil {
		return fmt.Errorf("failed to send verification message: %w", err)
	}

	log.Printf("[DEBUG] Verification message sent, message ID: %d", sentMsg.MessageID)

	if err := b.db.UpdateVerificationMessageID(chatID, userID, sentMsg.MessageID); err != nil {
		log.Printf("[ERROR] Failed to update message ID: %v", err)
	} else {
		log.Printf("[DEBUG] Updated message ID in database")
	}

	return nil
}

// composeChallenge picks random sets, downloads/loads their images, composes the
// verification image, and assembles a shuffled label set (correct + distractors).
// Caller is responsible for `b.composer.CleanupTempFile(composedPath)`.
func (b *Bot) composeChallenge() (composedPath string, correctLabels []string, allOptions []string, err error) {
	challengeSets, err := b.db.GetRandomImageSets(b.cfg.VerifyImageCount)
	if err != nil {
		return "", nil, nil, fmt.Errorf("get random image sets: %w", err)
	}

	var imagePaths []string
	for i, set := range challengeSets {
		log.Printf("[DEBUG] Getting random image from set %d: %s", i+1, set.Label)
		img, err := b.db.GetRandomImageFromSet(set.ID)
		if err != nil || img == nil {
			return "", nil, nil, fmt.Errorf("get image from set %s: %w", set.Label, err)
		}
		imgPath := img.FilePath.String
		if !img.FilePath.Valid || imgPath == "" {
			log.Printf("[DEBUG] Image not cached, downloading file_id: %s", img.FileID)
			imgPath, err = b.downloadAndCacheImage(img.FileID, img.ID)
			if err != nil {
				return "", nil, nil, fmt.Errorf("download image: %w", err)
			}
		}
		imagePaths = append(imagePaths, imgPath)
		correctLabels = append(correctLabels, set.Label)
	}
	log.Printf("[DEBUG] Correct labels for verification: %v", correctLabels)

	// Compose under bounded concurrency.
	select {
	case b.workerPool <- struct{}{}:
	case <-time.After(5 * time.Second):
		return "", nil, nil, fmt.Errorf("compose worker pool is busy")
	}
	defer func() { <-b.workerPool }()

	composedPath, err = b.composer.ComposeVerificationImage(imagePaths)
	if err != nil {
		return "", nil, nil, fmt.Errorf("compose verification image: %w", err)
	}

	allSets, err := b.db.GetAllImageSets()
	if err != nil {
		b.composer.CleanupTempFile(composedPath)
		return "", nil, nil, fmt.Errorf("get all image sets: %w", err)
	}
	correctMap := make(map[string]bool, len(correctLabels))
	for _, l := range correctLabels {
		correctMap[l] = true
	}
	var distractorLabels []string
	for _, set := range allSets {
		if !correctMap[set.Label] {
			distractorLabels = append(distractorLabels, set.Label)
		}
	}
	rand.Shuffle(len(distractorLabels), func(i, j int) {
		distractorLabels[i], distractorLabels[j] = distractorLabels[j], distractorLabels[i]
	})
	if len(distractorLabels) > b.cfg.VerifyDistractorCount {
		distractorLabels = distractorLabels[:b.cfg.VerifyDistractorCount]
	}

	allOptions = append(correctLabels, distractorLabels...)
	rand.Shuffle(len(allOptions), func(i, j int) {
		allOptions[i], allOptions[j] = allOptions[j], allOptions[i]
	})
	return composedPath, correctLabels, allOptions, nil
}

func (b *Bot) buildVerificationKeyboard(userID int64, step, version int, options []string, userLang string, showRegenerate, showAdminControls bool) tgbotapi.InlineKeyboardMarkup {
	var rows [][]tgbotapi.InlineKeyboardButton

	for i := 0; i < len(options); i += 2 {
		var row []tgbotapi.InlineKeyboardButton
		for j := i; j < i+2 && j < len(options); j++ {
			callbackData := fmt.Sprintf("v:%d:%d:%d:%s", userID, version, step, options[j])
			displayLabel := b.getDisplayLabelBySetLabel(options[j], userLang)
			row = append(row, tgbotapi.NewInlineKeyboardButtonData(displayLabel, callbackData))
		}
		rows = append(rows, row)
	}

	if showRegenerate {
		rows = append(rows, []tgbotapi.InlineKeyboardButton{
			tgbotapi.NewInlineKeyboardButtonData(
				b.i18n.T(userLang, "verify_regenerate_button"),
				fmt.Sprintf("r:%d:%d", userID, version),
			),
		})
	}

	if showAdminControls {
		rows = append(rows, []tgbotapi.InlineKeyboardButton{
			tgbotapi.NewInlineKeyboardButtonData(
				b.i18n.T(userLang, "verify_admin_approve_button"),
				fmt.Sprintf("a:%d:%d", userID, version),
			),
			tgbotapi.NewInlineKeyboardButtonData(
				b.i18n.T(userLang, "verify_admin_ban_button"),
				fmt.Sprintf("b:%d:%d", userID, version),
			),
		})
	}

	return tgbotapi.NewInlineKeyboardMarkup(rows...)
}

func (b *Bot) getDisplayLabelBySetLabel(setLabel, userLang string) string {
	set, err := b.db.GetImageSetByLabel(setLabel)
	if err != nil {
		log.Printf("[WARN] Failed to resolve image set %q: %v", setLabel, err)
		return setLabel
	}
	if set == nil {
		log.Printf("[WARN] Image set not found for label %q", setLabel)
		return setLabel
	}

	labels, err := b.db.GetImageSetLabels(set.ID)
	if err != nil {
		log.Printf("[WARN] Failed to get labels for set %d: %v", set.ID, err)
		return set.Label
	}

	return pickLocalizedLabel(labels, userLang, set.Label)
}

// pickLocalizedLabel returns the label that matches the user's language.
// Falls back to the set's raw label so an English client never sees Chinese
// (and vice versa) when a translation is missing.
func pickLocalizedLabel(labels map[string]string, userLang, fallback string) string {
	if label, ok := labels[userLang]; ok && label != "" {
		return label
	}
	return fallback
}

func (b *Bot) handleCallbackQuery(query *tgbotapi.CallbackQuery) {
	data := query.Data
	log.Printf("[DEBUG] Received callback query from user %d: %s", query.From.ID, data)

	switch {
	case strings.HasPrefix(data, "v:"):
		b.handleAnswerCallback(query)
	case strings.HasPrefix(data, "r:"):
		b.handleRegenerateCallback(query)
	case strings.HasPrefix(data, "a:"):
		b.handleAdminApproveCallback(query)
	case strings.HasPrefix(data, "b:"):
		b.handleAdminBanCallback(query)
	default:
		log.Printf("[DEBUG] Unknown callback prefix, ignoring: %s", data)
	}
}

// parseAdminCallback extracts targetUserID and version from a:{user}:{ver}
// or b:{user}:{ver} callback data. Returns (0, 0, false) on malformed input.
func parseAdminCallback(data string) (int64, int, bool) {
	parts := strings.SplitN(data[2:], ":", 2)
	if len(parts) != 2 {
		return 0, 0, false
	}
	uid, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return 0, 0, false
	}
	ver, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, 0, false
	}
	return uid, ver, true
}

// adminGateCheck centralizes the common preconditions for both approve and ban:
// admin permission, existence of the pending row, and challenge_version match.
// Returns (pv, ok). When ok is false the caller has already sent the appropriate
// toast and should return.
func (b *Bot) adminGateCheck(query *tgbotapi.CallbackQuery, targetUserID int64, version int) (*database.PendingVerification, bool) {
	chatID := query.Message.Chat.ID

	if !b.isGroupAdmin(chatID, query.From.ID) {
		b.api.Request(tgbotapi.NewCallback(query.ID, b.t(query.From, "verify_admin_only")))
		return nil, false
	}

	pv, err := b.db.GetPendingVerification(chatID, targetUserID)
	if err != nil || pv == nil {
		log.Printf("[WARN] Admin action: no pending verification for user %d in chat %d: %v", targetUserID, chatID, err)
		b.api.Request(tgbotapi.NewCallback(query.ID, b.t(query.From, "verify_callback_expired")))
		return nil, false
	}
	if version != pv.ChallengeVersion {
		log.Printf("[WARN] Admin action stale callback ver=%d, current=%d", version, pv.ChallengeVersion)
		b.api.Request(tgbotapi.NewCallback(query.ID, b.t(query.From, "verify_callback_expired")))
		return nil, false
	}
	return pv, true
}

// handleAdminApproveCallback processes a:{user}:{ver}. A chat admin bypasses
// the CAPTCHA for the target user.
func (b *Bot) handleAdminApproveCallback(query *tgbotapi.CallbackQuery) {
	targetUserID, version, ok := parseAdminCallback(query.Data)
	if !ok {
		log.Printf("[WARN] Invalid admin-approve callback format: %s", query.Data)
		return
	}
	chatID := query.Message.Chat.ID
	adminID := query.From.ID

	pv, ok := b.adminGateCheck(query, targetUserID, version)
	if !ok {
		return
	}

	claimed, err := b.db.ClaimPendingVerification(chatID, targetUserID, pv.ExpiresAt)
	if err != nil {
		log.Printf("[ERROR] Admin approve claim failed: %v", err)
		b.api.Request(tgbotapi.NewCallback(query.ID, b.t(query.From, "verify_callback_expired")))
		return
	}
	if !claimed {
		b.api.Request(tgbotapi.NewCallback(query.ID, b.t(query.From, "verify_callback_expired")))
		return
	}

	if err := b.db.RecordVerificationEvent(database.VerificationEvent{
		ChatID:      chatID,
		UserID:      targetUserID,
		Outcome:     "passed",
		IsTest:      pv.IsTest,
		UserLang:    pv.UserLang,
		Regenerated: pv.Regenerated,
		AdminUserID: sql.NullInt64{Int64: adminID, Valid: true},
		OccurredAt:  time.Now().Unix(),
	}); err != nil {
		log.Printf("[ERROR] Failed to record admin-approve event: %v", err)
	}

	b.applyVerificationSuccess(chatID, targetUserID, query.Message.MessageID)
	b.sendMessage(chatID, b.t(query.From, "verify_admin_approved_msg"))
	b.api.Request(tgbotapi.NewCallback(query.ID, b.t(query.From, "verify_admin_approved_toast")))
	log.Printf("[INFO] Admin %d approved user %d in chat %d", adminID, targetUserID, chatID)
}

// handleAdminBanCallback processes b:{user}:{ver}. A chat admin permanently
// bans the target user (no rejoin), short-circuiting the CAPTCHA.
func (b *Bot) handleAdminBanCallback(query *tgbotapi.CallbackQuery) {
	targetUserID, version, ok := parseAdminCallback(query.Data)
	if !ok {
		log.Printf("[WARN] Invalid admin-ban callback format: %s", query.Data)
		return
	}
	chatID := query.Message.Chat.ID
	adminID := query.From.ID

	pv, ok := b.adminGateCheck(query, targetUserID, version)
	if !ok {
		return
	}

	claimed, err := b.db.ClaimPendingVerification(chatID, targetUserID, pv.ExpiresAt)
	if err != nil {
		log.Printf("[ERROR] Admin ban claim failed: %v", err)
		b.api.Request(tgbotapi.NewCallback(query.ID, b.t(query.From, "verify_callback_expired")))
		return
	}
	if !claimed {
		b.api.Request(tgbotapi.NewCallback(query.ID, b.t(query.From, "verify_callback_expired")))
		return
	}

	if err := b.db.RecordVerificationEvent(database.VerificationEvent{
		ChatID:      chatID,
		UserID:      targetUserID,
		Outcome:     "failed",
		IsTest:      pv.IsTest,
		UserLang:    pv.UserLang,
		Regenerated: pv.Regenerated,
		AdminUserID: sql.NullInt64{Int64: adminID, Valid: true},
		OccurredAt:  time.Now().Unix(),
	}); err != nil {
		log.Printf("[ERROR] Failed to record admin-ban event: %v", err)
	}

	// Permanent ban — no UntilDate, no unban. Mirrors the cleanup subset from
	// handleVerificationFailure's permanent-ban branch.
	kickConfig := tgbotapi.KickChatMemberConfig{
		ChatMemberConfig: tgbotapi.ChatMemberConfig{
			ChatID: chatID,
			UserID: targetUserID,
		},
	}
	if _, err := b.api.Request(kickConfig); err != nil {
		log.Printf("[ERROR] Admin ban: failed to kick user %d: %v", targetUserID, err)
	}

	if err := b.db.DeleteUserJoinHistory(chatID, targetUserID); err != nil {
		log.Printf("[ERROR] Admin ban: failed to delete join history: %v", err)
	}
	if err := b.db.ClearVerificationFailure(chatID, targetUserID); err != nil {
		log.Printf("[ERROR] Admin ban: failed to clear failure record: %v", err)
	}

	// Delete the verification message and the join service message.
	deleteVerifyMsg := tgbotapi.NewDeleteMessage(chatID, query.Message.MessageID)
	if _, err := b.api.Request(deleteVerifyMsg); err != nil {
		log.Printf("[DEBUG] Admin ban: failed to delete verification message (non-fatal): %v", err)
	}
	b.deleteJoinMessage(chatID, pv)

	b.sendMessage(chatID, b.t(query.From, "verify_admin_banned_msg"))
	b.api.Request(tgbotapi.NewCallback(query.ID, b.t(query.From, "verify_admin_banned_toast")))
	log.Printf("[INFO] Admin %d banned user %d in chat %d", adminID, targetUserID, chatID)
}

// handleAnswerCallback processes a v:{user}:{ver}:{step}:{label} button tap.
// Also accepts the legacy v:{user}:{step}:{label} format from in-flight
// keyboards that predate the regenerate feature; those are treated as ver=0
// to keep active verifications working through a deploy.
func (b *Bot) handleAnswerCallback(query *tgbotapi.CallbackQuery) {
	parts := strings.SplitN(query.Data[2:], ":", 4)

	var targetUserID int64
	var version, step int
	var selectedLabel string
	var err error

	switch len(parts) {
	case 4:
		if targetUserID, err = strconv.ParseInt(parts[0], 10, 64); err != nil {
			log.Printf("[ERROR] Failed to parse target user ID: %v", err)
			return
		}
		if version, err = strconv.Atoi(parts[1]); err != nil {
			log.Printf("[ERROR] Failed to parse version: %v", err)
			return
		}
		if step, err = strconv.Atoi(parts[2]); err != nil {
			log.Printf("[ERROR] Failed to parse step: %v", err)
			return
		}
		selectedLabel = parts[3]
	case 3:
		// Legacy format predating challenge_version; treat as ver=0 (DB default).
		if targetUserID, err = strconv.ParseInt(parts[0], 10, 64); err != nil {
			log.Printf("[ERROR] Failed to parse target user ID: %v", err)
			return
		}
		if step, err = strconv.Atoi(parts[1]); err != nil {
			log.Printf("[ERROR] Failed to parse step: %v", err)
			return
		}
		version = 0
		selectedLabel = parts[2]
	default:
		log.Printf("[WARN] Invalid answer callback format: %s", query.Data)
		return
	}

	if query.From.ID != targetUserID {
		log.Printf("[WARN] Callback from user %d but target is %d", query.From.ID, targetUserID)
		b.api.Request(tgbotapi.NewCallback(query.ID, b.t(query.From, "verify_callback_not_yours")))
		return
	}
	chatID := query.Message.Chat.ID

	log.Printf("[DEBUG] User %d selected label '%s' for step %d (ver %d) in chat %d", targetUserID, selectedLabel, step, version, chatID)

	pv, err := b.db.GetPendingVerification(chatID, targetUserID)
	if err != nil || pv == nil {
		log.Printf("[WARN] No pending verification for user %d in chat %d: %v", targetUserID, chatID, err)
		b.api.Request(tgbotapi.NewCallback(query.ID, b.t(query.From, "verify_callback_expired")))
		return
	}
	// Stale callback from a previous (regenerated-away) challenge → discard.
	if version != pv.ChallengeVersion {
		log.Printf("[WARN] Stale callback ver=%d, current=%d for user %d", version, pv.ChallengeVersion, targetUserID)
		b.api.Request(tgbotapi.NewCallback(query.ID, b.t(query.From, "verify_callback_expired")))
		return
	}
	if step != pv.CurrentStep {
		log.Printf("[WARN] Step mismatch: expected %d, got %d", pv.CurrentStep, step)
		b.api.Request(tgbotapi.NewCallback(query.ID, b.t(query.From, "verify_callback_order")))
		return
	}

	answers := append(pv.UserAnswers, selectedLabel)
	newStep := step + 1

	ok, err := b.db.UpdateVerificationStep(chatID, targetUserID, newStep, answers, version)
	if err != nil {
		log.Printf("[ERROR] Failed to update verification step: %v", err)
		return
	}
	if !ok {
		// Challenge regenerated between our read and write — discard this answer.
		log.Printf("[WARN] Challenge regenerated during answer flow for user %d, discarding", targetUserID)
		b.api.Request(tgbotapi.NewCallback(query.ID, b.t(query.From, "verify_callback_expired")))
		return
	}

	if newStep >= b.cfg.VerifyImageCount {
		log.Printf("[DEBUG] All answers collected, evaluating verification")
		b.evaluateVerification(query, pv, answers)
		return
	}

	caption := b.t(query.From, "verify_step_prompt", newStep, b.cfg.VerifyImageCount, newStep+1)

	// Reconstruct option list from current keyboard, skipping the regenerate row (r:).
	var options []string
	if query.Message.ReplyMarkup != nil {
		for _, row := range query.Message.ReplyMarkup.InlineKeyboard {
			for _, btn := range row {
				if btn.CallbackData == nil || !strings.HasPrefix(*btn.CallbackData, "v:") {
					continue
				}
				btnParts := strings.SplitN((*btn.CallbackData)[2:], ":", 4)
				if len(btnParts) == 4 {
					options = append(options, btnParts[3])
				}
			}
		}
	}

	userLang := b.getUserLang(query.From)
	showAdminControls := query.Message.Chat.Type != "private" && !pv.IsTest
	keyboard := b.buildVerificationKeyboard(targetUserID, newStep, version, options, userLang, !pv.Regenerated, showAdminControls)

	editMsg := tgbotapi.NewEditMessageCaption(chatID, query.Message.MessageID, caption)
	editMsg.ParseMode = "HTML"
	editMsg.ReplyMarkup = &keyboard
	if _, err := b.api.Send(editMsg); err != nil {
		log.Printf("[ERROR] Failed to update message caption: %v", err)
	}

	displayLabel := b.getDisplayLabelBySetLabel(selectedLabel, userLang)
	b.api.Request(tgbotapi.NewCallback(query.ID, b.t(query.From, "verify_callback_selected", displayLabel)))
}

// handleRegenerateCallback processes a r:{user}:{ver} button tap. Replaces the
// pending challenge with a freshly-composed one; consumes the one-time chance.
func (b *Bot) handleRegenerateCallback(query *tgbotapi.CallbackQuery) {
	parts := strings.SplitN(query.Data[2:], ":", 2)
	if len(parts) != 2 {
		log.Printf("[WARN] Invalid regenerate callback format: %s", query.Data)
		return
	}
	targetUserID, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		log.Printf("[ERROR] Failed to parse target user ID: %v", err)
		return
	}
	if query.From.ID != targetUserID {
		log.Printf("[WARN] Regenerate callback from user %d but target is %d", query.From.ID, targetUserID)
		b.api.Request(tgbotapi.NewCallback(query.ID, b.t(query.From, "verify_callback_not_yours")))
		return
	}
	chatID := query.Message.Chat.ID

	pv, err := b.db.GetPendingVerification(chatID, targetUserID)
	if err != nil || pv == nil {
		log.Printf("[WARN] Regenerate: no pending verification for user %d: %v", targetUserID, err)
		b.api.Request(tgbotapi.NewCallback(query.ID, b.t(query.From, "verify_callback_expired")))
		return
	}
	if pv.Regenerated {
		log.Printf("[INFO] Regenerate already used for user %d", targetUserID)
		b.api.Request(tgbotapi.NewCallback(query.ID, b.t(query.From, "verify_regenerate_used")))
		return
	}

	// Eagerly answer the callback so we don't blow Telegram's ~10s deadline
	// while we download/compose. Followup feedback is sent as a separate message
	// if regeneration fails after this point.
	if _, err := b.api.Request(tgbotapi.NewCallback(query.ID, b.t(query.From, "verify_regenerate_toast"))); err != nil {
		log.Printf("[DEBUG] Failed to ack regenerate callback (non-fatal): %v", err)
	}

	composedPath, correctLabels, allOptions, err := b.composeChallenge()
	if err != nil {
		log.Printf("[ERROR] Regenerate compose failed for user %d: %v", targetUserID, err)
		// Compose-first ordering means regenerated is still 0 — user can retry.
		b.sendMessage(chatID, b.t(query.From, "verify_regenerate_failed"))
		return
	}
	defer b.composer.CleanupTempFile(composedPath)

	newExpiresAt := time.Now().Add(time.Duration(b.cfg.VerifyTimeoutSeconds) * time.Second)
	claimed, err := b.db.RegenerateVerification(chatID, targetUserID, correctLabels, newExpiresAt)
	if err != nil {
		log.Printf("[ERROR] RegenerateVerification DB error for user %d: %v", targetUserID, err)
		b.sendMessage(chatID, b.t(query.From, "verify_regenerate_failed"))
		return
	}
	if !claimed {
		// Lost the race to a concurrent regenerate or the row was deleted.
		log.Printf("[INFO] Regenerate not claimed for user %d (concurrent or gone)", targetUserID)
		b.api.Request(tgbotapi.NewCallback(query.ID, b.t(query.From, "verify_regenerate_used")))
		return
	}

	userLang := b.getUserLang(query.From)
	// New version is pv.ChallengeVersion + 1 (RegenerateVerification incremented it).
	newVersion := pv.ChallengeVersion + 1
	caption := b.t(query.From, "verify_welcome",
		formatUserMention(query.From),
		b.cfg.VerifyTimeoutSeconds,
		b.cfg.VerifyImageCount,
	)
	showAdminControls := query.Message.Chat.Type != "private" && !pv.IsTest
	keyboard := b.buildVerificationKeyboard(targetUserID, 0, newVersion, allOptions, userLang, false, showAdminControls)

	photo := tgbotapi.NewPhoto(chatID, tgbotapi.FilePath(composedPath))
	photo.Caption = caption
	photo.ParseMode = "HTML"
	photo.ReplyMarkup = keyboard

	sentMsg, err := b.api.Send(photo)
	if err != nil {
		log.Printf("[ERROR] Failed to send regenerated verification message: %v", err)
		b.sendMessage(chatID, b.t(query.From, "verify_regenerate_failed"))
		return
	}

	// Persist new message_id BEFORE deleting the old one so the sweeper/handler
	// always finds a valid message reference if anything fails in between.
	if err := b.db.UpdateVerificationMessageID(chatID, targetUserID, sentMsg.MessageID); err != nil {
		log.Printf("[ERROR] Failed to update message ID after regenerate: %v", err)
	}

	// Best-effort delete of the old message; ignore failures.
	deleteMsg := tgbotapi.NewDeleteMessage(chatID, query.Message.MessageID)
	if _, err := b.api.Request(deleteMsg); err != nil {
		log.Printf("[DEBUG] Failed to delete old verification message (non-fatal): %v", err)
	}

	log.Printf("[INFO] Regenerated verification for user %d in chat %d (new ver=%d)", targetUserID, chatID, newVersion)
}

func (b *Bot) evaluateVerification(query *tgbotapi.CallbackQuery, pv *database.PendingVerification, answers []string) {
	chatID := query.Message.Chat.ID
	userID := pv.UserID

	log.Printf("[DEBUG] Evaluating verification for user %d in chat %d", userID, chatID)
	log.Printf("[DEBUG] User answers: %v", answers)
	log.Printf("[DEBUG] Correct labels: %v", pv.CorrectLabels)

	correctCount := 0
	firstWrongStep := 0
	for i, answer := range answers {
		if i < len(pv.CorrectLabels) && answer == pv.CorrectLabels[i] {
			correctCount++
			log.Printf("[DEBUG] Answer %d correct: %s", i+1, answer)
		} else if i < len(pv.CorrectLabels) {
			if firstWrongStep == 0 {
				firstWrongStep = i + 1
			}
			log.Printf("[DEBUG] Answer %d incorrect: got %s, expected %s", i+1, answer, pv.CorrectLabels[i])
		}
	}

	log.Printf("[DEBUG] Correct count: %d/%d (required: %d)", correctCount, len(answers), b.cfg.VerifyRequiredCorrect)

	passed := correctCount >= b.cfg.VerifyRequiredCorrect

	// Atomic claim: if the expired sweeper already deleted this row, abort here so
	// we don't double-record (the sweeper will record the 'expired' event itself).
	claimed, err := b.db.ClaimPendingVerification(chatID, userID, pv.ExpiresAt)
	if err != nil {
		log.Printf("[ERROR] Failed to claim pending verification for user %d: %v", userID, err)
		return
	}
	if !claimed {
		log.Printf("[INFO] Pending verification for user %d already claimed (likely by expiry sweeper); skipping evaluation", userID)
		return
	}

	event := database.VerificationEvent{
		ChatID:      chatID,
		UserID:      userID,
		IsTest:      pv.IsTest,
		UserLang:    pv.UserLang,
		Regenerated: pv.Regenerated,
		OccurredAt:  time.Now().Unix(),
	}
	if passed {
		event.Outcome = "passed"
		event.CompletionTimeMs = sql.NullInt64{Int64: time.Since(pv.CreatedAt).Milliseconds(), Valid: true}
	} else {
		event.Outcome = "failed"
		if firstWrongStep > 0 {
			event.FailedAtStep = sql.NullInt64{Int64: int64(firstWrongStep), Valid: true}
		}
	}
	if err := b.db.RecordVerificationEvent(event); err != nil {
		log.Printf("[ERROR] Failed to record verification event: %v", err)
	}

	if passed {
		log.Printf("[INFO] User %d passed verification (%d/%d correct)", userID, correctCount, len(answers))
		b.handleVerificationSuccess(query, pv)
	} else {
		log.Printf("[INFO] User %d failed verification (%d/%d correct)", userID, correctCount, len(answers))
		b.handleVerificationFailure(query, pv)
	}
}

// applyVerificationSuccess performs the machine-side success path: clears the
// stored failure record, unrestricts the target user, and deletes the
// verification message. It does no user-facing messaging — that is the
// caller's responsibility — so this is safe to call from contexts where the
// initiator is an admin rather than the target user.
func (b *Bot) applyVerificationSuccess(chatID, targetUserID int64, verificationMessageID int) {
	if err := b.db.ClearVerificationFailure(chatID, targetUserID); err != nil {
		log.Printf("[ERROR] Failed to clear failure record: %v", err)
	}

	if err := b.unrestrictUser(chatID, targetUserID); err != nil {
		log.Printf("[WARN] Failed to unrestrict user (may already be admin): %v", err)
	}

	if verificationMessageID > 0 {
		deleteMsg := tgbotapi.NewDeleteMessage(chatID, verificationMessageID)
		if _, err := b.api.Request(deleteMsg); err != nil {
			log.Printf("[DEBUG] Failed to delete verification message %d (non-fatal): %v", verificationMessageID, err)
		}
	}
}

func (b *Bot) handleVerificationSuccess(query *tgbotapi.CallbackQuery, pv *database.PendingVerification) {
	chatID := query.Message.Chat.ID
	userID := pv.UserID

	log.Printf("[DEBUG] Handling verification success for user %d in chat %d", userID, chatID)

	// Calculate elapsed time
	elapsedTime := time.Since(pv.CreatedAt)
	log.Printf("[INFO] User %d completed verification in %.2f seconds", userID, elapsedTime.Seconds())

	// Record to leaderboard if in private chat (test mode)
	isPrivateChat := query.Message.Chat.Type == "private"
	if isPrivateChat {
		username := query.From.UserName
		firstName := query.From.FirstName
		lastName := query.From.LastName

		if err := b.db.RecordLeaderboardEntry(userID, username, firstName, lastName, elapsedTime); err != nil {
			log.Printf("[ERROR] Failed to record leaderboard entry: %v", err)
		} else {
			log.Printf("[INFO] Recorded leaderboard entry for user %d with time %.2f seconds", userID, elapsedTime.Seconds())

			// Send leaderboard update to channel
			b.sendLeaderboardUpdate()
		}
	}

	b.applyVerificationSuccess(chatID, userID, query.Message.MessageID)

	// Send welcome message with elapsed time and links
	log.Printf("[DEBUG] Sending welcome message to chat %d", chatID)
	userName := query.From.FirstName
	if query.From.LastName != "" {
		userName += " " + query.From.LastName
	}
	welcomeText := b.t(query.From, "verify_passed", escapeHTML(userName))
	
	// Add timing information and links for test mode (private chat)
	if isPrivateChat {
		welcomeText += "\n\n" + b.t(query.From, "test_completion_time", fmt.Sprintf("%.2f", elapsedTime.Seconds()))
		
		// Add percentile information based on this completion time
		percentile, err := b.db.GetTimePercentile(elapsedTime)
		if err != nil {
			log.Printf("[ERROR] Failed to get time percentile: %v", err)
		} else if percentile >= 0 {
			welcomeText += "\n" + b.t(query.From, "test_percentile", fmt.Sprintf("%.0f", percentile))
		}
		
		// Add retry and leaderboard links
		retryLink := fmt.Sprintf("t.me/%s?start=retry_test", b.self.UserName)
		leaderboardLink := "https://t.me/ING_reCAPTCHA_leaderboard/13"
		welcomeText += "\n" + b.t(query.From, "test_retry_link", retryLink)
		welcomeText += " | " + b.t(query.From, "test_leaderboard_link", leaderboardLink)
	}
	
	welcomeMsg := tgbotapi.NewMessage(chatID, welcomeText)
	welcomeMsg.ParseMode = "HTML"
	if _, err := b.api.Send(welcomeMsg); err != nil {
		log.Printf("[ERROR] Failed to send welcome message: %v", err)
	}

	callback := tgbotapi.NewCallback(query.ID, b.t(query.From, "verify_passed_callback"))
	if _, err := b.api.Request(callback); err != nil {
		log.Printf("[ERROR] Failed to send callback response: %v", err)
	}

	log.Printf("[INFO] User %d passed verification in chat %d", userID, chatID)
}

func (b *Bot) handleVerificationRetry(query *tgbotapi.CallbackQuery, pv *database.PendingVerification) {
	chatID := query.Message.Chat.ID
	userID := pv.UserID

	log.Printf("[DEBUG] Handling verification retry for user %d in chat %d (current retry count: %d)", userID, chatID, pv.RetryCount)

	// Increment retry count
	retryCount, err := b.db.IncrementRetryCount(chatID, userID)
	if err != nil {
		log.Printf("[ERROR] Failed to increment retry count: %v", err)
	} else {
		log.Printf("[DEBUG] Incremented retry count to %d", retryCount)
	}

	// Delete old message
	log.Printf("[DEBUG] Deleting old verification message %d", query.Message.MessageID)
	deleteMsg := tgbotapi.NewDeleteMessage(chatID, query.Message.MessageID)
	resp, err := b.api.Request(deleteMsg)
	if err != nil {
		log.Printf("[ERROR] Failed to delete old verification message: %v", err)
	} else {
		log.Printf("[DEBUG] Successfully deleted old message, response: %+v", resp)
	}

	callback := tgbotapi.NewCallback(query.ID, b.t(query.From, "verify_failed_retry", b.cfg.VerifyMaxRetry-retryCount+1))
	if _, err := b.api.Request(callback); err != nil {
		log.Printf("[ERROR] Failed to send callback response: %v", err)
	}

	// Start new verification (use test mode if user is admin)
	user := query.From
	if b.isGroupAdmin(chatID, userID) {
		log.Printf("[DEBUG] User %d is admin, starting test verification", userID)
		if err := b.startTestVerification(chatID, userID, user); err != nil {
			log.Printf("[ERROR] Failed to restart test verification: %v", err)
		}
	} else {
		log.Printf("[DEBUG] Starting new verification for user %d", userID)
		if err := b.startVerification(chatID, userID, user); err != nil {
			log.Printf("[ERROR] Failed to restart verification: %v", err)
			b.unrestrictUser(chatID, userID)
		}
	}
}

func (b *Bot) handleVerificationFailure(query *tgbotapi.CallbackQuery, pv *database.PendingVerification) {
	chatID := query.Message.Chat.ID
	userID := pv.UserID

	log.Printf("[DEBUG] Handling verification failure for user %d in chat %d", userID, chatID)

	// Delete verification message
	log.Printf("[DEBUG] Deleting verification message %d", query.Message.MessageID)
	deleteMsg := tgbotapi.NewDeleteMessage(chatID, query.Message.MessageID)
	resp, err := b.api.Request(deleteMsg)
	if err != nil {
		log.Printf("[ERROR] Failed to delete verification message: %v", err)
	} else {
		log.Printf("[DEBUG] Successfully deleted message, response: %+v", resp)
	}

	// Check if user is admin (can't kick admins)
	if b.isGroupAdmin(chatID, userID) {
		log.Printf("[DEBUG] User %d is admin, skipping kick (test mode)", userID)
		callback := tgbotapi.NewCallback(query.ID, b.t(query.From, "verify_failed_test"))
		b.api.Request(callback)
		log.Printf("[INFO] Admin %d failed test verification in chat %d", userID, chatID)
		b.deleteJoinMessage(chatID, pv)
		return
	}

	// Check if user has previous failure
	hasPreviousFailure, err := b.db.HasPreviousFailure(chatID, userID)
	if err != nil {
		log.Printf("[ERROR] Failed to check previous failure: %v", err)
		hasPreviousFailure = false
	}

	if hasPreviousFailure {
		// Second failure - kick and ban permanently
		log.Printf("[INFO] User %d has previous failure, kicking with permanent ban", userID)
		kickConfig := tgbotapi.KickChatMemberConfig{
			ChatMemberConfig: tgbotapi.ChatMemberConfig{
				ChatID: chatID,
				UserID: userID,
			},
			// UntilDate = 0 means permanent ban
		}
		kickResp, err := b.api.Request(kickConfig)
		if err != nil {
			log.Printf("[ERROR] Failed to kick and ban user %d: %v", userID, err)
		} else {
			log.Printf("[DEBUG] Successfully kicked and banned user %d, response: %+v", userID, kickResp)

			// Only delete join history if kick succeeded
			if err := b.db.DeleteUserJoinHistory(chatID, userID); err != nil {
				log.Printf("[ERROR] Failed to delete join history: %v", err)
			}
		}

		// Clear failure record after permanent ban
		if err := b.db.ClearVerificationFailure(chatID, userID); err != nil {
			log.Printf("[ERROR] Failed to clear failure record: %v", err)
		}

		callback := tgbotapi.NewCallback(query.ID, b.t(query.From, "verify_failed_banned"))
		if _, err := b.api.Request(callback); err != nil {
			log.Printf("[ERROR] Failed to send callback response: %v", err)
		}

		log.Printf("[INFO] User %d failed verification (2nd time) in chat %d and was permanently banned", userID, chatID)
		b.deleteJoinMessage(chatID, pv)
	} else {
		// First failure - tempban (kick with UntilDate). See bot.go for the
		// detailed rationale; the short version: kick+unban leaves the user's
		// pre-scheduled messages queued, tempban clears them and lets Telegram
		// auto-expire the ban so legit users can still rejoin.
		tempbanUntil := time.Now().Add(time.Duration(b.cfg.VerifyTempbanSeconds) * time.Second).Unix()
		log.Printf("[INFO] User %d first failure, tempbanning until %d (%ds)", userID, tempbanUntil, b.cfg.VerifyTempbanSeconds)

		kickConfig := tgbotapi.KickChatMemberConfig{
			ChatMemberConfig: tgbotapi.ChatMemberConfig{
				ChatID: chatID,
				UserID: userID,
			},
			UntilDate: tempbanUntil,
		}
		kickResp, err := b.api.Request(kickConfig)
		if err != nil {
			log.Printf("[ERROR] Failed to tempban user %d: %v", userID, err)
		} else {
			log.Printf("[DEBUG] Successfully tempbanned user %d, response: %+v", userID, kickResp)

			if err := b.db.RecordVerificationFailure(chatID, userID); err != nil {
				log.Printf("[ERROR] Failed to record verification failure: %v", err)
			}

			if err := b.db.DeleteUserJoinHistory(chatID, userID); err != nil {
				log.Printf("[ERROR] Failed to delete join history: %v", err)
			}
		}

		callback := tgbotapi.NewCallback(query.ID, b.t(query.From, "verify_failed_kicked"))
		if _, err := b.api.Request(callback); err != nil {
			log.Printf("[ERROR] Failed to send callback response: %v", err)
		}

		log.Printf("[INFO] User %d failed verification (1st time) in chat %d and was tempbanned (%ds)", userID, chatID, b.cfg.VerifyTempbanSeconds)
		b.deleteJoinMessage(chatID, pv)
	}
}

func (b *Bot) downloadAndCacheImage(fileID string, imageID int64) (string, error) {
	file, err := b.api.GetFile(tgbotapi.FileConfig{FileID: fileID})
	if err != nil {
		return "", fmt.Errorf("failed to get file: %w", err)
	}

	fileURL := file.Link(b.token)

	resp, err := httpClient.Get(fileURL)
	if err != nil {
		return "", fmt.Errorf("failed to download file: %w", err)
	}
	defer resp.Body.Close()

	ext := filepath.Ext(file.FilePath)
	if ext == "" {
		ext = ".jpg"
	}

	localPath := filepath.Join(b.cfg.ImageCachePath, fmt.Sprintf("%d%s", imageID, ext))

	out, err := os.Create(localPath)
	if err != nil {
		return "", fmt.Errorf("failed to create file: %w", err)
	}
	defer out.Close()

	if _, err := io.Copy(out, resp.Body); err != nil {
		return "", fmt.Errorf("failed to write file: %w", err)
	}

	// Update database with local path
	if err := b.db.UpdateImageFilePath(imageID, localPath); err != nil {
		log.Printf("Failed to update image file path: %v", err)
	}

	return localPath, nil
}

// OptionsData stores the shuffled options for a verification session
type OptionsData struct {
	Options []string `json:"options"`
}

// deleteJoinMessage removes the "X joined the group" service message stored in pv.
// Called only on verification failure or timeout; on success the message is kept.
func (b *Bot) deleteJoinMessage(chatID int64, pv *database.PendingVerification) {
	log.Printf("[DEBUG] deleteJoinMessage called for user %d, JoinMessageID.Valid=%v", pv.UserID, pv.JoinMessageID.Valid)
	if !pv.JoinMessageID.Valid {
		log.Printf("[DEBUG] No join message ID to delete for user %d", pv.UserID)
		return
	}
	log.Printf("[DEBUG] Attempting to delete join message %d for user %d", pv.JoinMessageID.Int64, pv.UserID)
	deleteMsg := tgbotapi.NewDeleteMessage(chatID, int(pv.JoinMessageID.Int64))
	if _, err := b.api.Request(deleteMsg); err != nil {
		log.Printf("[WARN] Failed to delete join message %d: %v", pv.JoinMessageID.Int64, err)
	} else {
		log.Printf("[DEBUG] Successfully deleted join message %d for user %d", pv.JoinMessageID.Int64, pv.UserID)
	}
}

// sendLeaderboardUpdate updates the pinned leaderboard message in the configured channel
func (b *Bot) sendLeaderboardUpdate() {
	if b.cfg.LeaderboardChannelID == 0 {
		log.Printf("[DEBUG] Leaderboard channel ID not configured, skipping update")
		return
	}

	if b.cfg.LeaderboardMessageID == 0 {
		log.Printf("[DEBUG] Leaderboard message ID not configured, skipping update")
		return
	}

	entries, err := b.db.GetTopLeaderboard(20)
	if err != nil {
		log.Printf("[ERROR] Failed to get leaderboard: %v", err)
		return
	}

	if len(entries) == 0 {
		log.Printf("[DEBUG] No leaderboard entries to update")
		return
	}

	// Build leaderboard message
	var sb strings.Builder
	sb.WriteString("🏆 <b>Top 20 Leaderboard</b>\n\n")
	
	for _, entry := range entries {
		medal := ""
		switch entry.Rank {
		case 1:
			medal = "🥇"
		case 2:
			medal = "🥈"
		case 3:
			medal = "🥉"
		default:
			medal = fmt.Sprintf("%d.", entry.Rank)
		}
		
		name := escapeHTML(entry.FirstName)
		if entry.LastName != "" {
			name += " " + escapeHTML(entry.LastName)
		}
		
		timeSeconds := float64(entry.CompletionTimeMs) / 1000.0
		sb.WriteString(fmt.Sprintf("%s %s - %.2fs\n", medal, name, timeSeconds))
	}
	
	sb.WriteString(fmt.Sprintf("\n<i>Updated: %s</i>", time.Now().Format("2006-01-02 15:04:05")))

	// Create inline keyboard with "Start Test" button
	startTestButton := tgbotapi.NewInlineKeyboardButtonURL("🎯 开始测试", fmt.Sprintf("t.me/%s?start=test", b.self.UserName))
	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(startTestButton),
	)

	// Edit the existing message instead of sending a new one
	editMsg := tgbotapi.NewEditMessageText(b.cfg.LeaderboardChannelID, b.cfg.LeaderboardMessageID, sb.String())
	editMsg.ParseMode = "HTML"
	editMsg.ReplyMarkup = &keyboard
	
	if _, err := b.api.Send(editMsg); err != nil {
		log.Printf("[ERROR] Failed to update leaderboard message: %v", err)
	} else {
		log.Printf("[INFO] Successfully updated leaderboard message %d in channel %d", b.cfg.LeaderboardMessageID, b.cfg.LeaderboardChannelID)
	}
}
