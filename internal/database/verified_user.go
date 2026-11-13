package database

import (
	"fmt"
	"log"
	"time"
)

// How a user earned their place on the pass list. Stored in
// verified_users.source so an operator auditing the table can tell which
// flow admitted them. Rows an operator adds by hand use their own value
// (the bulk group import writes "import").
const (
	VerifiedSourceCaptcha = "captcha"
	VerifiedSourceMiniApp = "miniapp"
)

// MarkUserVerified puts a user on the global pass list: they proved they are
// human in chatID and skip the captcha in every other chat the bot guards.
// The first record wins, so the row keeps where and how the user originally
// qualified.
func (db *DB) MarkUserVerified(userID, chatID int64, source string) error {
	_, err := db.Exec(
		"INSERT OR IGNORE INTO verified_users (user_id, chat_id, source, verified_at) VALUES (?, ?, ?, ?)",
		userID, chatID, source, time.Now().Unix(),
	)
	if err != nil {
		return fmt.Errorf("failed to mark user verified: %w", err)
	}
	return nil
}

func (db *DB) IsUserVerified(userID int64) (bool, error) {
	var count int
	err := db.QueryRow("SELECT COUNT(*) FROM verified_users WHERE user_id = ?", userID).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("failed to check verified user: %w", err)
	}
	return count > 0, nil
}

// seedVerifiedUsers fills a newly created pass list from the passes already
// on record, so users who solved a captcha before the list existed are not
// asked again. Admin approvals and test runs are left out: neither is the
// user solving a captcha as a joiner.
func (db *DB) seedVerifiedUsers() error {
	result, err := db.Exec(`
		INSERT OR IGNORE INTO verified_users (user_id, chat_id, source, verified_at)
		SELECT user_id, chat_id, ?, MIN(occurred_at)
		FROM verification_events
		WHERE outcome = 'passed' AND admin_user_id IS NULL AND is_test = 0
		GROUP BY user_id
	`, VerifiedSourceCaptcha)
	if err != nil {
		return fmt.Errorf("failed to seed verified users: %w", err)
	}
	if n, err := result.RowsAffected(); err == nil && n > 0 {
		log.Printf("[INFO] seeded %d verified user(s) from past verification passes", n)
	}
	return nil
}
