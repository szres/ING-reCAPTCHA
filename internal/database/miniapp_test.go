package database

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMiniAppSessions_CreateAndGet(t *testing.T) {
	db := newTestDB(t)
	expires := time.Now().Add(5 * time.Minute).Unix()

	sess, fresh, err := db.CreateMiniAppSession("nonce-1", "Q1", 100, 200, expires, "pending")
	require.NoError(t, err)
	require.True(t, fresh)
	assert.Equal(t, "nonce-1", sess.Nonce)
	assert.Equal(t, "Q1", sess.QueryID)
	assert.Equal(t, "pending", sess.Status)

	got, err := db.GetMiniAppSession("nonce-1")
	require.NoError(t, err)
	assert.Equal(t, int64(100), got.ChatID)
	assert.Equal(t, int64(200), got.UserID)
}

func TestMiniAppSessions_IdempotentOnDuplicatePending(t *testing.T) {
	db := newTestDB(t)
	expires := time.Now().Add(5 * time.Minute).Unix()

	first, fresh, err := db.CreateMiniAppSession("nonce-A", "Q1", 100, 200, expires, "pending")
	require.NoError(t, err)
	require.True(t, fresh)

	// Same chat+user, different nonce — should NOT insert a new pending row;
	// instead, return the first session.
	second, fresh2, err := db.CreateMiniAppSession("nonce-B", "Q2", 100, 200, expires, "pending")
	require.NoError(t, err)
	require.False(t, fresh2, "second call must NOT insert a new pending row")
	assert.Equal(t, first.Nonce, second.Nonce, "should return the original pending session")
}

func TestMiniAppSessions_FreshAllowedAfterTerminalState(t *testing.T) {
	db := newTestDB(t)
	expires := time.Now().Add(5 * time.Minute).Unix()

	_, _, err := db.CreateMiniAppSession("nonce-old", "Q1", 100, 200, expires, "pending")
	require.NoError(t, err)

	// Mark the first session as declined.
	ok, err := db.MarkMiniAppSession("nonce-old", "declined")
	require.NoError(t, err)
	require.True(t, ok)

	// New chat_join_request from same user — fresh session must be allowed
	// now that the first is in a terminal state.
	sess, fresh, err := db.CreateMiniAppSession("nonce-new", "Q2", 100, 200, expires, "pending")
	require.NoError(t, err)
	assert.True(t, fresh, "must insert a new pending row after the prior one was terminal")
	assert.Equal(t, "nonce-new", sess.Nonce)
}

// TestMiniAppSessions_TransientApproving verifies the pending → approving →
// approved_prejoin path and that HasRecentMiniAppApproval matches BOTH
// transient (approving) and terminal (approved_prejoin) states. This is
// the race fix: the guard must catch chat_member updates that arrive in
// the gap between Telegram's approve confirmation and our final DB mark.
func TestMiniAppSessions_TransientApproving(t *testing.T) {
	db := newTestDB(t)
	expires := time.Now().Add(5 * time.Minute).Unix()
	_, _, err := db.CreateMiniAppSession("n", "Q", 10, 20, expires, "pending")
	require.NoError(t, err)

	// pending → approving
	ok, err := db.MarkMiniAppSession("n", "approving")
	require.NoError(t, err)
	assert.True(t, ok)

	// Race-guard query during the transient window MUST hit.
	has, err := db.HasRecentMiniAppApproval(10, 20, 300)
	require.NoError(t, err)
	assert.True(t, has, "approving should count as 'recent approval' for the race guard")

	// approving → approved_prejoin
	ok, err = db.MarkMiniAppSession("n", "approved_prejoin")
	require.NoError(t, err)
	assert.True(t, ok)

	// Still matches after terminal transition.
	has, err = db.HasRecentMiniAppApproval(10, 20, 300)
	require.NoError(t, err)
	assert.True(t, has)

	got, err := db.GetMiniAppSession("n")
	require.NoError(t, err)
	assert.Equal(t, "approved_prejoin", got.Status)
}

