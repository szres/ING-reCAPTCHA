-- Daily verification statistics: one row per terminal outcome.
-- Mirrors the embedded migration in internal/database/db.go.

CREATE TABLE IF NOT EXISTS verification_events (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    chat_id INTEGER NOT NULL,
    user_id INTEGER NOT NULL,
    outcome TEXT NOT NULL CHECK (outcome IN ('passed', 'failed', 'expired')),
    completion_time_ms INTEGER,
    failed_at_step INTEGER,
    is_test INTEGER NOT NULL DEFAULT 0,
    user_lang TEXT,
    occurred_at INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_events_test_occurred ON verification_events(is_test, occurred_at);

ALTER TABLE pending_verifications ADD COLUMN is_test INTEGER NOT NULL DEFAULT 0;
ALTER TABLE pending_verifications ADD COLUMN user_lang TEXT;
