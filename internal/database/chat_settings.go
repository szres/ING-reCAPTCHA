package database

import (
	"database/sql"
	"errors"
	"fmt"
)

// ChatSettings holds per-group overrides for verification behaviour. Any
// pointer field being nil means "inherit from global config"; non-nil
// values are explicit choices set by a group admin via /setting.
//
// Kept minimal on purpose — this table is meant for behavioural toggles
// that make sense per-group, not for wholesale duplication of the env
// config. New toggles get their own nullable column here.
type ChatSettings struct {
	ChatID                int64
	EphemeralVerify       *bool // nil = inherit env, non-nil = explicit
	EphemeralAdminVisible bool
	// PassNoticeTTLSeconds is how long the "passed verification" notice
	// stays in the group before the bot deletes it. 0 = keep it.
	PassNoticeTTLSeconds int
	UpdatedBy            sql.NullInt64
	UpdatedAt            int64
}

// GetChatSettings returns the row for chatID or nil if none exists.
// A nil return means "everything inherits config defaults" — callers
// should treat that identically to a fully-default ChatSettings.
func (db *DB) GetChatSettings(chatID int64) (*ChatSettings, error) {
	var (
		cs                 ChatSettings
		ephemeralVerifyRaw sql.NullInt64
		adminVisibleRaw    int
	)
	cs.ChatID = chatID
	err := db.QueryRow(
		`SELECT ephemeral_verify, ephemeral_admin_visible, pass_notice_ttl_seconds, updated_by, updated_at
		   FROM chat_settings
		  WHERE chat_id = ?`,
		chatID,
	).Scan(&ephemeralVerifyRaw, &adminVisibleRaw, &cs.PassNoticeTTLSeconds, &cs.UpdatedBy, &cs.UpdatedAt)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("get chat_settings: %w", err)
	}
	if ephemeralVerifyRaw.Valid {
		v := ephemeralVerifyRaw.Int64 != 0
		cs.EphemeralVerify = &v
	}
	cs.EphemeralAdminVisible = adminVisibleRaw != 0
	return &cs, nil
}

// SetChatEphemeralVerify writes the ephemeral_verify override for chatID.
// Pass nil to clear the override (fall back to global config).
func (db *DB) SetChatEphemeralVerify(chatID int64, value *bool, updatedBy int64) error {
	var raw interface{}
	if value != nil {
		if *value {
			raw = 1
		} else {
			raw = 0
		}
	}
	_, err := db.Exec(
		`INSERT INTO chat_settings (chat_id, ephemeral_verify, updated_by, updated_at)
		      VALUES (?, ?, ?, strftime('%s','now'))
		 ON CONFLICT(chat_id) DO UPDATE SET
		      ephemeral_verify = excluded.ephemeral_verify,
		      updated_by       = excluded.updated_by,
		      updated_at       = excluded.updated_at`,
		chatID, raw, updatedBy,
	)
	if err != nil {
		return fmt.Errorf("set ephemeral_verify: %w", err)
	}
	return nil
}

// SetChatEphemeralAdminVisible writes the admin-visibility override for
// chatID. This one is not nullable — false is the intended default so
// there's no need to distinguish "unset" from "explicitly off".
func (db *DB) SetChatEphemeralAdminVisible(chatID int64, value bool, updatedBy int64) error {
	v := 0
	if value {
		v = 1
	}
	_, err := db.Exec(
		`INSERT INTO chat_settings (chat_id, ephemeral_admin_visible, updated_by, updated_at)
		      VALUES (?, ?, ?, strftime('%s','now'))
		 ON CONFLICT(chat_id) DO UPDATE SET
		      ephemeral_admin_visible = excluded.ephemeral_admin_visible,
		      updated_by              = excluded.updated_by,
		      updated_at              = excluded.updated_at`,
		chatID, v, updatedBy,
	)
	if err != nil {
		return fmt.Errorf("set ephemeral_admin_visible: %w", err)
	}
	return nil
}

// SetChatPassNoticeTTL writes how many seconds the "passed verification"
// notice stays in chatID before the bot deletes it. 0 turns deletion off.
func (db *DB) SetChatPassNoticeTTL(chatID int64, seconds int, updatedBy int64) error {
	_, err := db.Exec(
		`INSERT INTO chat_settings (chat_id, pass_notice_ttl_seconds, updated_by, updated_at)
		      VALUES (?, ?, ?, strftime('%s','now'))
		 ON CONFLICT(chat_id) DO UPDATE SET
		      pass_notice_ttl_seconds = excluded.pass_notice_ttl_seconds,
		      updated_by              = excluded.updated_by,
		      updated_at              = excluded.updated_at`,
		chatID, seconds, updatedBy,
	)
	if err != nil {
		return fmt.Errorf("set pass_notice_ttl_seconds: %w", err)
	}
	return nil
}

// AdminEphemeral is one row of the admin-broadcast tracker.
type AdminEphemeral struct {
	AdminID            int64
	EphemeralMessageID int
	ChallengeVersion   int
}

// RecordAdminEphemeral stores one admin-broadcast delivery so cleanup
// paths can delete it later. Upserts on (chat_id, user_id, admin_id) —
// a regenerate replaces the previous version's ID atomically.
func (db *DB) RecordAdminEphemeral(chatID, userID, adminID int64, ephemeralMessageID, challengeVersion int) error {
	_, err := db.Exec(
		`INSERT INTO verification_admin_ephemerals
		      (chat_id, user_id, admin_id, ephemeral_message_id, challenge_version)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(chat_id, user_id, admin_id) DO UPDATE SET
		      ephemeral_message_id = excluded.ephemeral_message_id,
		      challenge_version    = excluded.challenge_version,
		      created_at           = strftime('%s','now')`,
		chatID, userID, adminID, ephemeralMessageID, challengeVersion,
	)
	if err != nil {
		return fmt.Errorf("record admin ephemeral: %w", err)
	}
	return nil
}

// ListAdminEphemerals returns every admin-broadcast row for a pending
// verification. Called by cleanup paths (success/fail/timeout/regenerate)
// before dropping the messages via deleteEphemeralMessage.
func (db *DB) ListAdminEphemerals(chatID, userID int64) ([]AdminEphemeral, error) {
	rows, err := db.Query(
		`SELECT admin_id, ephemeral_message_id, challenge_version
		   FROM verification_admin_ephemerals
		  WHERE chat_id = ? AND user_id = ?`,
		chatID, userID,
	)
	if err != nil {
		return nil, fmt.Errorf("list admin ephemerals: %w", err)
	}
	defer rows.Close()
	var out []AdminEphemeral
	for rows.Next() {
		var ae AdminEphemeral
		if err := rows.Scan(&ae.AdminID, &ae.EphemeralMessageID, &ae.ChallengeVersion); err != nil {
			return nil, fmt.Errorf("scan admin ephemeral: %w", err)
		}
		out = append(out, ae)
	}
	return out, rows.Err()
}

// DeleteAdminEphemerals removes tracker rows for a pending verification.
// Callers must have already invoked deleteEphemeralMessage on each ID
// they got from ListAdminEphemerals; this only clears the DB tracker.
func (db *DB) DeleteAdminEphemerals(chatID, userID int64) error {
	_, err := db.Exec(
		`DELETE FROM verification_admin_ephemerals
		  WHERE chat_id = ? AND user_id = ?`,
		chatID, userID,
	)
	if err != nil {
		return fmt.Errorf("delete admin ephemerals: %w", err)
	}
	return nil
}
