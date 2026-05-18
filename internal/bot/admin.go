package bot

import (
	"strconv"
	"strings"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

func (b *Bot) cmdStart(msg *tgbotapi.Message, args string) {
	// Handle deep links like /start test or /start retry_test
	args = strings.TrimSpace(args)
	
	if args == "test" || args == "retry_test" {
		// Only allow in private chat
		if msg.Chat.Type == "private" {
			if err := b.startTestVerification(msg.Chat.ID, msg.From.ID, msg.From); err != nil {
				b.sendMessage(msg.Chat.ID, b.t(msg.From, "err_test_start_failed", err))
			}
			return
		}
	}
	
	// Default welcome message
	reply := tgbotapi.NewMessage(msg.Chat.ID, b.t(msg.From, "cmd_start_welcome"))
	reply.ParseMode = "HTML"
	b.api.Send(reply)
}

func (b *Bot) cmdHelp(msg *tgbotapi.Message) {
	var sb strings.Builder
	sb.WriteString(b.t(msg.From, "cmd_help_title"))
	sb.WriteString(b.t(msg.From, "cmd_help_general"))
	sb.WriteString(b.t(msg.From, "cmd_help_image_sets"))
	sb.WriteString(b.t(msg.From, "cmd_help_admin_mgmt"))
	sb.WriteString(b.t(msg.From, "cmd_help_verify_params"))
	sb.WriteString(b.t(msg.From, "cmd_help_image_count", b.cfg.VerifyImageCount))
	sb.WriteString(b.t(msg.From, "cmd_help_required_correct", b.cfg.VerifyRequiredCorrect))
	sb.WriteString(b.t(msg.From, "cmd_help_timeout", b.cfg.VerifyTimeoutSeconds))
	sb.WriteString(b.t(msg.From, "cmd_help_max_retry", b.cfg.VerifyMaxRetry))
	sb.WriteString(b.t(msg.From, "cmd_help_permissions"))
	sb.WriteString("\n\n")
	sb.WriteString(b.t(msg.From, "cmd_help_i18n"))

	reply := tgbotapi.NewMessage(msg.Chat.ID, sb.String())
	reply.ParseMode = "HTML"
	b.api.Send(reply)
}

func (b *Bot) cmdAddSet(msg *tgbotapi.Message, args string) {
	if !b.canManageBot(msg) {
		b.sendMessage(msg.Chat.ID, b.t(msg.From, "err_permission_denied"))
		return
	}

	label := strings.TrimSpace(args)
	if label == "" {
		b.sendMessage(msg.Chat.ID, b.t(msg.From, "err_usage_addset"))
		return
	}

	// Check if set already exists
	existing, err := b.db.GetImageSetByLabel(label)
	if err != nil {
		b.sendMessage(msg.Chat.ID, b.t(msg.From, "err_db_error"))
		return
	}
	if existing != nil {
		b.sendMessage(msg.Chat.ID, b.t(msg.From, "err_set_already_exists", escapeHTML(label)))
		return
	}

	_, err = b.db.CreateImageSet(label)
	if err != nil {
		b.sendMessage(msg.Chat.ID, b.t(msg.From, "err_create_set_failed"))
		return
	}

	b.sendMessage(msg.Chat.ID, b.t(msg.From, "success_set_created", escapeHTML(label)))
}

func (b *Bot) cmdAddImage(msg *tgbotapi.Message, args string) {
	if !b.canManageBot(msg) {
		b.sendMessage(msg.Chat.ID, b.t(msg.From, "err_permission_denied"))
		return
	}

	label := strings.TrimSpace(args)
	if label == "" {
		b.sendMessage(msg.Chat.ID, b.t(msg.From, "err_usage_addimage"))
		return
	}

	// Check if message is a reply to a photo
	if msg.ReplyToMessage == nil || len(msg.ReplyToMessage.Photo) == 0 {
		b.sendMessage(msg.Chat.ID, b.t(msg.From, "err_reply_photo_required"))
		return
	}

	// Get the image set
	set, err := b.db.GetImageSetByLabel(label)
	if err != nil {
		b.sendMessage(msg.Chat.ID, b.t(msg.From, "err_db_error"))
		return
	}
	if set == nil {
		b.sendMessage(msg.Chat.ID, b.t(msg.From, "err_set_not_found", escapeHTML(label)))
		return
	}

	// Get the largest photo
	photos := msg.ReplyToMessage.Photo
	largestPhoto := photos[len(photos)-1]

	// Add image to database first, then eagerly cache it so "added" means both DB and local file are ready.
	img, err := b.db.AddImage(set.ID, largestPhoto.FileID, "")
	if err != nil {
		b.sendMessage(msg.Chat.ID, b.t(msg.From, "err_add_image_failed"))
		return
	}

	if _, err := b.downloadAndCacheImage(img.FileID, img.ID); err != nil {
		if rollbackErr := b.db.DeleteImageByID(img.ID); rollbackErr != nil {
			b.sendMessage(msg.Chat.ID, b.t(msg.From, "err_add_image_cache_failed", err))
			return
		}
		b.sendMessage(msg.Chat.ID, b.t(msg.From, "err_add_image_cache_failed", err))
		return
	}

	count, _ := b.db.GetImageCountBySet(set.ID)
	b.sendMessage(msg.Chat.ID, b.t(msg.From, "success_image_added", escapeHTML(label), count))
}

func (b *Bot) cmdListSets(msg *tgbotapi.Message) {
	if !b.canManageBot(msg) {
		b.sendMessage(msg.Chat.ID, b.t(msg.From, "err_permission_denied"))
		return
	}

	sets, err := b.db.GetAllImageSets()
	if err != nil {
		b.sendMessage(msg.Chat.ID, b.t(msg.From, "err_get_sets_failed"))
		return
	}

	if len(sets) == 0 {
		b.sendMessage(msg.Chat.ID, b.t(msg.From, "set_list_empty"))
		return
	}

	var sb strings.Builder
	sb.WriteString(b.t(msg.From, "set_list_title"))
	for _, set := range sets {
		sb.WriteString(b.t(msg.From, "set_list_item", escapeHTML(set.Label)))
	}
	sb.WriteString(b.t(msg.From, "set_list_total", len(sets)))

	b.sendMessage(msg.Chat.ID, sb.String())
}

func (b *Bot) cmdDelSet(msg *tgbotapi.Message, args string) {
	if !b.canManageBot(msg) {
		b.sendMessage(msg.Chat.ID, b.t(msg.From, "err_permission_denied"))
		return
	}

	label := strings.TrimSpace(args)
	if label == "" {
		b.sendMessage(msg.Chat.ID, b.t(msg.From, "err_usage_delset"))
		return
	}

	// Check if set exists
	set, err := b.db.GetImageSetByLabel(label)
	if err != nil {
		b.sendMessage(msg.Chat.ID, b.t(msg.From, "err_db_error"))
		return
	}
	if set == nil {
		b.sendMessage(msg.Chat.ID, b.t(msg.From, "err_set_not_found", escapeHTML(label)))
		return
	}

	if err := b.db.DeleteImageSet(label); err != nil {
		b.sendMessage(msg.Chat.ID, b.t(msg.From, "err_delete_set_failed"))
		return
	}

	b.sendMessage(msg.Chat.ID, b.t(msg.From, "success_set_deleted", escapeHTML(label)))
}

func (b *Bot) cmdSetStats(msg *tgbotapi.Message) {
	if !b.canManageBot(msg) {
		b.sendMessage(msg.Chat.ID, b.t(msg.From, "err_permission_denied"))
		return
	}

	stats, err := b.db.GetAllImageStats()
	if err != nil {
		b.sendMessage(msg.Chat.ID, b.t(msg.From, "err_get_stats_failed"))
		return
	}

	if len(stats) == 0 {
		b.sendMessage(msg.Chat.ID, b.t(msg.From, "set_stats_empty"))
		return
	}

	var sb strings.Builder
	sb.WriteString(b.t(msg.From, "set_stats_title"))

	totalImages := 0
	for label, count := range stats {
		sb.WriteString(b.t(msg.From, "set_stats_item", escapeHTML(label), count))
		totalImages += count
	}

	sb.WriteString(b.t(msg.From, "set_stats_total", len(stats), totalImages))

	minRequired := b.cfg.VerifyImageCount + b.cfg.VerifyDistractorCount
	if len(stats) < minRequired {
		sb.WriteString(b.t(msg.From, "set_stats_warning", minRequired))
	}

	b.sendMessage(msg.Chat.ID, sb.String())
}

func (b *Bot) cmdTest(msg *tgbotapi.Message) {
	// Allow all users to test in private chat
	// Only require admin permission in groups
	if msg.Chat.Type == "group" || msg.Chat.Type == "supergroup" {
		if !b.canManageBot(msg) {
			b.sendMessage(msg.Chat.ID, b.t(msg.From, "err_permission_denied"))
			return
		}
	}

	// Start test verification (works in both groups and private chat)
	if err := b.startTestVerification(msg.Chat.ID, msg.From.ID, msg.From); err != nil {
		b.sendMessage(msg.Chat.ID, b.t(msg.From, "err_test_start_failed", err))
		return
	}
}

func (b *Bot) cmdMyID(msg *tgbotapi.Message) {
	b.sendMessage(msg.Chat.ID, b.t(msg.From, "cmd_myid_response", msg.From.ID))
}

func (b *Bot) cmdAddAdmin(msg *tgbotapi.Message, args string) {
	// Only existing bot admins can add new admins
	if !b.isBotAdmin(msg.From.ID) {
		b.sendMessage(msg.Chat.ID, b.t(msg.From, "admin_only_add"))
		return
	}

	args = strings.TrimSpace(args)
	if args == "" {
		b.sendMessage(msg.Chat.ID, b.t(msg.From, "err_usage_addadmin"))
		return
	}

	userID, err := strconv.ParseInt(args, 10, 64)
	if err != nil {
		b.sendMessage(msg.Chat.ID, b.t(msg.From, "err_invalid_user_id"))
		return
	}

	if err := b.db.AddAdmin(userID); err != nil {
		b.sendMessage(msg.Chat.ID, b.t(msg.From, "err_add_admin_failed"))
		return
	}

	b.sendMessage(msg.Chat.ID, b.t(msg.From, "success_admin_added", userID))
}

func (b *Bot) cmdRemoveAdmin(msg *tgbotapi.Message, args string) {
	// Only existing bot admins can remove admins
	if !b.isBotAdmin(msg.From.ID) {
		b.sendMessage(msg.Chat.ID, b.t(msg.From, "admin_only_remove"))
		return
	}

	args = strings.TrimSpace(args)
	if args == "" {
		b.sendMessage(msg.Chat.ID, b.t(msg.From, "err_usage_removeadmin"))
		return
	}

	userID, err := strconv.ParseInt(args, 10, 64)
	if err != nil {
		b.sendMessage(msg.Chat.ID, b.t(msg.From, "err_invalid_user_id"))
		return
	}

	// Prevent removing self
	if userID == msg.From.ID {
		b.sendMessage(msg.Chat.ID, b.t(msg.From, "err_cannot_remove_self"))
		return
	}

	if err := b.db.RemoveAdmin(userID); err != nil {
		b.sendMessage(msg.Chat.ID, b.t(msg.From, "err_remove_admin_failed"))
		return
	}

	b.sendMessage(msg.Chat.ID, b.t(msg.From, "success_admin_removed", userID))
}

func (b *Bot) cmdListAdmins(msg *tgbotapi.Message) {
	// Only bot admins can list admins
	if !b.isBotAdmin(msg.From.ID) {
		b.sendMessage(msg.Chat.ID, b.t(msg.From, "admin_only_list"))
		return
	}

	admins, err := b.db.GetAllAdmins()
	if err != nil {
		b.sendMessage(msg.Chat.ID, b.t(msg.From, "err_get_admins_failed"))
		return
	}

	if len(admins) == 0 {
		b.sendMessage(msg.Chat.ID, b.t(msg.From, "admin_list_empty"))
		return
	}

	var sb strings.Builder
	sb.WriteString(b.t(msg.From, "admin_list_title"))
	for i, adminID := range admins {
		sb.WriteString(b.t(msg.From, "admin_list_item", i+1, adminID))
	}
	sb.WriteString(b.t(msg.From, "admin_list_total", len(admins)))

	b.sendMessage(msg.Chat.ID, sb.String())
}

func (b *Bot) cmdCacheMissing(msg *tgbotapi.Message, args string) {
	if !b.canManageBot(msg) {
		b.sendMessage(msg.Chat.ID, b.t(msg.From, "err_permission_denied"))
		return
	}

	limit := 50
	trimmed := strings.TrimSpace(args)
	if trimmed != "" {
		n, err := strconv.Atoi(trimmed)
		if err != nil || n <= 0 {
			b.sendMessage(msg.Chat.ID, b.t(msg.From, "err_usage_cachemissing"))
			return
		}
		limit = n
	}

	images, err := b.db.ListImagesWithoutCache(limit)
	if err != nil {
		b.sendMessage(msg.Chat.ID, b.t(msg.From, "err_db_error"))
		return
	}
	if len(images) == 0 {
		b.sendMessage(msg.Chat.ID, b.t(msg.From, "cache_missing_empty"))
		return
	}

	okCount := 0
	failCount := 0
	for _, img := range images {
		if _, err := b.downloadAndCacheImage(img.FileID, img.ID); err != nil {
			failCount++
			continue
		}
		okCount++
	}

	b.sendMessage(msg.Chat.ID, b.t(msg.From, "cache_missing_done", len(images), okCount, failCount))
}
