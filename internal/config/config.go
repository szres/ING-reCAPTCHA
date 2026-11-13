package config

import (
	"log"
	"os"
	"strconv"
	"strings"
)

type Config struct {
	TelegramBotToken      string
	DatabasePath          string
	ImageCachePath        string
	VerifyImageCount      int
	VerifyRequiredCorrect int
	VerifyTimeoutSeconds  int
	VerifyMaxRetry        int
	VerifyDistractorCount int
	RejoinCooldownSeconds int
	// VerifyTempbanSeconds is how long a user is tempbanned after a first
	// verification failure. Using a tempban (KickChatMember with UntilDate)
	// instead of kick+unban clears any scheduled messages they may have
	// queued before/during the verification window. Telegram auto-lifts the
	// ban after this duration, so legit users can rejoin organically.
	VerifyTempbanSeconds int
	DefaultLanguage      string
	InitialAdminIDs       []int64
	LeaderboardChannelID  int64
	LeaderboardMessageID  int
	DailyStatsTimezone    string
	// AntispamBanBot is the username (no @) of an in-group anti-spam bot we
	// delegate to when a tombstone fires. If non-empty, on tombstone hits we
	// reply to the offending message with "/ban@<AntispamBanBot>" so that bot
	// can act first; after a 2-second window we clean up our own command and
	// fall back to our own delete+tempban. Set to "" to disable.
	AntispamBanBot string
	// AntispamTombstoneEnabled turns the scheduled-message tombstone filter
	// on/off globally. When false, handleMessage skips the tombstone check
	// entirely — no ban, no delete, no delegation. Intended for groups that
	// run in Join Request mode where the pre-join Mini App flow makes the
	// tombstone unnecessary (users can't post scheduled messages before
	// they're admitted, and the DB's verification_failures rows are only
	// populated by the legacy in-group flow so the tombstone would only
	// misfire on already-admitted admins with stale rows). Defaults to
	// true to preserve legacy-flow behaviour.
	AntispamTombstoneEnabled bool
	// VerifySilentNotifications mutes the in-group verification messages
	// (challenge photo, regenerated photo, welcome on pass, admin moderation
	// follow-ups). They still arrive in chat, just without sound/vibration.
	VerifySilentNotifications bool
	// EphemeralVerifyEnabled sends the in-group verification challenge as
	// a Bot API 10.2 Ephemeral Message — visible only to the joining user
	// (and the bot). Eliminates group spam and shoulder-surfing of the
	// captcha. Only applies to the legacy in-group chat_member flow;
	// the Mini App / chat_join_request flow already runs pre-join and
	// doesn't need this. Requires the bot to be on Bot API 10.2+ (July
	// 2026). Defaults to false while ephemeral rollout stabilises.
	EphemeralVerifyEnabled bool
	// EphemeralFallbackSeconds is how long an ephemeral challenge may sit
	// without a single tap before it is re-sent as a regular group message.
	// Telegram does not guarantee delivery of ephemeral messages, least of
	// all to a user who is offline when they are admitted, and an undelivered
	// challenge would otherwise run out as a timeout and a ban. 0 disables
	// the fallback. Set via EPHEMERAL_FALLBACK_SECONDS; values at or above
	// VERIFY_TIMEOUT_SECONDS never trigger.
	EphemeralFallbackSeconds int

	// Mini App verification (chat_join_request flow).
	//
	// MiniAppEnabled turns the entire pre-join Mini App flow on/off. When
	// false the bot ignores chat_join_request updates and the HTTP server
	// is not started. The existing in-group chat_member flow keeps working
	// unchanged either way.
	MiniAppEnabled bool
	// MiniAppListenAddr is the TCP address the internal HTTP server binds
	// to (e.g. ":8080"). Cloudflare Tunnel terminates TLS at the edge and
	// forwards plain HTTP to this address; we never expose it directly.
	MiniAppListenAddr string
	// MiniAppPublicURL is the externally-reachable HTTPS origin the Mini
	// App is served from, with no trailing slash (e.g.
	// "https://verify.szres.org"). Sent to Telegram as the web_app_url
	// argument to sendChatJoinRequestWebApp.
	MiniAppPublicURL string
	// MiniAppSessionTTLSeconds is the lifetime of a Mini App verification
	// session. Codex's review flagged the original 90s as too tight for a
	// real user on a slow mobile network; default 300s (5 minutes), matching
	// Telegram's documented chat_join_request window.
	MiniAppSessionTTLSeconds int
	// WelcomeMessageInGroup controls whether we send a (silent) welcome
	// message in the group after a Mini App pass + chat_join_request
	// approval. The welcome message is the only in-group artifact of the
	// Mini App flow; everything else happens silently before the user
	// even joins.
	WelcomeMessageInGroup bool

	// FailNotifyChatIDs restricts the failure-notification hook to a
	// specific set of group chats — DMs are only sent for verification
	// failures / timeouts that occurred in one of these chats. Empty
	// disables the feature. Set via FAIL_NOTIFY_CHAT_IDS (comma-separated
	// list of chat_ids, negative for supergroups).
	FailNotifyChatIDs []int64
	// FailNotifyUserIDs receives the DM when a failure/timeout fires in
	// a chat listed in FailNotifyChatIDs. Set via FAIL_NOTIFY_USER_IDS
	// (comma-separated user_ids). The DM includes the applicant's
	// username and user_id so an admin can eyeball spam patterns.
	FailNotifyUserIDs []int64
}