// TestMiniAppSessions_ApprovingRevertOnFailure verifies that an approving
// reservation can be reverted to 'expired' (the path taken when Telegram's
// answerChatJoinRequestQuery fails after we already reserved).
func TestMiniAppSessions_ApprovingRevertOnFailure(t *testing.T) {
	db := newTestDB(t)
	expires := time.Now().Add(5 * time.Minute).Unix()
	_, _, err := db.CreateMiniAppSession("n", "Q", 10, 20, expires, "pending")
	require.NoError(t, err)

	ok, err := db.MarkMiniAppSession("n", "approving")
	require.NoError(t, err)
	assert.True(t, ok)

	ok, err = db.MarkMiniAppSession("n", "expired")
	require.NoError(t, err)
	assert.True(t, ok, "approving → expired must be a valid transition")

	got, err := db.GetMiniAppSession("n")
	require.NoError(t, err)
	assert.Equal(t, "expired", got.Status)

	// And the race guard must NOT match an expired session.
	has, err := db.HasRecentMiniAppApproval(10, 20, 300)
	require.NoError(t, err)
	assert.False(t, has, "expired sessions must not count toward the race guard")
}

func TestMiniAppSessions_MarkApprovedAtomic(t *testing.T) {
	db := newTestDB(t)
	expires := time.Now().Add(5 * time.Minute).Unix()
	_, _, err := db.CreateMiniAppSession("n", "Q", 1, 2, expires, "pending")
	require.NoError(t, err)

	// First mark wins.
	ok1, err := db.MarkMiniAppSession("n", "approved_prejoin")
	require.NoError(t, err)
	assert.True(t, ok1)

	// Second mark on already-terminal session must NOT succeed; this is the
	// race guarantee the HTTP server relies on for double-tap submissions.
	ok2, err := db.MarkMiniAppSession("n", "approved_prejoin")
	require.NoError(t, err)
	assert.False(t, ok2)

	// And the row stays in approved_prejoin, not flipped to anything else.
	got, err := db.GetMiniAppSession("n")
	require.NoError(t, err)
	assert.Equal(t, "approved_prejoin", got.Status)
	assert.True(t, got.CompletedAt.Valid)
}

func TestMiniAppSessions_MarkRejectsInvalidStatus(t *testing.T) {
	db := newTestDB(t)
	expires := time.Now().Add(5 * time.Minute).Unix()
	_, _, err := db.CreateMiniAppSession("n", "Q", 1, 2, expires, "pending")
	require.NoError(t, err)
	_, err = db.MarkMiniAppSession("n", "garbage")
	require.Error(t, err)
}

func TestMiniAppSessions_HasRecentApproval(t *testing.T) {
	db := newTestDB(t)
	expires := time.Now().Add(5 * time.Minute).Unix()

	// No session → false.
	has, err := db.HasRecentMiniAppApproval(10, 20, 300)
	require.NoError(t, err)
	assert.False(t, has)

	_, _, err = db.CreateMiniAppSession("n", "Q", 10, 20, expires, "pending")
	require.NoError(t, err)

	// Pending session is NOT an approval.
	has, err = db.HasRecentMiniAppApproval(10, 20, 300)
	require.NoError(t, err)
	assert.False(t, has)

	// Approve it → now within window.
	_, err = db.MarkMiniAppSession("n", "approved_prejoin")
	require.NoError(t, err)
	has, err = db.HasRecentMiniAppApproval(10, 20, 300)
	require.NoError(t, err)
	assert.True(t, has)

	// Outside window (negative window) should be false.
	has, err = db.HasRecentMiniAppApproval(10, 20, -1)
	require.NoError(t, err)
	assert.False(t, has)
}

// TestMiniAppSessions_ConcurrentCreate exercises the race between two
// goroutines that arrive at CreateMiniAppSession for the same (chat, user)
// before either INSERT has committed. Exactly one must end up with
// fresh=true; the other must reuse that winner via the conflict-recovery
// path (not surface a UNIQUE constraint error).
func TestMiniAppSessions_ConcurrentCreate(t *testing.T) {
	// File-backed DB on tmpfs: SQLite's :memory: store is per-connection,
	// so a multi-connection pool with :memory: would see empty databases
	// from each goroutine and hide the race entirely.
	dir := t.TempDir()
	db, err := New(dir + "/miniapp_race.db")
	require.NoError(t, err)
	require.NoError(t, db.Migrate(""))
	t.Cleanup(func() { db.Close() })

	expires := time.Now().Add(5 * time.Minute).Unix()

	const goroutines = 8
	type result struct {
		sess  *MiniAppSession
		fresh bool
		err   error
	}
	resultsCh := make(chan result, goroutines)
	start := make(chan struct{})
	for i := 0; i < goroutines; i++ {
		nonce := fmt.Sprintf("nonce-%d", i)
		go func(n string) {
			<-start
			s, f, err := db.CreateMiniAppSession(n, "Q", 42, 99, expires, "pending")
			resultsCh <- result{s, f, err}
		}(nonce)
	}
	close(start)

	freshCount := 0
	var winningNonce string
	for i := 0; i < goroutines; i++ {
		r := <-resultsCh
		require.NoError(t, r.err, "concurrent CreateMiniAppSession should never surface an error")
		require.NotNil(t, r.sess)
		if r.fresh {
			freshCount++
			winningNonce = r.sess.Nonce
		} else {
			// Losers must point at the same session as the winner.
			assert.Equal(t, int64(42), r.sess.ChatID)
			assert.Equal(t, int64(99), r.sess.UserID)
		}
	}
	assert.Equal(t, 1, freshCount, "exactly one goroutine should have inserted the pending row")
	assert.NotEmpty(t, winningNonce)
}

