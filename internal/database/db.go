package database

import (
	"database/sql"
	"fmt"
	"log"
	"strings"

	_ "github.com/mattn/go-sqlite3"
)

type DB struct {
	*sql.DB
}

func New(dbPath string) (*DB, error) {
	db, err := sql.Open("sqlite3", dbPath+"?_foreign_keys=on&_busy_timeout=5000")
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("failed to ping database: %w", err)
	}

	// Enable WAL mode
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		return nil, fmt.Errorf("failed to enable WAL mode: %w", err)
	}

	return &DB{DB: db}, nil
}

func (db *DB) Migrate(migrationsPath string) error {
	return db.migrateFromFile(migrationsPath)
}

func (db *DB) migrateFromFile(path string) error {
	// Recorded before the schema runs: the pass list is seeded from history
	// only on the run that creates it, never again afterwards.
	hadVerifiedUsers, err := db.HasTable("verified_users")
	if err != nil {
		return err
	}

	migration := `
-- Enable WAL mode for better concurrency
PRAGMA journal_mode=WAL;

-- Image sets table
CREATE TABLE IF NOT EXISTS image_sets (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    label TEXT NOT NULL UNIQUE,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

-- Images table
CREATE TABLE IF NOT EXISTS images (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    set_id INTEGER NOT NULL,
    file_id TEXT NOT NULL,
    file_path TEXT,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (set_id) REFERENCES image_sets(id) ON DELETE CASCADE
);

-- Pending verifications table
CREATE TABLE IF NOT EXISTS pending_verifications (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    chat_id INTEGER NOT NULL,
    user_id INTEGER NOT NULL,
    message_id INTEGER,
    join_message_id INTEGER,
    correct_labels TEXT NOT NULL,
    current_step INTEGER DEFAULT 0,
    user_answers TEXT DEFAULT '[]',
    retry_count INTEGER DEFAULT 0,
    expires_at INTEGER NOT NULL,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(chat_id, user_id)
);

-- Admins table
CREATE TABLE IF NOT EXISTS admins (
    user_id INTEGER PRIMARY KEY,
    added_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

-- User join history for anti-spam
CREATE TABLE IF NOT EXISTS user_join_history (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    chat_id INTEGER NOT NULL,
    user_id INTEGER NOT NULL,
    joined_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

-- Verification failures table (tracks failures across sessions)
CREATE TABLE IF NOT EXISTS verification_failures (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    chat_id INTEGER NOT NULL,
    user_id INTEGER NOT NULL,
    failed_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(chat_id, user_id)
);

-- Indexes for performance
CREATE INDEX IF NOT EXISTS idx_images_set_id ON images(set_id);
CREATE INDEX IF NOT EXISTS idx_pending_user_chat ON pending_verifications(user_id, chat_id);
CREATE INDEX IF NOT EXISTS idx_pending_expires ON pending_verifications(expires_at);
CREATE INDEX IF NOT EXISTS idx_join_history ON user_join_history(chat_id, user_id, joined_at);
CREATE INDEX IF NOT EXISTS idx_failures_lookup ON verification_failures(chat_id, user_id);
CREATE INDEX IF NOT EXISTS idx_failures_cleanup ON verification_failures(failed_at);

-- User language preferences
CREATE TABLE IF NOT EXISTS user_language_preferences (
    user_id INTEGER PRIMARY KEY,
    language_code TEXT NOT NULL DEFAULT 'zh',
    updated_at INTEGER DEFAULT (strftime('%s', 'now'))
);

-- Image set multilingual labels
CREATE TABLE IF NOT EXISTS image_set_labels (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    set_id INTEGER NOT NULL,
    language_code TEXT NOT NULL,
    label TEXT NOT NULL,
    created_at INTEGER DEFAULT (strftime('%s', 'now')),
    FOREIGN KEY (set_id) REFERENCES image_sets(id) ON DELETE CASCADE,
    UNIQUE(set_id, language_code)
);

CREATE INDEX IF NOT EXISTS idx_set_labels_lookup ON image_set_labels(set_id, language_code);

-- Migrate existing labels as Chinese
INSERT OR IGNORE INTO image_set_labels (set_id, language_code, label)
SELECT id, 'zh', label FROM image_sets;

-- Clean up any invalid pending_verifications with non-integer expires_at
-- (leftover from pre-migration-003 data)
DELETE FROM pending_verifications WHERE CAST(expires_at AS TEXT) LIKE '%-%';

-- Leaderboard table to track test completion times
CREATE TABLE IF NOT EXISTS leaderboard (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id INTEGER NOT NULL,
    username TEXT NOT NULL,
    first_name TEXT NOT NULL,
    last_name TEXT,
    completion_time_ms INTEGER NOT NULL,
    completed_at INTEGER NOT NULL,
    UNIQUE(user_id)
);

CREATE INDEX IF NOT EXISTS idx_leaderboard_time ON leaderboard(completion_time_ms ASC);
CREATE INDEX IF NOT EXISTS idx_leaderboard_completed ON leaderboard(completed_at DESC);

-- Verification events: one row per terminal outcome (passed/failed/expired)
CREATE TABLE IF NOT EXISTS verification_events (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    chat_id INTEGER NOT NULL,
    user_id INTEGER NOT NULL,
    outcome TEXT NOT NULL CHECK (outcome IN ('passed', 'failed', 'expired')),
    completion_time_ms INTEGER,
    failed_at_step INTEGER,
    is_test INTEGER NOT NULL DEFAULT 0,
    user_lang TEXT,
    regenerated INTEGER NOT NULL DEFAULT 0,
    admin_user_id INTEGER,
    occurred_at INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_events_test_occurred ON verification_events(is_test, occurred_at);

-- Mini App pre-join verification sessions.
--
-- One row per chat_join_request the bot decides to send through the Mini App
-- flow. The nonce (random 32-byte URL-safe string) is the public handle the
-- Mini App uses to identify the session over HTTP. The row also carries the
-- Telegram-issued query_id required to call answerChatJoinRequestQuery
-- within Telegram's 10-second window after Mini App success.
--
-- status transitions:
--   awaiting_dm      → no-query_id chat_join_request: bot is waiting up to
--                      5 min for the user to /start it in DM. Created when
--                      Telegram delivers an old-style join request that we
--                      can't drive through sendChatJoinRequestWebApp. On
--                      /start, transitions to 'pending' (Mini App button
--                      sent). On TTL expiry, sweeper applies the chat's
--                      configured join_policy (approve|decline|dismiss).
--   pending          → row created when chat_join_request arrives, OR
--                      awaiting_dm → pending after we deliver the Mini App
--                      button to the user in DM.
--   approved_prejoin → answerChatJoinRequestQuery=approve confirmed by
--                      Telegram. The follow-up chat_member 'left → member'
--                      transition uses this to skip the in-group captcha.
--   declined         → answerChatJoinRequestQuery=decline (failed captcha
--                      twice, or proven spam)
--   expired          → never solved before expires_at; we declined or
--                      let Telegram time out. Also used when approving →
--                      Telegram API failed: there's no admit, so we free
--                      the unique-pending slot for a retry.
--
-- The unique partial index on (chat_id, user_id) for status='pending' makes
-- duplicate join requests idempotent: if Telegram re-delivers the same
-- update, we hand the user back the existing in-flight session instead of
-- starting two.
CREATE TABLE IF NOT EXISTS miniapp_sessions (
    nonce TEXT PRIMARY KEY,
    chat_id INTEGER NOT NULL,
    user_id INTEGER NOT NULL,
    query_id TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'pending'
        CHECK (status IN ('awaiting_dm', 'pending', 'approving', 'approved_prejoin', 'declined', 'expired')),
    expires_at INTEGER NOT NULL,
    created_at INTEGER NOT NULL DEFAULT (strftime('%s', 'now')),
    completed_at INTEGER
);

CREATE INDEX IF NOT EXISTS idx_miniapp_user_chat ON miniapp_sessions(chat_id, user_id, status);
CREATE INDEX IF NOT EXISTS idx_miniapp_expires
    ON miniapp_sessions(expires_at) WHERE status IN ('pending', 'awaiting_dm');
-- Block duplicate active sessions for the same (chat, user). Includes
-- awaiting_dm so a repeated chat_join_request can find and reuse the
-- in-flight session rather than racing.
CREATE UNIQUE INDEX IF NOT EXISTS idx_miniapp_pending_unique
    ON miniapp_sessions(chat_id, user_id) WHERE status IN ('pending', 'awaiting_dm');
-- Used by /start to claim awaiting_dm sessions by user_id without scanning.
-- Partial index: only awaiting_dm rows are large enough in cardinality to
-- matter for this query, and they're the only ones we'd want to claim.
CREATE INDEX IF NOT EXISTS idx_miniapp_awaiting_user
    ON miniapp_sessions(user_id, expires_at) WHERE status = 'awaiting_dm';

-- Per-chat fallback policy for chat_join_request updates that arrive
-- WITHOUT query_id (older chats, older invite links, older Telegram
-- clients — see Guard Bots rollout). For chats with this row absent, the
-- bot uses the global default 'approve' (legacy approveChatJoinRequest +
-- in-group captcha) which matches pre-policy behavior.
--
--   approve → call approveChatJoinRequest; the chat_member 'left → member'
--             transition then triggers the existing in-group inline-button
--             captcha. The user gets verified, just not via Mini App.
--   decline → call declineChatJoinRequest; the user is rejected outright
--             with no captcha. Useful for very-high-spam chats where
--             admins would rather risk false-positives than process spam.
--   ignore  → do nothing; the request stays in Telegram's pending queue
--             for admins to handle manually.
CREATE TABLE IF NOT EXISTS chat_join_policy (
    chat_id INTEGER PRIMARY KEY,
    on_missing_query_id TEXT NOT NULL DEFAULT 'approve'
        CHECK (on_missing_query_id IN ('approve', 'decline', 'dismiss')),
    updated_by INTEGER,
    updated_at INTEGER NOT NULL DEFAULT (strftime('%s', 'now'))
);

-- Per-chat overrides for verification behaviour. Each column is nullable
-- so an unset value falls back to the global env config; that lets admins
-- override just the toggles they care about without carrying the rest.
CREATE TABLE IF NOT EXISTS chat_settings (
    chat_id INTEGER PRIMARY KEY,
    -- NULL = inherit config.EphemeralVerifyEnabled, 0 = force off, 1 = force on.
    ephemeral_verify INTEGER,
    -- When ephemeral_verify resolves to on, also broadcast the challenge
    -- to every chat admin as their own ephemeral message so admins can
    -- watch attempts happen. 0 = user-only, 1 = admins see it too.
    ephemeral_admin_visible INTEGER NOT NULL DEFAULT 0,
    -- Seconds after which the bot removes its own "passed verification"
    -- notice from the group. 0 = leave it in the chat.
    pass_notice_ttl_seconds INTEGER NOT NULL DEFAULT 0,
    updated_by INTEGER,
    updated_at INTEGER NOT NULL DEFAULT (strftime('%s', 'now'))
);

-- Messages the bot has promised to delete later (see chat_settings.
-- pass_notice_ttl_seconds). Kept in the database rather than in timers so a
-- restart between sending a notice and its deadline doesn't strand it.
CREATE TABLE IF NOT EXISTS scheduled_message_deletions (
    chat_id INTEGER NOT NULL,
    message_id INTEGER NOT NULL,
    delete_at INTEGER NOT NULL,
    PRIMARY KEY (chat_id, message_id)
);
CREATE INDEX IF NOT EXISTS idx_scheduled_deletions_due
    ON scheduled_message_deletions(delete_at);

-- Tracks the ephemeral message IDs delivered to each admin during a
-- verification when ephemeral_admin_visible=1. Separate table because
-- there are up to N rows per pending_verification (one per admin), and
-- we need to delete each on cleanup. Rows are dropped in the same paths
-- that clean up pending_verifications.
CREATE TABLE IF NOT EXISTS verification_admin_ephemerals (
    chat_id INTEGER NOT NULL,
    user_id INTEGER NOT NULL,
    admin_id INTEGER NOT NULL,
    ephemeral_message_id INTEGER NOT NULL,
    challenge_version INTEGER NOT NULL DEFAULT 0,
    created_at INTEGER NOT NULL DEFAULT (strftime('%s', 'now')),
    PRIMARY KEY (chat_id, user_id, admin_id)
);
CREATE INDEX IF NOT EXISTS idx_admin_ephemerals_lookup
    ON verification_admin_ephemerals(chat_id, user_id);

-- Global pass list: users who solved a captcha themselves in any chat (or
-- were imported by an operator) and so skip verification in every other
-- chat the bot guards. Admin approvals never add a row here.
--   chat_id → where the user qualified
--   source  → 'captcha' (in-group), 'miniapp' (pre-join), 'import'
CREATE TABLE IF NOT EXISTS verified_users (
    user_id INTEGER PRIMARY KEY,
    chat_id INTEGER NOT NULL,
    source TEXT NOT NULL,
    verified_at INTEGER NOT NULL
);
`
	if _, err := db.Exec(migration); err != nil {
		return fmt.Errorf("failed to run migration: %w", err)
	}

	// One-time rebuild: the miniapp_sessions CHECK constraint and partial
	// indexes evolved as new statuses (approving, awaiting_dm) were added
	// to support race-free approve and the DM fallback flow. SQLite can't
	// ALTER a CHECK constraint, so we detect outdated DDL and rebuild the
	// table. Sessions are short-lived (5min TTL) and live only for in-flight
	// join requests, so destructive rebuild is safe — in-flight users hit
	// the legacy fallback after retry.
	var miniappDDL sql.NullString
	if err := db.QueryRow(
		`SELECT sql FROM sqlite_master WHERE type='table' AND name='miniapp_sessions'`,
	).Scan(&miniappDDL); err == nil && miniappDDL.Valid && !strings.Contains(miniappDDL.String, "awaiting_dm") {
		log.Printf("[INFO] migrating miniapp_sessions to add 'awaiting_dm' status (table rebuild)")
		rebuild := `
			DROP INDEX IF EXISTS idx_miniapp_user_chat;
			DROP INDEX IF EXISTS idx_miniapp_expires;
			DROP INDEX IF EXISTS idx_miniapp_pending_unique;
			DROP TABLE IF EXISTS miniapp_sessions;
			CREATE TABLE miniapp_sessions (
				nonce TEXT PRIMARY KEY,
				chat_id INTEGER NOT NULL,
				user_id INTEGER NOT NULL,
				query_id TEXT NOT NULL,
				status TEXT NOT NULL DEFAULT 'pending'
					CHECK (status IN ('awaiting_dm', 'pending', 'approving', 'approved_prejoin', 'declined', 'expired')),
				expires_at INTEGER NOT NULL,
				created_at INTEGER NOT NULL DEFAULT (strftime('%s', 'now')),
				completed_at INTEGER
			);
			CREATE INDEX IF NOT EXISTS idx_miniapp_user_chat ON miniapp_sessions(chat_id, user_id, status);
			CREATE INDEX IF NOT EXISTS idx_miniapp_expires
				ON miniapp_sessions(expires_at) WHERE status IN ('pending', 'awaiting_dm');
			CREATE UNIQUE INDEX IF NOT EXISTS idx_miniapp_pending_unique
				ON miniapp_sessions(chat_id, user_id) WHERE status IN ('pending', 'awaiting_dm');
			CREATE INDEX IF NOT EXISTS idx_miniapp_awaiting_user
				ON miniapp_sessions(user_id, expires_at) WHERE status = 'awaiting_dm';
		`
		if _, err := db.Exec(rebuild); err != nil {
			return fmt.Errorf("rebuild miniapp_sessions: %w", err)
		}
	}

	// Backfill the awaiting_user index for deployments that ran the previous
	// (table-rebuild) migration before the index was added. CREATE INDEX
	// IF NOT EXISTS is a no-op when the index already exists.
	if _, err := db.Exec(
		`CREATE INDEX IF NOT EXISTS idx_miniapp_awaiting_user
		     ON miniapp_sessions(user_id, expires_at) WHERE status = 'awaiting_dm'`,
	); err != nil {
		return fmt.Errorf("create idx_miniapp_awaiting_user: %w", err)
	}

	// chat_join_policy's CHECK gained 'dismiss' (renamed from 'ignore'),
	// so old deployments need a constraint rebuild. The previous version of
	// this migration dropped the table outright; we now copy rows to a
	// scratch table first, mapping any 'ignore' values to 'dismiss', so
	// admins don't lose their per-chat configuration on upgrade.
	var policyDDL sql.NullString
	if err := db.QueryRow(
		`SELECT sql FROM sqlite_master WHERE type='table' AND name='chat_join_policy'`,
	).Scan(&policyDDL); err == nil && policyDDL.Valid && !strings.Contains(policyDDL.String, "dismiss") {
		log.Printf("[INFO] migrating chat_join_policy to rename 'ignore' → 'dismiss' (preserving rows)")
		rebuild := `
			CREATE TABLE chat_join_policy_new (
				chat_id INTEGER PRIMARY KEY,
				on_missing_query_id TEXT NOT NULL DEFAULT 'approve'
					CHECK (on_missing_query_id IN ('approve', 'decline', 'dismiss')),
				updated_by INTEGER,
				updated_at INTEGER NOT NULL DEFAULT (strftime('%s', 'now'))
			);
			INSERT INTO chat_join_policy_new (chat_id, on_missing_query_id, updated_by, updated_at)
			SELECT chat_id,
			       CASE WHEN on_missing_query_id = 'ignore' THEN 'dismiss'
			            ELSE on_missing_query_id END,
			       updated_by,
			       updated_at
			  FROM chat_join_policy;
			DROP TABLE chat_join_policy;
			ALTER TABLE chat_join_policy_new RENAME TO chat_join_policy;
		`
		if _, err := db.Exec(rebuild); err != nil {
			return fmt.Errorf("rebuild chat_join_policy: %w", err)
		}
	}

	// Additive column migrations for existing deployments.
	// "duplicate column name" is expected on subsequent runs; any other error is real.
	additiveColumns := []struct {
		table, column, ddl string
	}{
		{"pending_verifications", "join_message_id", "ALTER TABLE pending_verifications ADD COLUMN join_message_id INTEGER"},
		{"pending_verifications", "is_test", "ALTER TABLE pending_verifications ADD COLUMN is_test INTEGER NOT NULL DEFAULT 0"},
		{"pending_verifications", "user_lang", "ALTER TABLE pending_verifications ADD COLUMN user_lang TEXT"},
		{"pending_verifications", "regenerated", "ALTER TABLE pending_verifications ADD COLUMN regenerated INTEGER NOT NULL DEFAULT 0"},
		{"pending_verifications", "challenge_version", "ALTER TABLE pending_verifications ADD COLUMN challenge_version INTEGER NOT NULL DEFAULT 0"},
		{"pending_verifications", "ephemeral_message_id", "ALTER TABLE pending_verifications ADD COLUMN ephemeral_message_id INTEGER NOT NULL DEFAULT 0"},
		{"pending_verifications", "admin_broadcast", "ALTER TABLE pending_verifications ADD COLUMN admin_broadcast INTEGER NOT NULL DEFAULT 0"},
		{"pending_verifications", "options", "ALTER TABLE pending_verifications ADD COLUMN options TEXT NOT NULL DEFAULT '[]'"},
		{"verification_events", "regenerated", "ALTER TABLE verification_events ADD COLUMN regenerated INTEGER NOT NULL DEFAULT 0"},
		{"verification_events", "admin_user_id", "ALTER TABLE verification_events ADD COLUMN admin_user_id INTEGER"},
		{"chat_settings", "pass_notice_ttl_seconds", "ALTER TABLE chat_settings ADD COLUMN pass_notice_ttl_seconds INTEGER NOT NULL DEFAULT 0"},
	}
	for _, c := range additiveColumns {
		if _, err := db.Exec(c.ddl); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
			return fmt.Errorf("failed to add %s.%s column: %w", c.table, c.column, err)
		}
	}

	// After the column migrations: the seed reads verification_events.admin_user_id.
	if !hadVerifiedUsers {
		if err := db.seedVerifiedUsers(); err != nil {
			return err
		}
	}

	return nil
}
