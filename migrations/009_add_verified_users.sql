-- Global pass list: a user who solved a captcha themselves in any chat skips
-- verification in every other chat the bot guards. Admin approvals never add
-- a row here.
-- Mirrors the embedded migration in internal/database/db.go.

CREATE TABLE IF NOT EXISTS verified_users (
    user_id INTEGER PRIMARY KEY,
    chat_id INTEGER NOT NULL,
    source TEXT NOT NULL,
    verified_at INTEGER NOT NULL
);

-- Users who already passed before the list existed are not asked again.
INSERT OR IGNORE INTO verified_users (user_id, chat_id, source, verified_at)
SELECT user_id, chat_id, 'captcha', MIN(occurred_at)
FROM verification_events
WHERE outcome = 'passed' AND admin_user_id IS NULL AND is_test = 0
GROUP BY user_id;