// TestMiniAppSessions_AwaitingDMFlow exercises the DM fallback lifecycle:
// create with status=awaiting_dm, claim on /start to promote to pending,
// then take the standard approving → approved_prejoin path.
func TestMiniAppSessions_AwaitingDMFlow(t *testing.T) {
	db := newTestDB(t)
	expires := time.Now().Add(5 * time.Minute).Unix()

	_, _, err := db.CreateMiniAppSession("dm-nonce", "", 10, 20, expires, "awaiting_dm")
	require.NoError(t, err)

	// /start arrives → claim promotes to pending AND extends expires_at.
	newExp := time.Now().Add(7 * time.Minute).Unix()
	got, ok, err := db.ClaimAwaitingDMByUser(20, newExp)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "dm-nonce", got.Nonce)
	assert.Equal(t, "pending", got.Status)
	assert.Equal(t, newExp, got.ExpiresAt, "promotion should extend expires_at to the new value")

	// Subsequent claim with no awaiting_dm sessions should be a no-op.
	_, ok, err = db.ClaimAwaitingDMByUser(20, newExp)
	require.NoError(t, err)
	assert.False(t, ok)

	// Standard race-guarded approve flow continues from here.
	ok, err = db.MarkMiniAppSession("dm-nonce", "approving")
	require.NoError(t, err)
	assert.True(t, ok)
	ok, err = db.MarkMiniAppSession("dm-nonce", "approved_prejoin")
	require.NoError(t, err)
	assert.True(t, ok)
}

// TestMiniAppSessions_PromoteAwaitingToPending exercises the proactive-DM
// promotion path. After a successful sendMessage to user_chat_id we
// transition awaiting_dm → pending AND extend the deadline so the user
// gets a fresh solve window from DM-delivery time.
func TestMiniAppSessions_PromoteAwaitingToPending(t *testing.T) {
	db := newTestDB(t)
	originalExpires := time.Now().Add(10 * time.Second).Unix() // near-expiry
	_, _, err := db.CreateMiniAppSession("proactive", "", 100, 200, originalExpires, "awaiting_dm")
	require.NoError(t, err)

	extended := time.Now().Add(5 * time.Minute).Unix()
	ok, err := db.PromoteAwaitingToPending("proactive", extended)
	require.NoError(t, err)
	assert.True(t, ok)

	got, err := db.GetMiniAppSession("proactive")
	require.NoError(t, err)
	assert.Equal(t, "pending", got.Status)
	assert.Equal(t, extended, got.ExpiresAt, "expires_at must be extended to give fresh solve window")
}

// TestMiniAppSessions_PromoteAwaitingToPending_ExpiredBlocked verifies
// the same close-the-race guarantee as ClaimAwaitingDMByUser: a session
// whose original deadline already passed cannot be silently promoted.
func TestMiniAppSessions_PromoteAwaitingToPending_ExpiredBlocked(t *testing.T) {
	db := newTestDB(t)
	pastExpires := time.Now().Add(-1 * time.Second).Unix()
	_, err := db.Exec(
		`INSERT INTO miniapp_sessions (nonce, chat_id, user_id, query_id, status, expires_at)
		      VALUES ('stale-proactive', 1, 2, '', 'awaiting_dm', ?)`,
		pastExpires,
	)
	require.NoError(t, err)

	extended := time.Now().Add(5 * time.Minute).Unix()
	ok, err := db.PromoteAwaitingToPending("stale-proactive", extended)
	require.NoError(t, err)
	assert.False(t, ok, "must not promote an already-expired session")
}

