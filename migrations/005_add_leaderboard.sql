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
