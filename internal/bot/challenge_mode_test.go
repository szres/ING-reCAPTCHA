package bot

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	modeChatID int64 = -1009990000000
	modeUserID int64 = 5150
)

func enableEphemeral(t *testing.T, b *Bot, adminView bool) {
	t.Helper()
	on := true
	require.NoError(t, b.db.SetChatEphemeralVerify(modeChatID, &on, 1))
	require.NoError(t, b.db.SetChatEphemeralAdminVisible(modeChatID, adminView, 1))
}

func TestChooseChallengeMode_FirstAttemptIsEphemeral(t *testing.T) {
	b, _ := newTestBot(t)
	enableEphemeral(t, b, true)

	ephemeral, adminVisible := b.chooseChallengeMode(modeChatID, modeUserID, false)
	assert.True(t, ephemeral)
	assert.True(t, adminVisible, "admin view rides along with the ephemeral challenge")
}

func TestChooseChallengeMode_PriorFailureFallsBackToRegular(t *testing.T) {
	b, _ := newTestBot(t)
	enableEphemeral(t, b, true)
	require.NoError(t, b.db.RecordVerificationFailure(modeChatID, modeUserID))

	ephemeral, adminVisible := b.chooseChallengeMode(modeChatID, modeUserID, false)
	assert.False(t, ephemeral, "the second attempt must use the regular challenge every client can render")
	assert.False(t, adminVisible, "no ephemeral copies to broadcast on a regular challenge")
}

func TestChooseChallengeMode_StrikeIsPerChat(t *testing.T) {
	b, _ := newTestBot(t)
	enableEphemeral(t, b, false)
	// A strike in another group says nothing about this one.
	require.NoError(t, b.db.RecordVerificationFailure(-1001234, modeUserID))

	ephemeral, _ := b.chooseChallengeMode(modeChatID, modeUserID, false)
	assert.True(t, ephemeral)
}

func TestChooseChallengeMode_ClearedStrikeRestoresEphemeral(t *testing.T) {
	b, _ := newTestBot(t)
	enableEphemeral(t, b, false)
	require.NoError(t, b.db.RecordVerificationFailure(modeChatID, modeUserID))
	// A pass, admin approval, or expiry clears the strike.
	require.NoError(t, b.db.ClearVerificationFailure(modeChatID, modeUserID))

	ephemeral, _ := b.chooseChallengeMode(modeChatID, modeUserID, false)
	assert.True(t, ephemeral)
}

func TestChooseChallengeMode_NeverEphemeralWhenDisabledPrivateOrTest(t *testing.T) {
	b, _ := newTestBot(t)

	ephemeral, _ := b.chooseChallengeMode(modeChatID, modeUserID, false)
	assert.False(t, ephemeral, "ephemeral not enabled for this chat")

	enableEphemeral(t, b, true)
	ephemeral, adminVisible := b.chooseChallengeMode(modeChatID, modeUserID, true)
	assert.False(t, ephemeral, "test verifications stay public for QA")
	assert.False(t, adminVisible)

	ephemeral, _ = b.chooseChallengeMode(modeUserID, modeUserID, false)
	assert.False(t, ephemeral, "private chats have no ephemeral messages")
}

func TestChooseChallengeMode_StrikeLookupErrorUsesRegular(t *testing.T) {
	b, _ := newTestBot(t)
	// Ephemeral on via global config, so the chat-settings read failing on a
	// closed DB still resolves to "on" and the strike lookup is what errors.
	b.cfg.EphemeralVerifyEnabled = true
	require.NoError(t, b.db.Close())

	ephemeral, adminVisible := b.chooseChallengeMode(modeChatID, modeUserID, false)
	assert.False(t, ephemeral, "an unknown strike state must not risk a challenge the user can't see")
	assert.False(t, adminVisible)
}

func TestHandleExpiredVerification_FailedPermabanKeepsStrike(t *testing.T) {
	b, mock := newTestBot(t)
	require.NoError(t, b.db.CreatePendingVerification(modeChatID, modeUserID, []string{"cat"}, time.Now().Add(-time.Second), false, "en", nil))
	require.NoError(t, b.db.RecordVerificationFailure(modeChatID, modeUserID))
	mock.requestErr = errors.New("Bad Request: not enough rights to restrict/unrestrict chat member")

	pv, err := b.db.GetPendingVerification(modeChatID, modeUserID)
	require.NoError(t, err)
	b.handleExpiredVerification(pv)

	strike, err := b.db.HasPreviousFailure(modeChatID, modeUserID)
	require.NoError(t, err)
	assert.True(t, strike, "a ban that didn't happen must leave the user marked for one")
}

func TestHandleExpiredVerification_SuccessfulPermabanClearsStrike(t *testing.T) {
	b, _ := newTestBot(t)
	require.NoError(t, b.db.CreatePendingVerification(modeChatID, modeUserID, []string{"cat"}, time.Now().Add(-time.Second), false, "en", nil))
	require.NoError(t, b.db.RecordVerificationFailure(modeChatID, modeUserID))

	pv, err := b.db.GetPendingVerification(modeChatID, modeUserID)
	require.NoError(t, err)
	b.handleExpiredVerification(pv)

	strike, err := b.db.HasPreviousFailure(modeChatID, modeUserID)
	require.NoError(t, err)
	assert.False(t, strike)
}
