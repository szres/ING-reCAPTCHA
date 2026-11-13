package database

import (
	"database/sql"
	"errors"
	"fmt"
)

// Allowed values for chat_join_policy.on_missing_query_id. These describe
// what the bot does when the 5-minute DM-Mini-App window expires without a
// successful verification — NOT what it does the moment chat_join_request
// arrives without query_id. The first action on a no-query_id request is
// always the same (attempt DM Mini App); these values pick the fallback.
const (
	JoinPolicyApprove = "approve"
	JoinPolicyDecline = "decline"
	JoinPolicyDismiss = "dismiss"
)

// IsValidJoinPolicy reports whether s is one of the three allowed policy
// values. Callers should reject anything else before reaching the DB —
// the CHECK constraint enforces it as a backstop but the error message is
// less actionable than a pre-DB validation.
func IsValidJoinPolicy(s string) bool {
	switch s {
	case JoinPolicyApprove, JoinPolicyDecline, JoinPolicyDismiss:
		return true
	}
	return false
}

// GetChatJoinPolicy returns the configured fallback policy for the given
// chat, or the empty string if no row exists. Callers treat "no row" as
// "use the global default" — see SetChatJoinPolicy doc.
func (db *DB) GetChatJoinPolicy(chatID int64) (string, error) {
	var policy string
	err := db.QueryRow(
		`SELECT on_missing_query_id FROM chat_join_policy WHERE chat_id = ?`,
		chatID,
	).Scan(&policy)
	switch {
	case err == nil:
		return policy, nil
	case errors.Is(err, sql.ErrNoRows):
		return "", nil
	default:
		return "", err
	}
}

// SetChatJoinPolicy upserts the per-chat policy. updatedBy is the Telegram
// user id of the admin who issued the change, recorded for audit.
//
// The global default for chats with no row is 'approve' — pre-policy
// behavior. The handler resolves it at lookup time rather than seeding a
// row for every chat the bot has ever seen.
func (db *DB) SetChatJoinPolicy(chatID int64, policy string, updatedBy int64) error {
	if !IsValidJoinPolicy(policy) {
		return fmt.Errorf("invalid join policy %q", policy)
	}
	_, err := db.Exec(
		`INSERT INTO chat_join_policy (chat_id, on_missing_query_id, updated_by, updated_at)
		      VALUES (?, ?, ?, strftime('%s','now'))
		      ON CONFLICT(chat_id) DO UPDATE SET
		          on_missing_query_id = excluded.on_missing_query_id,
		          updated_by          = excluded.updated_by,
		          updated_at          = excluded.updated_at`,
		chatID, policy, updatedBy,
	)
	return err
}
