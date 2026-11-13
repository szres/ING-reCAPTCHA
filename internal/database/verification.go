package database

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

type PendingVerification struct {
	ID        int64
	ChatID    int64
	UserID    int64
	MessageID sql.NullInt64
	// EphemeralMessageID is non-zero when the challenge was sent as a
	// Bot API 10.2 Ephemeral Message (visible only to the joining user).
	// The zero value means "not ephemeral, use the regular MessageID."
	// Delete/cleanup paths branch on this to pick between the standard
	// deleteMessage and the ephemeral-only deleteEphemeralMessage.
	EphemeralMessageID int
	// AdminBroadcast records whether the initial send also broadcast
	// the challenge to group admins as their own ephemerals (per the
	// chat_settings.ephemeral_admin_visible toggle at start-time).
	// Persisted so regenerate honors the ORIGINAL decision even if an
	// admin flips the setting mid-verification, and so cleanup knows
	// whether to bother looking for tracker rows.
	AdminBroadcast bool
	// Options is the shuffled label keypad shown for this challenge, written
	// in the same statement that creates or regenerates it. Each step's
	// keyboard is rebuilt from it because ephemeral-message callbacks aren't
	// guaranteed to carry the original reply_markup.
	Options          []string
	JoinMessageID    sql.NullInt64
	CorrectLabels    []string
	CurrentStep      int
	UserAnswers      []string
	RetryCount       int
	ExpiresAt        time.Time
	CreatedAt        time.Time
	IsTest           bool
	UserLang         string
	Regenerated      bool
	ChallengeVersion int
}

func (db *DB) CreatePendingVerification(chatID, userID int64, correctLabels []string, expiresAt time.Time, isTest bool, userLang string, options []string) error {
	labelsJSON, err := json.Marshal(correctLabels)
	if err != nil {
		return fmt.Errorf("failed to marshal correct labels: %w", err)
	}
	optionsJSON, err := marshalOptions(options)
	if err != nil {
		return err
	}

	// Store expires_at as UNIX timestamp (integer) to avoid timezone issues
	expiresAtUnix := expiresAt.Unix()
	isTestInt := 0
	if isTest {
		isTestInt = 1
	}

	_, err = db.Exec(`
		INSERT INTO pending_verifications (chat_id, user_id, correct_labels, expires_at, is_test, user_lang, options)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(chat_id, user_id) DO UPDATE SET
			correct_labels = excluded.correct_labels,
			current_step = 0,
			user_answers = '[]',
			retry_count = pending_verifications.retry_count,
			expires_at = excluded.expires_at,
			message_id = NULL,
			ephemeral_message_id = 0,
			admin_broadcast = 0,
			options = excluded.options,
			is_test = excluded.is_test,
			user_lang = excluded.user_lang,
			regenerated = 0,
			challenge_version = 0
	`, chatID, userID, string(labelsJSON), expiresAtUnix, isTestInt, userLang, optionsJSON)
	if err != nil {
		return fmt.Errorf("failed to create pending verification: %w", err)
	}
	return nil
}

func (db *DB) UpdateVerificationMessageID(chatID, userID int64, messageID int) error {
	_, err := db.Exec(
		"UPDATE pending_verifications SET message_id = ? WHERE chat_id = ? AND user_id = ?",
		messageID, chatID, userID,
	)
	if err != nil {
		return fmt.Errorf("failed to update message id: %w", err)
	}
	return nil
}

// UpdateVerificationEphemeralMessageID records that the challenge for
// this pending verification was sent as an Ephemeral Message (Bot API
// 10.2). Cleanup/regenerate paths read this ID to know whether to use
// deleteEphemeralMessage instead of the standard delete.
func (db *DB) UpdateVerificationEphemeralMessageID(chatID, userID int64, ephemeralMessageID int) error {
	_, err := db.Exec(
		"UPDATE pending_verifications SET ephemeral_message_id = ? WHERE chat_id = ? AND user_id = ?",
		ephemeralMessageID, chatID, userID,
	)
	if err != nil {
		return fmt.Errorf("failed to update ephemeral message id: %w", err)
	}
	return nil
}