func Load() *Config {
	return &Config{
		TelegramBotToken:      getEnv("TELEGRAM_BOT_TOKEN", ""),
		DatabasePath:          getEnv("DATABASE_PATH", "./data/bot.db"),
		ImageCachePath:        getEnv("IMAGE_CACHE_PATH", "./images"),
		VerifyImageCount:      getEnvInt("VERIFY_IMAGE_COUNT", 3),
		VerifyRequiredCorrect: getEnvInt("VERIFY_REQUIRED_CORRECT", 2),
		VerifyTimeoutSeconds:  getEnvInt("VERIFY_TIMEOUT_SECONDS", 120),
		VerifyMaxRetry:        getEnvInt("VERIFY_MAX_RETRY", 1),
		VerifyDistractorCount: getEnvInt("VERIFY_DISTRACTOR_COUNT", 3),
		RejoinCooldownSeconds: getEnvInt("REJOIN_COOLDOWN_SECONDS", 300),
		VerifyTempbanSeconds:  getEnvInt("VERIFY_TEMPBAN_SECONDS", 300),
		DefaultLanguage:       getEnv("DEFAULT_LANGUAGE", "en"),
		InitialAdminIDs:       getEnvInt64List("BOT_ADMIN_IDS"),
		LeaderboardChannelID:  getEnvInt64("LEADERBOARD_CHANNEL_ID", 0),
		LeaderboardMessageID:  getEnvInt("LEADERBOARD_MESSAGE_ID", 0),
		DailyStatsTimezone:    getEnv("DAILY_STATS_TIMEZONE", "Asia/Shanghai"),
		AntispamBanBot:            getEnv("ANTISPAM_BAN_BOT", "BadBustBot"),
		AntispamTombstoneEnabled:  getEnvBool("ANTISPAM_TOMBSTONE_ENABLED", true),
		VerifySilentNotifications: getEnvBool("VERIFY_SILENT_NOTIFICATIONS", true),
		EphemeralVerifyEnabled:    getEnvBool("EPHEMERAL_VERIFY_ENABLED", false),
		EphemeralFallbackSeconds:  getEnvInt("EPHEMERAL_FALLBACK_SECONDS", 30),

		MiniAppEnabled:           getEnvBool("MINIAPP_ENABLED", false),
		MiniAppListenAddr:        getEnv("MINIAPP_LISTEN_ADDR", ":8080"),
		MiniAppPublicURL:         strings.TrimRight(getEnv("MINIAPP_PUBLIC_URL", ""), "/"),
		MiniAppSessionTTLSeconds: getEnvInt("MINIAPP_SESSION_TTL_SECONDS", 300),
		WelcomeMessageInGroup:    getEnvBool("WELCOME_MESSAGE_IN_GROUP", true),

		FailNotifyChatIDs: getEnvInt64List("FAIL_NOTIFY_CHAT_IDS"),
		FailNotifyUserIDs: getEnvInt64List("FAIL_NOTIFY_USER_IDS"),
	}
}

func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

func getEnvInt(key string, defaultValue int) int {
	if value := os.Getenv(key); value != "" {
		if intVal, err := strconv.Atoi(value); err == nil {
			return intVal
		}
		log.Printf("Warning: invalid integer value for %s: %s, using default %d", key, value, defaultValue)
	}
	return defaultValue
}

func getEnvInt64List(key string) []int64 {
	value := os.Getenv(key)
	if value == "" {
		return nil
	}

	parts := strings.Split(value, ",")
	var result []int64
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if id, err := strconv.ParseInt(p, 10, 64); err == nil {
			result = append(result, id)
		} else {
			log.Printf("Warning: invalid admin ID in %s: %s", key, p)
		}
	}
	return result
}

func getEnvInt64(key string, defaultValue int64) int64 {
	if value := os.Getenv(key); value != "" {
		if intVal, err := strconv.ParseInt(value, 10, 64); err == nil {
			return intVal
		}
		log.Printf("Warning: invalid int64 value for %s: %s, using default %d", key, value, defaultValue)
	}
	return defaultValue
}

func getEnvBool(key string, defaultValue bool) bool {
	if value := os.Getenv(key); value != "" {
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "1", "true", "yes", "on":
			return true
		case "0", "false", "no", "off":
			return false
		}
		log.Printf("Warning: invalid bool value for %s: %s, using default %v", key, value, defaultValue)
	}
	return defaultValue
}
