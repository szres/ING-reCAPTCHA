package database

import (
	"database/sql"
	"fmt"
	"math"
)

// VerificationEvent is a single terminal outcome record.
type VerificationEvent struct {
	ChatID           int64
	UserID           int64
	Outcome          string // "passed" | "failed" | "expired"
	CompletionTimeMs sql.NullInt64
	FailedAtStep     sql.NullInt64
	IsTest           bool
	UserLang         string
	Regenerated      bool
	AdminUserID      sql.NullInt64 // non-NULL when a chat admin overrode the outcome via the moderation buttons
	OccurredAt       int64         // unix seconds
}

func (db *DB) RecordVerificationEvent(e VerificationEvent) error {
	isTestInt := 0
	if e.IsTest {
		isTestInt = 1
	}
	regeneratedInt := 0
	if e.Regenerated {
		regeneratedInt = 1
	}
	_, err := db.Exec(`
		INSERT INTO verification_events
			(chat_id, user_id, outcome, completion_time_ms, failed_at_step, is_test, user_lang, regenerated, admin_user_id, occurred_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, e.ChatID, e.UserID, e.Outcome, e.CompletionTimeMs, e.FailedAtStep, isTestInt, e.UserLang, regeneratedInt, e.AdminUserID, e.OccurredAt)
	if err != nil {
		return fmt.Errorf("failed to record verification event: %w", err)
	}
	return nil
}

// DailyStats is the aggregated production verification stats for a window.
// Test events (is_test=1) are always excluded.
type DailyStats struct {
	Total         int
	Passed        int
	Failed        int
	Expired       int
	PassRate      float64 // 0..1
	AvgPassMs     int64
	MedianPassMs  int64
	P95PassMs     int64
	FastestPassMs int64
	UniqueUsers   int
	FailStepHist  map[int]int    // step number (1-indexed) -> count
	LangSplit     map[string]int // user_lang -> total event count
}

// GetDailyStats returns stats for the half-open window [startUnix, endUnix).
func (db *DB) GetDailyStats(startUnix, endUnix int64) (*DailyStats, error) {
	s := &DailyStats{
		FailStepHist: make(map[int]int),
		LangSplit:    make(map[string]int),
	}

	rows, err := db.Query(`
		SELECT outcome, COUNT(*), COALESCE(NULLIF(user_lang, ''), 'unknown')
		FROM verification_events
		WHERE is_test = 0 AND occurred_at >= ? AND occurred_at < ?
		GROUP BY outcome, user_lang
	`, startUnix, endUnix)
	if err != nil {
		return nil, fmt.Errorf("failed to query outcomes: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var outcome, lang string
		var count int
		if err := rows.Scan(&outcome, &count, &lang); err != nil {
			return nil, fmt.Errorf("failed to scan outcome: %w", err)
		}
		switch outcome {
		case "passed":
			s.Passed += count
		case "failed":
			s.Failed += count
		case "expired":
			s.Expired += count
		}
		s.LangSplit[lang] += count
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	s.Total = s.Passed + s.Failed + s.Expired
	if s.Total > 0 {
		s.PassRate = float64(s.Passed) / float64(s.Total)
	}

	if err := db.QueryRow(`
		SELECT COUNT(DISTINCT user_id) FROM verification_events
		WHERE is_test = 0 AND occurred_at >= ? AND occurred_at < ?
	`, startUnix, endUnix).Scan(&s.UniqueUsers); err != nil {
		return nil, fmt.Errorf("failed to query unique users: %w", err)
	}

	if s.Passed > 0 {
		var avg sql.NullFloat64
		var fastest sql.NullInt64
		if err := db.QueryRow(`
			SELECT AVG(completion_time_ms), MIN(completion_time_ms)
			FROM verification_events
			WHERE is_test = 0 AND outcome = 'passed' AND occurred_at >= ? AND occurred_at < ?
			  AND completion_time_ms IS NOT NULL
		`, startUnix, endUnix).Scan(&avg, &fastest); err != nil {
			return nil, fmt.Errorf("failed to query pass-time stats: %w", err)
		}
		if avg.Valid {
			s.AvgPassMs = int64(math.Round(avg.Float64))
		}
		if fastest.Valid {
			s.FastestPassMs = fastest.Int64
		}

		// SQLite has no built-in percentile aggregate; fetch sorted list and index.
		timesRows, err := db.Query(`
			SELECT completion_time_ms FROM verification_events
			WHERE is_test = 0 AND outcome = 'passed' AND occurred_at >= ? AND occurred_at < ?
			  AND completion_time_ms IS NOT NULL
			ORDER BY completion_time_ms
		`, startUnix, endUnix)
		if err != nil {
			return nil, fmt.Errorf("failed to query pass-time list: %w", err)
		}
		defer timesRows.Close()
		var times []int64
		for timesRows.Next() {
			var ms int64
			if err := timesRows.Scan(&ms); err != nil {
				return nil, fmt.Errorf("failed to scan pass-time: %w", err)
			}
			times = append(times, ms)
		}
		if err := timesRows.Err(); err != nil {
			return nil, err
		}
		s.MedianPassMs = percentileMs(times, 0.50)
		s.P95PassMs = percentileMs(times, 0.95)
	}

	histRows, err := db.Query(`
		SELECT failed_at_step, COUNT(*) FROM verification_events
		WHERE is_test = 0 AND outcome = 'failed' AND occurred_at >= ? AND occurred_at < ?
		  AND failed_at_step IS NOT NULL
		GROUP BY failed_at_step
		ORDER BY failed_at_step
	`, startUnix, endUnix)
	if err != nil {
		return nil, fmt.Errorf("failed to query fail-step histogram: %w", err)
	}
	defer histRows.Close()
	for histRows.Next() {
		var step, count int
		if err := histRows.Scan(&step, &count); err != nil {
			return nil, fmt.Errorf("failed to scan fail-step: %w", err)
		}
		s.FailStepHist[step] = count
	}
	if err := histRows.Err(); err != nil {
		return nil, err
	}

	return s, nil
}

func percentileMs(sorted []int64, p float64) int64 {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	idx := int(math.Round(p * float64(n-1)))
	if idx < 0 {
		idx = 0
	}
	if idx >= n {
		idx = n - 1
	}
	return sorted[idx]
}
