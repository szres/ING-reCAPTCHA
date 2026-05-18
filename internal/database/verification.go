package database

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

type PendingVerification struct {
	ID               int64
	ChatID           int64
	UserID           int64
	MessageID        sql.NullInt64
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

func (db *DB) CreatePendingVerification(chatID, userID int64, correctLabels []string, expiresAt time.Time, isTest bool, userLang string) error {
	labelsJSON, err := json.Marshal(correctLabels)
	if err != nil {
		return fmt.Errorf("failed to marshal correct labels: %w", err)
	}

	// Store expires_at as UNIX timestamp (integer) to avoid timezone issues
	expiresAtUnix := expiresAt.Unix()
	isTestInt := 0
	if isTest {
		isTestInt = 1
	}

	_, err = db.Exec(`
		INSERT INTO pending_verifications (chat_id, user_id, correct_labels, expires_at, is_test, user_lang)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(chat_id, user_id) DO UPDATE SET
			correct_labels = excluded.correct_labels,
			current_step = 0,
			user_answers = '[]',
			retry_count = pending_verifications.retry_count,
			expires_at = excluded.expires_at,
			message_id = NULL,
			is_test = excluded.is_test,
			user_lang = excluded.user_lang,
			regenerated = 0,
			challenge_version = 0
	`, chatID, userID, string(labelsJSON), expiresAtUnix, isTestInt, userLang)
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
	var labelsJSON, answersJSON string
	var expiresAtUnix int64
	var isTestInt, regeneratedInt int
	var userLang sql.NullString

	err := db.QueryRow(`
		SELECT id, chat_id, user_id, message_id, join_message_id, correct_labels, current_step, user_answers, retry_count, expires_at, created_at, is_test, user_lang, regenerated, challenge_version
		FROM pending_verifications
		WHERE chat_id = ? AND user_id = ?
	`, chatID, userID).Scan(
		&pv.ID, &pv.ChatID, &pv.UserID, &pv.MessageID, &pv.JoinMessageID,
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
	if userLang.Valid {
		pv.UserLang = userLang.String
	}

	if err := json.Unmarshal([]byte(labelsJSON), &pv.CorrectLabels); err != nil {
		return nil, fmt.Errorf("failed to unmarshal correct labels: %w", err)
	}
	if err := json.Unmarshal([]byte(answersJSON), &pv.UserAnswers); err != nil {
		return nil, fmt.Errorf("failed to unmarshal user answers: %w", err)
	}

	return &pv, nil
}

// UpdateVerificationStep advances the step and answers atomically, gated on the
// expected challenge_version. Returns true if the update applied; false means the
// challenge was regenerated under the caller's feet — the caller must discard the
// in-flight answer and respond with an "expired" toast.
func (db *DB) UpdateVerificationStep(chatID, userID int64, step int, answers []string, expectedVersion int) (bool, error) {
	answersJSON, err := json.Marshal(answers)
	if err != nil {
		return false, fmt.Errorf("failed to marshal answers: %w", err)
	}

	result, err := db.Exec(
		"UPDATE pending_verifications SET current_step = ?, user_answers = ? WHERE chat_id = ? AND user_id = ? AND challenge_version = ?",
		step, string(answersJSON), chatID, userID, expectedVersion,
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
func (db *DB) RegenerateVerification(chatID, userID int64, newLabels []string, newExpiresAt time.Time) (bool, error) {
	labelsJSON, err := json.Marshal(newLabels)
	if err != nil {
		return false, fmt.Errorf("failed to marshal new labels: %w", err)
	}

	result, err := db.Exec(`
		UPDATE pending_verifications
		SET correct_labels = ?,
		    current_step = 0,
		    user_answers = '[]',
		    expires_at = ?,
		    message_id = NULL,
		    regenerated = 1,
		    challenge_version = challenge_version + 1
		WHERE chat_id = ? AND user_id = ? AND regenerated = 0
	`, string(labelsJSON), newExpiresAt.Unix(), chatID, userID)
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
	nowUnix := time.Now().Unix()

	rows, err := db.Query(`
		SELECT id, chat_id, user_id, message_id, join_message_id, correct_labels, current_step, user_answers, retry_count, expires_at, created_at, is_test, user_lang, regenerated, challenge_version
		FROM pending_verifications
		WHERE expires_at < ?
	`, nowUnix)
	if err != nil {
		return nil, fmt.Errorf("failed to query expired verifications: %w", err)
	}
	defer rows.Close()

	var verifications []*PendingVerification
	for rows.Next() {
		var pv PendingVerification
		var labelsJSON, answersJSON string
		var expiresAtUnix int64
		var isTestInt, regeneratedInt int
		var userLang sql.NullString

		if err := rows.Scan(
			&pv.ID, &pv.ChatID, &pv.UserID, &pv.MessageID, &pv.JoinMessageID,
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
		if userLang.Valid {
			pv.UserLang = userLang.String
		}

		if err := json.Unmarshal([]byte(labelsJSON), &pv.CorrectLabels); err != nil {
			continue
		}
		if err := json.Unmarshal([]byte(answersJSON), &pv.UserAnswers); err != nil {
			continue
		}

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