// SetVerificationAdminBroadcast records whether admin-visibility was on
// when the initial challenge was sent. Regenerate reads this rather
// than re-querying chat_settings so an admin flipping /setting adminview
// mid-verification can't leave orphan tracker rows.
func (db *DB) SetVerificationAdminBroadcast(chatID, userID int64, on bool) error {
	v := 0
	if on {
		v = 1
	}
	_, err := db.Exec(
		"UPDATE pending_verifications SET admin_broadcast = ? WHERE chat_id = ? AND user_id = ?",
		v, chatID, userID,
	)
	if err != nil {
		return fmt.Errorf("failed to update admin_broadcast: %w", err)
	}
	return nil
}

func (db *DB) UpdateJoinMessageID(chatID, userID int64, messageID int) error {
	_, err := db.Exec(
		"UPDATE pending_verifications SET join_message_id = ? WHERE chat_id = ? AND user_id = ?",
		messageID, chatID, userID,
	)
	if err != nil {
		return fmt.Errorf("failed to update join message id: %w", err)
	}
	return nil
}

func (db *DB) GetPendingVerification(chatID, userID int64) (*PendingVerification, error) {
	var pv PendingVerification
	var labelsJSON, answersJSON, optionsJSON string
	var expiresAtUnix int64
	var isTestInt, regeneratedInt, adminBroadcastInt int
	var userLang sql.NullString

	err := db.QueryRow(`
		SELECT id, chat_id, user_id, message_id, ephemeral_message_id, admin_broadcast, options, join_message_id, correct_labels, current_step, user_answers, retry_count, expires_at, created_at, is_test, user_lang, regenerated, challenge_version
		FROM pending_verifications
		WHERE chat_id = ? AND user_id = ?
	`, chatID, userID).Scan(
		&pv.ID, &pv.ChatID, &pv.UserID, &pv.MessageID, &pv.EphemeralMessageID, &adminBroadcastInt, &optionsJSON, &pv.JoinMessageID,
		&labelsJSON, &pv.CurrentStep, &answersJSON,
		&pv.RetryCount, &expiresAtUnix, &pv.CreatedAt,
		&isTestInt, &userLang,
		&regeneratedInt, &pv.ChallengeVersion,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get pending verification: %w", err)
	}

	// Convert UNIX timestamp back to time.Time
	pv.ExpiresAt = time.Unix(expiresAtUnix, 0)
	pv.IsTest = isTestInt != 0
	pv.Regenerated = regeneratedInt != 0
	pv.AdminBroadcast = adminBroadcastInt != 0
	if userLang.Valid {
		pv.UserLang = userLang.String
	}

	if err := json.Unmarshal([]byte(labelsJSON), &pv.CorrectLabels); err != nil {
		return nil, fmt.Errorf("failed to unmarshal correct labels: %w", err)
	}
	if err := json.Unmarshal([]byte(answersJSON), &pv.UserAnswers); err != nil {
		return nil, fmt.Errorf("failed to unmarshal user answers: %w", err)
	}
	pv.Options = decodeOptions(optionsJSON)

	return &pv, nil
}

// decodeOptions parses the stored keypad. A malformed value degrades to nil
// (callers then fall back to scraping the keyboard) rather than failing the
// whole lookup, since options are a rebuild aid, not verification state.
func decodeOptions(raw string) []string {
	var opts []string
	if err := json.Unmarshal([]byte(raw), &opts); err != nil {
		return nil
	}
	return opts
}

// marshalOptions encodes the keypad for storage. A nil slice is stored as an
// empty list so the column never holds JSON null.
func marshalOptions(options []string) (string, error) {
	if options == nil {
		options = []string{}
	}
	raw, err := json.Marshal(options)
	if err != nil {
		return "", fmt.Errorf("failed to marshal options: %w", err)
	}
	return string(raw), nil
}