// TestMiniAppSessions_RefreshSessionForRetry covers the chat_join_request
// redelivery path: a second join request for the same (chat,user) finds
// the existing session and refreshes it. Two variants matter:
//   - new query_id present: status drives to 'pending' (covers
//     awaiting_dm → pending when Telegram first dropped query_id and the
//     retry includes it), query_id is overwritten with the new one,
//     deadline extended.
//   - new query_id empty: stale stored query_id is cleared (it can never
//     succeed past the 10s window), deadline extended; status untouched.
func TestMiniAppSessions_RefreshSessionForRetry(t *testing.T) {
	t.Run("with new query_id promotes awaiting_dm to pending", func(t *testing.T) {
		db := newTestDB(t)
		nearExpiry := time.Now().Add(10 * time.Second).Unix()
		_, _, err := db.CreateMiniAppSession("retry-1", "", 100, 200, nearExpiry, "awaiting_dm")
		require.NoError(t, err)

		extended := time.Now().Add(5 * time.Minute).Unix()
		ok, err := db.RefreshSessionForRetry("retry-1", "fresh-qid", extended)
		require.NoError(t, err)
		assert.True(t, ok)

		got, err := db.GetMiniAppSession("retry-1")
		require.NoError(t, err)
		assert.Equal(t, "pending", got.Status)
		assert.Equal(t, "fresh-qid", got.QueryID)
		assert.Equal(t, extended, got.ExpiresAt)
	})

	t.Run("with empty query_id clears stale query_id and extends", func(t *testing.T) {
		db := newTestDB(t)
		nearExpiry := time.Now().Add(10 * time.Second).Unix()
		_, _, err := db.CreateMiniAppSession("retry-2", "stale-qid", 100, 200, nearExpiry, "pending")
		require.NoError(t, err)

		extended := time.Now().Add(5 * time.Minute).Unix()
		ok, err := db.RefreshSessionForRetry("retry-2", "", extended)
		require.NoError(t, err)
		assert.True(t, ok)

		got, err := db.GetMiniAppSession("retry-2")
		require.NoError(t, err)
		assert.Equal(t, "pending", got.Status, "status untouched in legacy-clear branch")
		assert.Equal(t, "", got.QueryID, "stale query_id must be cleared so finishAndApprove uses legacy path")
		assert.Equal(t, extended, got.ExpiresAt)
	})

	t.Run("rejects expired session", func(t *testing.T) {
		db := newTestDB(t)
		pastExpires := time.Now().Add(-1 * time.Second).Unix()
		_, err := db.Exec(
			`INSERT INTO miniapp_sessions (nonce, chat_id, user_id, query_id, status, expires_at)
			      VALUES ('retry-stale', 1, 2, 'old', 'pending', ?)`,
			pastExpires,
		)
		require.NoError(t, err)

		extended := time.Now().Add(5 * time.Minute).Unix()
		ok, err := db.RefreshSessionForRetry("retry-stale", "new-qid", extended)
		require.NoError(t, err)
		assert.False(t, ok, "must not silently refresh an already-expired session")
	})
}

// TestMiniAppSessions_ClaimAwaitingDMByUser_ExpiredNotPromotable verifies
// that ClaimAwaitingDMByUser refuses to promote a session whose expires_at
// has already passed. Without this guard the /start handler could promote
// a stale row and immediately have the Mini App reject it on solve. This
// is Codex's fix #6: re-check expires_at in the UPDATE as well as the
// SELECT.
func TestMiniAppSessions_ClaimAwaitingDMByUser_ExpiredNotPromotable(t *testing.T) {
	db := newTestDB(t)
	pastExpires := time.Now().Add(-1 * time.Second).Unix()

	// Insert a stale row directly (Create rejects past expires_at).
	_, err := db.Exec(
		`INSERT INTO miniapp_sessions (nonce, chat_id, user_id, query_id, status, expires_at)
		      VALUES ('stale-dm', 1, 42, '', 'awaiting_dm', ?)`,
		pastExpires,
	)
	require.NoError(t, err)

	newExp := time.Now().Add(5 * time.Minute).Unix()
	_, ok, err := db.ClaimAwaitingDMByUser(42, newExp)
	require.NoError(t, err)
	assert.False(t, ok, "must NOT promote a session that expired between SELECT and UPDATE")

	// The row stays in awaiting_dm; the TTL sweeper will pick it up.
	got, err := db.GetMiniAppSession("stale-dm")
	require.NoError(t, err)
	assert.Equal(t, "awaiting_dm", got.Status)
}

