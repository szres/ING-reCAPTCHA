package database

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := New(":memory:")
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	require.NoError(t, db.Migrate(""))
	t.Cleanup(func() { db.Close() })
	return db
}

func TestNew_InMemory(t *testing.T) {
	db, err := New(":memory:")
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	defer db.Close()
	require.NoError(t, db.Ping())
}

func TestMigrate_CreatesAllTables(t *testing.T) {
	db := newTestDB(t)
	tables := []string{
		"image_sets", "images", "pending_verifications",
		"admins", "user_join_history", "verification_failures",
		"user_language_preferences", "image_set_labels",
		"verified_users", "scheduled_message_deletions",
	}
	for _, table := range tables {
		ok, err := db.HasTable(table)
		require.NoError(t, err)
		assert.True(t, ok, "table %q should exist", table)
	}
}

func TestMigrate_Idempotent(t *testing.T) {
	db := newTestDB(t)
	// Running migrate again should not error (all CREATE TABLE IF NOT EXISTS)
	require.NoError(t, db.Migrate(""))
}
