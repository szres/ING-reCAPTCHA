-- Per-group auto-delete of the "passed verification" notice.
-- Mirrors the embedded migration in internal/database/db.go (which also
-- creates chat_settings itself).

ALTER TABLE chat_settings ADD COLUMN pass_notice_ttl_seconds INTEGER NOT NULL DEFAULT 0;

CREATE TABLE IF NOT EXISTS scheduled_message_deletions (
    chat_id INTEGER NOT NULL,
    message_id INTEGER NOT NULL,
    delete_at INTEGER NOT NULL,
    PRIMARY KEY (chat_id, message_id)
);
CREATE INDEX IF NOT EXISTS idx_scheduled_deletions_due
    ON scheduled_message_deletions(delete_at);