// TestMiniAppSessions_ListExpiredForPolicyFallback exercises the policy
// sweeper's view of sessions the bot must resolve. The sweeper handles
// two categories — stale awaiting_dm (DM never delivered) and stale
// pending-with-empty-query_id (DM delivered, promoted, never solved) —
// while leaving modern-flow timeouts (pending + non-empty query_id) to
// expire silently.
func TestMiniAppSessions_ListExpiredForPolicyFallback(t *testing.T) {
	db := newTestDB(t)
	pastExpires := time.Now().Add(-1 * time.Second).Unix()
	futureExpires := time.Now().Add(5 * time.Minute).Unix()

	// Stale awaiting_dm — must be returned (bot is the only actor).
	_, err := db.Exec(
		`INSERT INTO miniapp_sessions (nonce, chat_id, user_id, query_id, status, expires_at)
		      VALUES ('dm-stale', 1, 2, '', 'awaiting_dm', ?)`,
		pastExpires,
	)
	require.NoError(t, err)

	// Stale pending with empty query_id (DM-fallback that got promoted
	// but never solved) — must be returned. This is the bug fix:
	// previously this row was invisible to the sweeper and the join
	// request was stranded.
	_, err = db.Exec(
		`INSERT INTO miniapp_sessions (nonce, chat_id, user_id, query_id, status, expires_at)
		      VALUES ('dm-promoted-stale', 10, 20, '', 'pending', ?)`,
		pastExpires,
	)
	require.NoError(t, err)

	// Stale pending with non-empty query_id (modern flow) — must NOT
	// be returned. Telegram showed the popup; let it stay in the
	// pending queue for human review.
	_, err = db.Exec(
		`INSERT INTO miniapp_sessions (nonce, chat_id, user_id, query_id, status, expires_at)
		      VALUES ('modern-stale', 100, 200, 'qid-modern', 'pending', ?)`,
		pastExpires,
	)
	require.NoError(t, err)

	// Fresh awaiting_dm — not stale, must NOT be returned.
	_, _, err = db.CreateMiniAppSession("dm-fresh", "", 3, 4, futureExpires, "awaiting_dm")
	require.NoError(t, err)

	// Stale but in terminal state ('expired') — must NOT be returned;
	// otherwise the sweeper would re-apply policy on every tick.
	_, err = db.Exec(
		`INSERT INTO miniapp_sessions (nonce, chat_id, user_id, query_id, status, expires_at)
		      VALUES ('already-expired', 1000, 2000, '', 'expired', ?)`,
		pastExpires,
	)
	require.NoError(t, err)

	expired, err := db.ListExpiredForPolicyFallback()
	require.NoError(t, err)
	nonces := make([]string, 0, len(expired))
	for _, s := range expired {
		nonces = append(nonces, s.Nonce)
	}
	assert.ElementsMatch(t, []string{"dm-stale", "dm-promoted-stale"}, nonces)
}

// TestMiniAppSessions_ExpireStaleMiniAppSessions_SkipsDMFallback verifies
// that the silent bulk-expirer only catches modern-flow timeouts. The
// DM-fallback promoted-pending rows are left for the policy sweeper.
func TestMiniAppSessions_ExpireStaleMiniAppSessions_SkipsDMFallback(t *testing.T) {
	db := newTestDB(t)
	pastExpires := time.Now().Add(-1 * time.Second).Unix()

	_, err := db.Exec(
		`INSERT INTO miniapp_sessions (nonce, chat_id, user_id, query_id, status, expires_at)
		      VALUES ('modern-stale', 1, 2, 'qid', 'pending', ?),
		             ('dm-promoted-stale', 3, 4, '', 'pending', ?)`,
		pastExpires, pastExpires,
	)
	require.NoError(t, err)

	n, err := db.ExpireStaleMiniAppSessions()
	require.NoError(t, err)
	assert.Equal(t, int64(1), n, "only the modern-flow row should be silently expired")

	modern, err := db.GetMiniAppSession("modern-stale")
	require.NoError(t, err)
	assert.Equal(t, "expired", modern.Status)

	dm, err := db.GetMiniAppSession("dm-promoted-stale")
	require.NoError(t, err)
	assert.Equal(t, "pending", dm.Status, "DM-fallback row must remain pending for the policy sweeper to handle")
}

