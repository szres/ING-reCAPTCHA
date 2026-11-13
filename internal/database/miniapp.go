package database

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/mattn/go-sqlite3"
)

// MiniAppSession is a Mini App pre-join verification session keyed by an
// opaque nonce. See db.go for the table-level docs and status transitions.
type MiniAppSession struct {
	Nonce       string
	ChatID      int64
	UserID      int64
	QueryID     string
	Status      string
	ExpiresAt   int64
	CreatedAt   int64
	CompletedAt sql.NullInt64
}

// CreateMiniAppSession inserts a new session with the given initial status
// ('pending' for the standard Mini App flow, 'awaiting_dm' for the DM
// fallback flow). If an active session for (chat_id, user_id) — either
// 'pending' or 'awaiting_dm' — already exists, the existing row is
// returned untouched so the bot reuses it instead of starting two
// parallel flows.
//
// Returns (session, fresh) where fresh=true means a new row was inserted.
//
// Concurrency: the SELECT-then-INSERT inside a transaction is racy on its
// own — two goroutines can pass the SELECT before either INSERTs — so we
// also catch the SQLite unique-conflict from the partial index
// (idx_miniapp_pending_unique) and re-fetch the winning row. That makes
// the function idempotent under concurrent calls without requiring SERIALIZABLE.
func (db *DB) CreateMiniAppSession(nonce, queryID string, chatID, userID, expiresAt int64, initialStatus string) (*MiniAppSession, bool, error) {
	if initialStatus != "pending" && initialStatus != "awaiting_dm" {
		return nil, false, fmt.Errorf("invalid initialStatus %q", initialStatus)
	}

	// First, opportunistic read: handle the common case (no conflict) with
	// a single INSERT. If somebody else got there first we'll find them
	// via the conflict-recovery path below.
	if existing, found, err := db.lookupActiveSession(chatID, userID); err != nil {
		return nil, false, fmt.Errorf("lookup existing: %w", err)
	} else if found {
		return existing, false, nil
	}

	_, err := db.Exec(
		`INSERT INTO miniapp_sessions (nonce, chat_id, user_id, query_id, status, expires_at)
		      VALUES (?, ?, ?, ?, ?, ?)`,
		nonce, chatID, userID, queryID, initialStatus, expiresAt,
	)
	if err != nil {
		if isUniqueConstraintErr(err) {
			// Lost the race against a concurrent CreateMiniAppSession. The
			// winning row is now visible; return it as fresh=false so the
			// caller treats this as a duplicate delivery.
			if existing, found, fetchErr := db.lookupActiveSession(chatID, userID); fetchErr != nil {
				return nil, false, fmt.Errorf("conflict recovery: %w", fetchErr)
			} else if found {
				return existing, false, nil
			}
			// Conflict but no row visible (e.g. the winner expired between
			// the INSERT and our re-read). Surface as the original error so
			// the caller can decide.
			return nil, false, fmt.Errorf("insert conflict but no active row visible: %w", err)
		}
		return nil, false, fmt.Errorf("insert: %w", err)
	}

	return &MiniAppSession{
		Nonce:     nonce,
		ChatID:    chatID,
		UserID:    userID,
		QueryID:   queryID,
		Status:    initialStatus,
		ExpiresAt: expiresAt,
		CreatedAt: time.Now().Unix(),
	}, true, nil
}

// lookupActiveSession returns the active (pending or awaiting_dm) session
// for the given (chat_id, user_id) pair if one exists. found=false means
// the slot is free and a fresh INSERT can proceed.
func (db *DB) lookupActiveSession(chatID, userID int64) (*MiniAppSession, bool, error) {
	var s MiniAppSession
	err := db.QueryRow(
		`SELECT nonce, chat_id, user_id, query_id, status, expires_at, created_at, completed_at
		   FROM miniapp_sessions
		  WHERE chat_id = ? AND user_id = ?
		    AND status IN ('pending', 'awaiting_dm')
		    AND expires_at > ?
		  LIMIT 1`,
		chatID, userID, time.Now().Unix(),
	).Scan(&s.Nonce, &s.ChatID, &s.UserID, &s.QueryID,
		&s.Status, &s.ExpiresAt, &s.CreatedAt, &s.CompletedAt)
	switch {
	case err == nil:
		return &s, true, nil
	case errors.Is(err, sql.ErrNoRows):
		return nil, false, nil
	default:
		return nil, false, err
	}
}