// UpdateVerificationStep advances the step and answers as a compare-and-swap on
// both challenge_version and current_step. Callbacks are handled concurrently,
// so two taps on the same step can both pass a read-time check; the step guard
// makes exactly one of them win. Returns false when either guard misses — the
// challenge was regenerated, or another tap already advanced this step — and
// the caller must not record the answer.
func (db *DB) UpdateVerificationStep(chatID, userID int64, expectedStep, newStep int, answers []string, expectedVersion int) (bool, error) {
	answersJSON, err := json.Marshal(answers)
	if err != nil {
		return false, fmt.Errorf("failed to marshal answers: %w", err)
	}

	result, err := db.Exec(
		"UPDATE pending_verifications SET current_step = ?, user_answers = ? WHERE chat_id = ? AND user_id = ? AND challenge_version = ? AND current_step = ?",
		newStep, string(answersJSON), chatID, userID, expectedVersion, expectedStep,
	)
	if err != nil {
		return false, fmt.Errorf("failed to update verification step: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("failed to get rows affected: %w", err)
	}
	return rows > 0, nil
}

func (db *DB) IncrementRetryCount(chatID, userID int64) (int, error) {
	_, err := db.Exec(
		"UPDATE pending_verifications SET retry_count = retry_count + 1, current_step = 0, user_answers = '[]' WHERE chat_id = ? AND user_id = ?",
		chatID, userID,
	)
	if err != nil {
		return 0, fmt.Errorf("failed to increment retry count: %w", err)
	}

	var count int
	err = db.QueryRow("SELECT retry_count FROM pending_verifications WHERE chat_id = ? AND user_id = ?", chatID, userID).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("failed to get retry count: %w", err)
	}
	return count, nil
}

func (db *DB) DeletePendingVerification(chatID, userID int64) error {
	_, err := db.Exec("DELETE FROM pending_verifications WHERE chat_id = ? AND user_id = ?", chatID, userID)
	if err != nil {
		return fmt.Errorf("failed to delete pending verification: %w", err)
	}
	return nil
}

