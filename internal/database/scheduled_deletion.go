package database

import (
	"fmt"
	"time"
)

// ScheduledDeletion is a message the bot has committed to delete at DeleteAt.
type ScheduledDeletion struct {
	ChatID    int64
	MessageID int
	DeleteAt  time.Time
}

// ScheduleMessageDeletion records that messageID in chatID is to be deleted
// at deleteAt. Scheduling the same message again moves its deadline.
func (db *DB) ScheduleMessageDeletion(chatID int64, messageID int, deleteAt time.Time) error {
	_, err := db.Exec(
		`INSERT INTO scheduled_message_deletions (chat_id, message_id, delete_at)
		      VALUES (?, ?, ?)
		 ON CONFLICT(chat_id, message_id) DO UPDATE SET delete_at = excluded.delete_at`,
		chatID, messageID, deleteAt.Unix(),
	)
	if err != nil {
		return fmt.Errorf("failed to schedule message deletion: %w", err)
	}
	return nil
}

// GetDueMessageDeletions returns up to limit deletions whose deadline has
// passed at now, oldest first.
func (db *DB) GetDueMessageDeletions(now time.Time, limit int) ([]ScheduledDeletion, error) {
	rows, err := db.Query(
		`SELECT chat_id, message_id, delete_at
		   FROM scheduled_message_deletions
		  WHERE delete_at <= ?
		  ORDER BY delete_at
		  LIMIT ?`,
		now.Unix(), limit,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to query due message deletions: %w", err)
	}
	defer rows.Close()

	var due []ScheduledDeletion
	for rows.Next() {
		var d ScheduledDeletion
		var deleteAtUnix int64
		if err := rows.Scan(&d.ChatID, &d.MessageID, &deleteAtUnix); err != nil {
			return nil, fmt.Errorf("failed to scan message deletion: %w", err)
		}
		d.DeleteAt = time.Unix(deleteAtUnix, 0)
		due = append(due, d)
	}
	return due, rows.Err()
}

// RemoveScheduledDeletion forgets a scheduled deletion, once the message is
// gone or the bot has given up on it.
func (db *DB) RemoveScheduledDeletion(chatID int64, messageID int) error {
	_, err := db.Exec(
		"DELETE FROM scheduled_message_deletions WHERE chat_id = ? AND message_id = ?",
		chatID, messageID,
	)
	if err != nil {
		return fmt.Errorf("failed to remove scheduled deletion: %w", err)
	}
	return nil
}