// isUniqueConstraintErr reports whether the error came from a SQLite UNIQUE
// constraint violation. The go-sqlite3 driver exposes a typed error, but
// we also fall back to substring matching in case the driver is swapped.
func isUniqueConstraintErr(err error) bool {
	var sqliteErr sqlite3.Error
	if errors.As(err, &sqliteErr) {
		return sqliteErr.ExtendedCode == sqlite3.ErrConstraintUnique
	}
	return strings.Contains(strings.ToLower(err.Error()), "unique constraint")
}

// GetMiniAppSession looks up a session by nonce. Returns sql.ErrNoRows if
// the nonce is unknown.
func (db *DB) GetMiniAppSession(nonce string) (*MiniAppSession, error) {
	var s MiniAppSession
	err := db.QueryRow(
		`SELECT nonce, chat_id, user_id, query_id, status, expires_at, created_at, completed_at
		   FROM miniapp_sessions WHERE nonce = ?`,
		nonce,
	).Scan(&s.Nonce, &s.ChatID, &s.UserID, &s.QueryID, &s.Status,
		&s.ExpiresAt, &s.CreatedAt, &s.CompletedAt)
	if err != nil {
		return nil, err
	}
	return &s, nil
}

// MarkMiniAppSession transitions a session into a new status with an atomic
// UPDATE guarded by the current status, so two concurrent writers can never
// both succeed. Valid transitions:
//
//	awaiting_dm      → pending           (user /started bot, Mini App button sent)
//	awaiting_dm      → approve_required  → (caller dispatches to handler;
//	                                       not a stored status — see
//	                                       ClaimExpiredAwaitingDM)
//	awaiting_dm      → expired           (5-min timeout, fallback fires)
//	pending          → approving         (server reserved the session for approve)
//	pending          → declined          (decline path skips approving)
//	pending          → expired           (cleanup ticker)
//	approving        → approved_prejoin  (Telegram approve confirmed)
//	approving        → expired           (Telegram approve failed)
//
// We allow approved_prejoin from `pending OR approving` so tests that
// transition straight from pending stay valid; production paths go
// through approving.
//
// Returns ok=true iff the row was actually updated.
func (db *DB) MarkMiniAppSession(nonce, newStatus string) (bool, error) {
	var fromClause string
	switch newStatus {
	case "pending":
		// pending is only set as a transition from awaiting_dm (DM-flow
		// transitions from "waiting for /start" to "Mini App link sent").
		fromClause = "status = 'awaiting_dm'"
	case "approving":
		fromClause = "status = 'pending'"
	case "approved_prejoin":
		fromClause = "status IN ('pending', 'approving')"
	case "declined":
		fromClause = "status IN ('pending', 'awaiting_dm')"
	case "expired":
		fromClause = "status IN ('pending', 'awaiting_dm', 'approving')"
	default:
		return false, fmt.Errorf("invalid newStatus %q", newStatus)
	}
	res, err := db.Exec(
		`UPDATE miniapp_sessions
		    SET status = ?, completed_at = strftime('%s','now')
		  WHERE nonce = ? AND `+fromClause,
		newStatus, nonce,
	)
	if err != nil {
		return false, fmt.Errorf("update: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("rows affected: %w", err)
	}
	return n == 1, nil
}

// ClaimAwaitingDMByUser atomically transitions one awaiting_dm session for
// the given user into 'pending' AND extends its expires_at to newExpiresAt
// so the user has fresh solve time after /start-ing the bot. Used by the
// /start handler in DM.
//
// The UPDATE explicitly re-checks `expires_at > now` to close the race
// where a session expires between the SELECT and UPDATE; otherwise we'd
// promote a session the server is about to reject (Codex fix #6).
//
// If the user has multiple awaiting_dm sessions (multiple groups awaiting
// verification simultaneously), this claims the OLDEST one. The /start
// handler calls repeatedly until ok=false to drain them all.
//
// Returns (session, true) on success or (nil, false) when no eligible
// awaiting_dm session exists for this user.
func (db *DB) ClaimAwaitingDMByUser(userID, newExpiresAt int64) (*MiniAppSession, bool, error) {
	tx, err := db.Begin()
	if err != nil {
		return nil, false, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	var s MiniAppSession
	err = tx.QueryRow(
		`SELECT nonce, chat_id, user_id, query_id, status, expires_at, created_at, completed_at
		   FROM miniapp_sessions
		  WHERE user_id = ? AND status = 'awaiting_dm' AND expires_at > strftime('%s','now')
		  ORDER BY created_at ASC
		  LIMIT 1`,
		userID,
	).Scan(&s.Nonce, &s.ChatID, &s.UserID, &s.QueryID, &s.Status,
		&s.ExpiresAt, &s.CreatedAt, &s.CompletedAt)
	if err == sql.ErrNoRows {
		return nil, false, tx.Commit()
	}
	if err != nil {
		return nil, false, fmt.Errorf("select: %w", err)
	}

	res, err := tx.Exec(
		`UPDATE miniapp_sessions
		    SET status = 'pending', expires_at = ?
		  WHERE nonce = ? AND status = 'awaiting_dm'
		    AND expires_at > strftime('%s','now')`,
		newExpiresAt, s.Nonce,
	)
	if err != nil {
		return nil, false, fmt.Errorf("promote: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return nil, false, fmt.Errorf("rows affected: %w", err)
	}
	if n != 1 {
		// Lost a race with another claimer, OR the session expired between
		// SELECT and UPDATE. Either way the caller should treat this as
		// "no claim made" and either retry or stop.
		return nil, false, tx.Commit()
	}
	if err := tx.Commit(); err != nil {
		return nil, false, fmt.Errorf("commit: %w", err)
	}
	s.Status = "pending"
	s.ExpiresAt = newExpiresAt
	return &s, true, nil
}

// PromoteAwaitingToPending atomically transitions awaiting_dm → pending
// AND extends expires_at to newExpiresAt. Used by the proactive DM path
// at chat_join_request time: when sendMessage succeeds we promote so the
// Mini App server accepts incoming requests, and we extend the deadline
// so the user has a fresh solve window even if Telegram delivered the
// DM with most of the original 5-minute window already consumed.
//
// The UPDATE re-checks expires_at > now to close the race where the
// session may have expired between SELECT and UPDATE; otherwise we'd
// promote a session the server is about to reject.
//
// Returns ok=true iff a row was actually updated.
func (db *DB) PromoteAwaitingToPending(nonce string, newExpiresAt int64) (bool, error) {
	res, err := db.Exec(
		`UPDATE miniapp_sessions
		    SET status = 'pending', expires_at = ?
		  WHERE nonce = ? AND status = 'awaiting_dm' AND expires_at > strftime('%s','now')`,
		newExpiresAt, nonce,
	)
	if err != nil {
		return false, fmt.Errorf("promote: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("rows affected: %w", err)
	}
	return n == 1, nil
}

// RefreshSessionForRetry updates the query_id, extends expires_at, and
// (when newQueryID is non-empty) promotes status to 'pending'. Used when
// a redelivered chat_join_request supersedes a prior one for the same
// (chat,user) pair:
//
//   - newQueryID != "": Telegram gave us a fresh query_id. Drive status
//     to 'pending' (covers awaiting_dm → pending transitions when the
//     retry brings query_id Telegram didn't include the first time), so
//     the Mini App server accepts /v/init.
//   - newQueryID == "": Telegram dropped query_id on retry. Clear any
//     stale stored query_id (it can never succeed — the 10s window is
//     long gone) so finishAndApprove falls through to the legacy
//     approveChatJoinRequest path. Status is left untouched; the caller
//     drives delivery via DM.
//
// Returns ok=true iff a row was actually updated. The WHERE clause
// re-checks expires_at > now so we don't extend an already-expired
// session by accident.
func (db *DB) RefreshSessionForRetry(nonce, newQueryID string, newExpiresAt int64) (bool, error) {
	var (
		res sql.Result
		err error
	)
	if newQueryID != "" {
		res, err = db.Exec(
			`UPDATE miniapp_sessions
			    SET query_id = ?, expires_at = ?, status = 'pending'
			  WHERE nonce = ? AND status IN ('pending', 'awaiting_dm') AND expires_at > strftime('%s','now')`,
			newQueryID, newExpiresAt, nonce,
		)
	} else {
		res, err = db.Exec(
			`UPDATE miniapp_sessions
			    SET query_id = '', expires_at = ?
			  WHERE nonce = ? AND status IN ('pending', 'awaiting_dm') AND expires_at > strftime('%s','now')`,
			newExpiresAt, nonce,
		)
	}
	if err != nil {
		return false, fmt.Errorf("refresh: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("rows affected: %w", err)
	}
	return n == 1, nil
}

// CountAwaitingDMByUser returns the number of awaiting_dm sessions still
// eligible for delivery for the given user. Used to surface overflow when
// deliverAwaitingDMSessions hits its per-call cap (Codex fix #4).
func (db *DB) CountAwaitingDMByUser(userID int64) (int, error) {
	var n int
	err := db.QueryRow(
		`SELECT COUNT(*) FROM miniapp_sessions
		  WHERE user_id = ? AND status = 'awaiting_dm'
		    AND expires_at > strftime('%s','now')`,
		userID,
	).Scan(&n)
	return n, err
}

// ListExpiredForPolicyFallback returns sessions whose TTL has passed AND
// whose join request can ONLY be acted on by the bot (i.e. there is no
// Telegram-native approve path remaining). Two such categories:
//
//   - status = 'awaiting_dm': we never delivered the Mini App button —
//     either because user_chat_id was absent or the DM send failed.
//     Nothing in Telegram's UI can resolve the join request; the bot
//     must apply the chat's fallback policy.
//
//   - status = 'pending' AND query_id = '': the DM-fallback path
//     successfully delivered the button (promoting awaiting_dm →
//     pending). The user never solved in time. Since the original
//     request came without query_id, Telegram never showed a native
//     Mini App popup; the bot is again the only actor that can clear
//     the request.
//
// Modern-flow timeouts (status = 'pending' AND query_id != '') are
// intentionally NOT returned here. Those sessions had a Telegram-native
// popup shown to the user; if the user didn't engage, expiring silently
// matches the legacy verification behaviour where unverified joiners
// stay in the queue for a human admin.
//
// The caller is expected to apply each chat's join policy and then
// transition the session via MarkMiniAppSession(nonce, "expired") so it
// isn't returned again.
func (db *DB) ListExpiredForPolicyFallback() ([]*MiniAppSession, error) {
	rows, err := db.Query(
		`SELECT nonce, chat_id, user_id, query_id, status, expires_at, created_at, completed_at
		   FROM miniapp_sessions
		  WHERE expires_at <= strftime('%s','now')
		    AND (status = 'awaiting_dm'
		         OR (status = 'pending' AND query_id = ''))`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*MiniAppSession
	for rows.Next() {
		var s MiniAppSession
		if err := rows.Scan(&s.Nonce, &s.ChatID, &s.UserID, &s.QueryID,
			&s.Status, &s.ExpiresAt, &s.CreatedAt, &s.CompletedAt); err != nil {
			return nil, err
		}
		out = append(out, &s)
	}
	return out, rows.Err()
}

// HasRecentMiniAppApproval reports whether the (chat_id, user_id) pair has
// a Mini App session that's either already approved or in the middle of
// being approved, within `withinSeconds`. The chat_member handler uses
// this to skip the in-group captcha after the Mini App admits the user.
//
// Why both 'approving' and 'approved_prejoin': there's a tight window
// between answerChatJoinRequestQuery returning success (which causes
// Telegram to fire chat_member 'left → member') and our subsequent DB
// mark to approved_prejoin. By stamping 'approving' BEFORE the Telegram
// call, the guard catches the chat_member transition even when it lands
// in the gap.
func (db *DB) HasRecentMiniAppApproval(chatID, userID int64, withinSeconds int) (bool, error) {
	cutoff := time.Now().Unix() - int64(withinSeconds)
	var n int
	err := db.QueryRow(
		`SELECT COUNT(*) FROM miniapp_sessions
		  WHERE chat_id = ? AND user_id = ?
		    AND status IN ('approving', 'approved_prejoin')
		    AND completed_at IS NOT NULL
		    AND completed_at >= ?`,
		chatID, userID, cutoff,
	).Scan(&n)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// ExpireStaleMiniAppSessions silently transitions modern-flow pending
// sessions whose expires_at has passed into 'expired'. "Modern-flow" is
// identified by a non-empty query_id (the session originated from a
// chat_join_request that Telegram enriched with query_id, so the user
// saw a native Mini App popup). For these, the request stays in
// Telegram's pending queue for a human admin to handle — same as legacy
// verification.
//
// Sessions for which the BOT is the only actor that can resolve the
// join request — awaiting_dm and pending-with-empty-query_id — are
// expired by ListExpiredForPolicyFallback + applyJoinPolicyFallback
// instead, so the chat's configured policy can fire BEFORE we mark
// expired.
//
// Runs from a background ticker so abandoned rows don't pile up and so
// the unique partial index for active sessions eventually frees up for
// new join attempts.
func (db *DB) ExpireStaleMiniAppSessions() (int64, error) {
	res, err := db.Exec(
		`UPDATE miniapp_sessions
		    SET status = 'expired', completed_at = strftime('%s','now')
		  WHERE status = 'pending'
		    AND query_id != ''
		    AND expires_at <= strftime('%s','now')`,
	)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// CleanupOldMiniAppSessions deletes rows older than `keepSeconds`, regardless
// of status. Run from the same background ticker as the other history
// cleanups so the table stays small.
func (db *DB) CleanupOldMiniAppSessions(keepSeconds int) error {
	cutoff := time.Now().Unix() - int64(keepSeconds)
	_, err := db.Exec(`DELETE FROM miniapp_sessions WHERE created_at < ?`, cutoff)
	return err
}
