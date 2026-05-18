-- Chat-admin moderation buttons: record which admin (if any) overrode a
-- verification outcome. NULL = ordinary user-driven outcome; non-NULL = approve
-- or ban triggered by a chat admin clicking the inline buttons.
-- Mirrors the embedded migration in internal/database/db.go.

ALTER TABLE verification_events ADD COLUMN admin_user_id INTEGER;