func TestMiniAppSessions_CountAwaitingDMByUser(t *testing.T) {
	db := newTestDB(t)
	futureExpires := time.Now().Add(5 * time.Minute).Unix()
	pastExpires := time.Now().Add(-1 * time.Second).Unix()

	_, _, err := db.CreateMiniAppSession("fresh-1", "", 1, 42, futureExpires, "awaiting_dm")
	require.NoError(t, err)
	_, _, err = db.CreateMiniAppSession("other-user", "", 1, 99, futureExpires, "awaiting_dm")
	require.NoError(t, err)
	_, err = db.Exec(
		`INSERT INTO miniapp_sessions (nonce, chat_id, user_id, query_id, status, expires_at)
		      VALUES ('expired', 2, 42, '', 'awaiting_dm', ?)`,
		pastExpires,
	)
	require.NoError(t, err)

	count, err := db.CountAwaitingDMByUser(42)
	require.NoError(t, err)
	assert.Equal(t, 1, count)
}

func TestMiniAppSessions_ExpireStale(t *testing.T) {
	db := newTestDB(t)
	pastExpires := time.Now().Add(-1 * time.Second).Unix()
	futureExpires := time.Now().Add(5 * time.Minute).Unix()

	// CreateMiniAppSession's "reuse pending" path requires expires_at > now,
	// so the stale row can't be inserted through it. Drop directly to SQL.
	_, err := db.Exec(
		`INSERT INTO miniapp_sessions (nonce, chat_id, user_id, query_id, status, expires_at)
		      VALUES ('stale', 1, 2, 'Q', 'pending', ?)`,
		pastExpires,
	)
	require.NoError(t, err)
	_, _, err = db.CreateMiniAppSession("fresh", "Q", 3, 4, futureExpires, "pending")
	require.NoError(t, err)

	n, err := db.ExpireStaleMiniAppSessions()
	require.NoError(t, err)
	assert.Equal(t, int64(1), n)

	stale, err := db.GetMiniAppSession("stale")
	require.NoError(t, err)
	assert.Equal(t, "expired", stale.Status)
	fresh, err := db.GetMiniAppSession("fresh")
	require.NoError(t, err)
	assert.Equal(t, "pending", fresh.Status)
}

func TestMiniAppSessions_CleanupOldMiniAppSessions(t *testing.T) {
	db := newTestDB(t)
	_, err := db.Exec(
		`INSERT INTO miniapp_sessions (nonce, chat_id, user_id, query_id, status, expires_at, created_at)
		      VALUES ('old', 1, 2, '', 'declined', ?, ?)`,
		time.Now().Add(5*time.Minute).Unix(),
		time.Now().Add(-2*time.Hour).Unix(),
	)
	require.NoError(t, err)
	_, _, err = db.CreateMiniAppSession("fresh", "", 3, 4, time.Now().Add(5*time.Minute).Unix(), "pending")
	require.NoError(t, err)

	require.NoError(t, db.CleanupOldMiniAppSessions(3600))

	_, err = db.GetMiniAppSession("old")
	require.Error(t, err)
	fresh, err := db.GetMiniAppSession("fresh")
	require.NoError(t, err)
	assert.Equal(t, "fresh", fresh.Nonce)
}

func TestCloseActiveMiniAppSessions_ClosesOnlyThatUsersOpenSessions(t *testing.T) {
	db := newTestDB(t)
	expires := time.Now().Add(5 * time.Minute).Unix()
	seed := func(nonce, queryID string, chatID, userID int64, status string) {
		t.Helper()
		_, _, err := db.CreateMiniAppSession(nonce, queryID, chatID, userID, expires, status)
		require.NoError(t, err)
	}
	seed("target-pending", "q", -100, 1, "pending")
	seed("other-user", "q", -100, 2, "pending")
	seed("other-chat", "", -200, 1, "awaiting_dm")
	seed("approved-earlier", "q", -300, 1, "pending")
	ok, err := db.MarkMiniAppSession("approved-earlier", "approving")
	require.NoError(t, err)
	require.True(t, ok)

	closed, err := db.CloseActiveMiniAppSessions(-100, 1)
	require.NoError(t, err)
	assert.Equal(t, int64(1), closed)

	want := map[string]string{
		"target-pending":   "expired",
		"other-user":       "pending",
		"other-chat":       "awaiting_dm",
		"approved-earlier": "approving",
	}
	for nonce, status := range want {
		sess, err := db.GetMiniAppSession(nonce)
		require.NoError(t, err)
		assert.Equal(t, status, sess.Status, nonce)
	}

	again, err := db.CloseActiveMiniAppSessions(-100, 1)
	require.NoError(t, err)
	assert.Zero(t, again, "nothing left to close")
}