// RegenerateVerification atomically swaps the challenge of a pending verification
// to a freshly-composed one, gated on regenerated=0. Resets step/answers/message_id,
// bumps challenge_version, writes the new expires_at, and marks regenerated=1 — all
// in a single statement so an in-flight sweeper or answer-callback cannot interleave.
// Returns true if the row was successfully transitioned (caller may now send the new
// message); false means the row was missing, already regenerated, or otherwise gone.
func (db *DB) RegenerateVerification(chatID, userID int64, newLabels []string, newExpiresAt time.Time, options []string) (bool, error) {
	labelsJSON, err := json.Marshal(newLabels)
	if err != nil {
		return false, fmt.Errorf("failed to marshal new labels: %w", err)
	}
	optionsJSON, err := marshalOptions(options)
	if err != nil {
		return false, err
	}

	result, err := db.Exec(`
		UPDATE pending_verifications
		SET correct_labels = ?,
		    current_step = 0,
		    user_answers = '[]',
		    expires_at = ?,
		    message_id = NULL,
		    ephemeral_message_id = 0,
		    options = ?,
		    regenerated = 1,
		    challenge_version = challenge_version + 1
		WHERE chat_id = ? AND user_id = ? AND regenerated = 0
	`, string(labelsJSON), newExpiresAt.Unix(), optionsJSON, chatID, userID)
	if err != nil {
		return false, fmt.Errorf("failed to regenerate verification: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("failed to get rows affected: %w", err)
	}
	return rows > 0, nil
}

// ClaimPendingVerification atomically deletes the record only if it still matches
// the expected expires_at, preventing double-processing by concurrent goroutines.
// Returns true if the record was claimed (deleted), false if it was already gone or updated.
func (db *DB) ClaimPendingVerification(chatID, userID int64, expiresAt time.Time) (bool, error) {
	result, err := db.Exec(
		"DELETE FROM pending_verifications WHERE chat_id = ? AND user_id = ? AND expires_at = ?",
		chatID, userID, expiresAt.Unix(),
	)
	if err != nil {
		return false, fmt.Errorf("failed to claim pending verification: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("failed to get rows affected: %w", err)
	}
	return rows > 0, nil
}

func (db *DB) GetExpiredVerifications() ([]*PendingVerification, error) {
	// Use UNIX timestamp comparison to avoid timezone issues
	verifications, err := db.queryPendingVerifications("expires_at < ?", time.Now().Unix())
	if err != nil {
		return nil, fmt.Errorf("failed to query expired verifications: %w", err)
	}
	return verifications, nil
}

// GetUntouchedEphemeralChallenges returns the ephemeral challenges that are
// still running at now, expire at or before expiresBy, and whose joiner has
// not tapped anything: no answer recorded and no regenerate used. Telegram
// does not guarantee that an ephemeral message reaches its receiver, so a
// challenge nobody has touched may never have been seen.
//
// A challenge's age is read off its deadline, so expiresBy is "now + timeout
// - how long a challenge may sit untouched".
func (db *DB) GetUntouchedEphemeralChallenges(now, expiresBy time.Time) ([]*PendingVerification, error) {
	verifications, err := db.queryPendingVerifications(
		"ephemeral_message_id != 0 AND current_step = 0 AND regenerated = 0 AND expires_at > ? AND expires_at <= ?",
		now.Unix(), expiresBy.Unix(),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to query untouched ephemeral challenges: %w", err)
	}
	return verifications, nil
}

// ReplaceUntouchedEphemeralChallenge swaps an ephemeral challenge the joiner
// never touched for a regular one that has already been sent as messageID,
// with a fresh deadline. The regenerate chance is left unused.
//
// It is a compare-and-swap on the same conditions that made the challenge
// eligible. Returns false when they no longer hold — the joiner tapped the
// ephemeral message after all, or the row is gone — and the caller must then
// take the regular message down again.
func (db *DB) ReplaceUntouchedEphemeralChallenge(chatID, userID int64, expectedVersion int, newLabels []string, newExpiresAt time.Time, options []string, messageID int) (bool, error) {
	labelsJSON, err := json.Marshal(newLabels)
	if err != nil {
		return false, fmt.Errorf("failed to marshal new labels: %w", err)
	}
	optionsJSON, err := marshalOptions(options)
	if err != nil {
		return false, err
	}

	result, err := db.Exec(`
		UPDATE pending_verifications
		SET correct_labels = ?,
		    user_answers = '[]',
		    expires_at = ?,
		    message_id = ?,
		    ephemeral_message_id = 0,
		    admin_broadcast = 0,
		    options = ?,
		    challenge_version = challenge_version + 1
		WHERE chat_id = ? AND user_id = ? AND challenge_version = ?
		  AND ephemeral_message_id != 0 AND current_step = 0 AND regenerated = 0
	`, string(labelsJSON), newExpiresAt.Unix(), messageID, optionsJSON, chatID, userID, expectedVersion)
	if err != nil {
		return false, fmt.Errorf("failed to replace ephemeral challenge: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("failed to get rows affected: %w", err)
	}
	return rows > 0, nil
}

// queryPendingVerifications loads every pending verification matching the
// WHERE clause. Rows whose stored labels or answers don't parse are skipped
// rather than failing the whole batch.
func (db *DB) queryPendingVerifications(where string, args ...interface{}) ([]*PendingVerification, error) {
	rows, err := db.Query(`
		SELECT id, chat_id, user_id, message_id, ephemeral_message_id, admin_broadcast, options, join_message_id, correct_labels, current_step, user_answers, retry_count, expires_at, created_at, is_test, user_lang, regenerated, challenge_version
		FROM pending_verifications
		WHERE `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var verifications []*PendingVerification
	for rows.Next() {
		var pv PendingVerification
		var labelsJSON, answersJSON, optionsJSON string
		var expiresAtUnix int64
		var isTestInt, regeneratedInt, adminBroadcastInt int
		var userLang sql.NullString

		if err := rows.Scan(
			&pv.ID, &pv.ChatID, &pv.UserID, &pv.MessageID, &pv.EphemeralMessageID, &adminBroadcastInt, &optionsJSON, &pv.JoinMessageID,
			&labelsJSON, &pv.CurrentStep, &answersJSON,
			&pv.RetryCount, &expiresAtUnix, &pv.CreatedAt,
			&isTestInt, &userLang,
			&regeneratedInt, &pv.ChallengeVersion,
		); err != nil {
			return nil, fmt.Errorf("failed to scan verification: %w", err)
		}

		// Convert UNIX timestamp back to time.Time
		pv.ExpiresAt = time.Unix(expiresAtUnix, 0)
		pv.IsTest = isTestInt != 0
		pv.Regenerated = regeneratedInt != 0
		pv.AdminBroadcast = adminBroadcastInt != 0
		if userLang.Valid {
			pv.UserLang = userLang.String
		}

		if err := json.Unmarshal([]byte(labelsJSON), &pv.CorrectLabels); err != nil {
			continue
		}
		if err := json.Unmarshal([]byte(answersJSON), &pv.UserAnswers); err != nil {
			continue
		}
		pv.Options = decodeOptions(optionsJSON)

		verifications = append(verifications, &pv)
	}
	return verifications, rows.Err()
}

func (db *DB) RecordUserJoin(chatID, userID int64) error {
	_, err := db.Exec("INSERT INTO user_join_history (chat_id, user_id) VALUES (?, ?)", chatID, userID)
	if err != nil {
		return fmt.Errorf("failed to record user join: %w", err)
	}
	return nil
}

func (db *DB) GetRecentJoinCount(chatID, userID int64, withinSeconds int) (int, error) {
	var count int
	err := db.QueryRow(`
		SELECT COUNT(*) FROM user_join_history
		WHERE chat_id = ? AND user_id = ? AND joined_at > datetime('now', '-' || ? || ' seconds')
	`, chatID, userID, withinSeconds).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("failed to count recent joins: %w", err)
	}
	return count, nil
}

func (db *DB) CleanupOldJoinHistory(olderThanSeconds int) error {
	_, err := db.Exec(`
		DELETE FROM user_join_history
		WHERE joined_at < datetime('now', '-' || ? || ' seconds')
	`, olderThanSeconds)
	if err != nil {
		return fmt.Errorf("failed to cleanup join history: %w", err)
	}
	return nil
}

func (db *DB) DeleteUserJoinHistory(chatID, userID int64) error {
	_, err := db.Exec("DELETE FROM user_join_history WHERE chat_id = ? AND user_id = ?", chatID, userID)
	if err != nil {
		return fmt.Errorf("failed to delete user join history: %w", err)
	}
	return nil
}

func (db *DB) IsAdmin(userID int64) (bool, error) {
	var count int
	err := db.QueryRow("SELECT COUNT(*) FROM admins WHERE user_id = ?", userID).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("failed to check admin: %w", err)
	}
	return count > 0, nil
}

func (db *DB) AddAdmin(userID int64) error {
	_, err := db.Exec("INSERT OR IGNORE INTO admins (user_id) VALUES (?)", userID)
	if err != nil {
		return fmt.Errorf("failed to add admin: %w", err)
	}
	return nil
}

func (db *DB) RemoveAdmin(userID int64) error {
	_, err := db.Exec("DELETE FROM admins WHERE user_id = ?", userID)
	if err != nil {
		return fmt.Errorf("failed to remove admin: %w", err)
	}
	return nil
}

func (db *DB) GetAllAdmins() ([]int64, error) {
	rows, err := db.Query("SELECT user_id FROM admins ORDER BY added_at")
	if err != nil {
		return nil, fmt.Errorf("failed to query admins: %w", err)
	}
	defer rows.Close()

	var admins []int64
	for rows.Next() {
		var userID int64
		if err := rows.Scan(&userID); err != nil {
			return nil, fmt.Errorf("failed to scan admin: %w", err)
		}
		admins = append(admins, userID)
	}
	return admins, rows.Err()
}

// RecordVerificationFailure records a verification failure for a user
func (db *DB) RecordVerificationFailure(chatID, userID int64) error {
	_, err := db.Exec(`
		INSERT INTO verification_failures (chat_id, user_id)
		VALUES (?, ?)
		ON CONFLICT(chat_id, user_id) DO UPDATE SET failed_at = CURRENT_TIMESTAMP
	`, chatID, userID)
	if err != nil {
		return fmt.Errorf("failed to record verification failure: %w", err)
	}
	return nil
}

// HasPreviousFailure checks if a user has a previous verification failure
func (db *DB) HasPreviousFailure(chatID, userID int64) (bool, error) {
	var count int
	err := db.QueryRow(`
		SELECT COUNT(*) FROM verification_failures
		WHERE chat_id = ? AND user_id = ?
	`, chatID, userID).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("failed to check previous failure: %w", err)
	}
	return count > 0, nil
}

// ClearVerificationFailure removes a failure record (e.g., after successful verification)
func (db *DB) ClearVerificationFailure(chatID, userID int64) error {
	_, err := db.Exec("DELETE FROM verification_failures WHERE chat_id = ? AND user_id = ?", chatID, userID)
	if err != nil {
		return fmt.Errorf("failed to clear verification failure: %w", err)
	}
	return nil
}

// CleanupOldFailures removes failure records older than specified seconds
func (db *DB) CleanupOldFailures(olderThanSeconds int) error {
	_, err := db.Exec(`
		DELETE FROM verification_failures
		WHERE failed_at < datetime('now', '-' || ? || ' seconds')
	`, olderThanSeconds)
	if err != nil {
		return fmt.Errorf("failed to cleanup old failures: %w", err)
	}
	return nil
}
