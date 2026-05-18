-- Regenerate-questions-once feature: track whether a session has used its
-- one-time regenerate, and a monotonic challenge version so stale answer
-- callbacks can be discarded after regeneration.
-- Mirrors the embedded migration in internal/database/db.go.

ALTER TABLE pending_verifications ADD COLUMN regenerated INTEGER NOT NULL DEFAULT 0;
ALTER TABLE pending_verifications ADD COLUMN challenge_version INTEGER NOT NULL DEFAULT 0;

-- Stats: flag events that came from a regenerated session for future analysis.
ALTER TABLE verification_events ADD COLUMN regenerated INTEGER NOT NULL DEFAULT 0;
