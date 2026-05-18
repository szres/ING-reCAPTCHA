package database

import (
	"database/sql"
	"fmt"
	"time"
)

// LeaderboardEntry represents a single leaderboard entry
type LeaderboardEntry struct {
	Rank             int
	UserID           int64
	Username         string
	FirstName        string
	LastName         string
	CompletionTimeMs int64
	CompletedAt      time.Time
}

// RecordLeaderboardEntry records or updates a user's test completion time
func (db *DB) RecordLeaderboardEntry(userID int64, username, firstName, lastName string, completionTime time.Duration) error {
	_, err := db.Exec(`
		INSERT INTO leaderboard (user_id, username, first_name, last_name, completion_time_ms, completed_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(user_id) DO UPDATE SET
			username = excluded.username,
			first_name = excluded.first_name,
			last_name = excluded.last_name,
			completion_time_ms = CASE
				WHEN excluded.completion_time_ms < leaderboard.completion_time_ms THEN excluded.completion_time_ms
				ELSE leaderboard.completion_time_ms
			END,
			completed_at = excluded.completed_at
	`, userID, username, firstName, lastName, completionTime.Milliseconds(), time.Now().Unix())
	
	return err
}

// GetTopLeaderboard returns the top N entries from the leaderboard
func (db *DB) GetTopLeaderboard(limit int) ([]LeaderboardEntry, error) {
	rows, err := db.Query(`
		SELECT user_id, username, first_name, last_name, completion_time_ms, completed_at
		FROM leaderboard
		ORDER BY completion_time_ms ASC
		LIMIT ?
	`, limit)
	if err != nil {
		return nil, fmt.Errorf("failed to query leaderboard: %w", err)
	}
	defer rows.Close()

	var entries []LeaderboardEntry
	rank := 1
	for rows.Next() {
		var entry LeaderboardEntry
		var completedAtUnix int64
		var lastName sql.NullString

		err := rows.Scan(
			&entry.UserID,
			&entry.Username,
			&entry.FirstName,
			&lastName,
			&entry.CompletionTimeMs,
			&completedAtUnix,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to scan leaderboard entry: %w", err)
		}

		entry.Rank = rank
		entry.CompletedAt = time.Unix(completedAtUnix, 0)
		if lastName.Valid {
			entry.LastName = lastName.String
		}

		entries = append(entries, entry)
		rank++
	}

	return entries, rows.Err()
}

// GetUserRank returns a user's rank in the leaderboard (0 if not found)
func (db *DB) GetUserRank(userID int64) (int, error) {
	var rank int
	err := db.QueryRow(`
		SELECT COUNT(*) + 1
		FROM leaderboard
		WHERE completion_time_ms < (
			SELECT completion_time_ms FROM leaderboard WHERE user_id = ?
		)
	`, userID).Scan(&rank)
	
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	
	return rank, nil
}

// GetUserPercentile returns what percentage of users this user beats (0-100)
// This is based on the user's best time stored in the database
func (db *DB) GetUserPercentile(userID int64) (float64, error) {
	var totalUsers int
	err := db.QueryRow(`SELECT COUNT(*) FROM leaderboard`).Scan(&totalUsers)
	if err != nil {
		return 0, fmt.Errorf("failed to get total users: %w", err)
	}
	
	if totalUsers == 0 {
		return 0, nil
	}
	
	rank, err := db.GetUserRank(userID)
	if err != nil {
		return 0, fmt.Errorf("failed to get user rank: %w", err)
	}
	
	if rank == 0 {
		return 0, nil
	}
	
	// Calculate percentile: (total - rank) / total * 100
	// If rank is 1 out of 10, beats 9 users = 90%
	// If rank is 10 out of 10, beats 0 users = 0%
	percentile := float64(totalUsers-rank) / float64(totalUsers) * 100
	return percentile, nil
}

// GetTimePercentile returns what percentage of users a given time beats (0-100)
// This calculates the percentile for a specific completion time without requiring it to be in the database
func (db *DB) GetTimePercentile(completionTime time.Duration) (float64, error) {
	var totalUsers int
	err := db.QueryRow(`SELECT COUNT(*) FROM leaderboard`).Scan(&totalUsers)
	if err != nil {
		return 0, fmt.Errorf("failed to get total users: %w", err)
	}
	
	if totalUsers == 0 {
		return 0, nil
	}
	
	// Count how many users this time would beat (users with slower times)
	var usersBeat int
	err = db.QueryRow(`
		SELECT COUNT(*)
		FROM leaderboard
		WHERE completion_time_ms > ?
	`, completionTime.Milliseconds()).Scan(&usersBeat)
	if err != nil {
		return 0, fmt.Errorf("failed to count users beat: %w", err)
	}
	
	// Calculate percentile: usersBeat / total * 100
	percentile := float64(usersBeat) / float64(totalUsers) * 100
	return percentile, nil
}
