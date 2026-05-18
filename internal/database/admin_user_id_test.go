package database

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRecordVerificationEvent_AdminUserIDNullableAndQueryable(t *testing.T) {
	db := newTestDB(t)

	// User-driven event: AdminUserID NULL.
	require.NoError(t, db.RecordVerificationEvent(VerificationEvent{
		ChatID: 1, UserID: 100, Outcome: "passed",
		CompletionTimeMs: sql.NullInt64{Int64: 5000, Valid: true},
		OccurredAt:       1_000,
	}))

	// Admin-driven event: AdminUserID set.
	require.NoError(t, db.RecordVerificationEvent(VerificationEvent{
		ChatID: 1, UserID: 101, Outcome: "passed",
		AdminUserID: sql.NullInt64{Int64: 999, Valid: true},
		OccurredAt:  1_001,
	}))

	// Admin-driven ban: outcome=failed + AdminUserID set.
	require.NoError(t, db.RecordVerificationEvent(VerificationEvent{
		ChatID: 1, UserID: 102, Outcome: "failed",
		AdminUserID: sql.NullInt64{Int64: 999, Valid: true},
		OccurredAt:  1_002,
	}))

	var nullCount, adminCount int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM verification_events WHERE admin_user_id IS NULL`).Scan(&nullCount))
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM verification_events WHERE admin_user_id = 999`).Scan(&adminCount))

	assert.Equal(t, 1, nullCount, "exactly one user-driven event")
	assert.Equal(t, 2, adminCount, "two admin-driven events for admin 999")
}
