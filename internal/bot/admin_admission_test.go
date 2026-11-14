package bot

import (
	"database/sql"
	"errors"
	"testing"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/szres/ing-recaptcha/internal/database"
)

const (
	admitAdminID  int64 = 77
	admitMemberID int64 = 78
)

// admissionBot is a fully provisioned bot in a chat where admitAdminID is an
// administrator and everyone else is an ordinary member.
func admissionBot(t *testing.T) (*Bot, *mockTelegramAPI) {
	t.Helper()
	b, mock := provisionedBot(t)
	mock.chatMemberFn = func(cfg tgbotapi.GetChatMemberConfig) (tgbotapi.ChatMember, error) {
		switch cfg.UserID {
		case passBotID:
			return tgbotapi.ChatMember{Status: "administrator", CanRestrictMembers: true, CanDeleteMessages: true}, nil
		case admitAdminID:
			return tgbotapi.ChatMember{Status: "administrator"}, nil
		}
		return tgbotapi.ChatMember{Status: "member"}, nil
	}
	return b, mock
}

// joinPerformedBy is a fresh join of passUserID that Telegram attributes to
// performer: the admin who approved the request, whoever added them, or the
// joiner themselves.
func joinPerformedBy(performer tgbotapi.User) *tgbotapi.ChatMemberUpdated {
	update := freshJoin(passChatID, passUserID)
	update.From = performer
	return update
}

type admissionEvent struct {
	Outcome string
	AdminID sql.NullInt64
}

func admissionEvents(t *testing.T, b *Bot) []admissionEvent {
	t.Helper()
	rows, err := b.db.Query("SELECT outcome, admin_user_id FROM verification_events WHERE chat_id = ? AND user_id = ?", passChatID, passUserID)
	require.NoError(t, err)
	defer rows.Close()
	var events []admissionEvent
	for rows.Next() {
		var e admissionEvent
		require.NoError(t, rows.Scan(&e.Outcome, &e.AdminID))
		events = append(events, e)
	}
	require.NoError(t, rows.Err())
	return events
}

func assertAdmittedUntouched(t *testing.T, b *Bot, mock *mockTelegramAPI) {
	t.Helper()
	mock.mu.Lock()
	requests, sends := len(mock.requested), len(mock.sent)
	mock.mu.Unlock()
	assert.Zero(t, requests, "no mute, no kick")
	assert.Zero(t, sends, "no challenge message")
	assert.Empty(t, rawRequests(mock), "their permissions are left as the group has them")
	pv, err := b.db.GetPendingVerification(passChatID, passUserID)
	require.NoError(t, err)
	assert.Nil(t, pv)
}

func TestAdminAdmission_ApprovedJoinSkipsVerification(t *testing.T) {
	b, mock := admissionBot(t)

	b.handleChatMemberUpdate(joinPerformedBy(tgbotapi.User{ID: admitAdminID, FirstName: "Admin"}))

	assertAdmittedUntouched(t, b, mock)
	assert.Equal(t, []admissionEvent{{Outcome: "passed", AdminID: sql.NullInt64{Int64: admitAdminID, Valid: true}}},
		admissionEvents(t, b), "the admission is on record with the admin who made it")
	assertVerified(t, b, passUserID, false, "an admin letting someone in is not that user solving a captcha")
}

func TestAdminAdmission_AnonymousAdminCounts(t *testing.T) {
	b, mock := admissionBot(t)

	b.handleChatMemberUpdate(joinPerformedBy(tgbotapi.User{ID: groupAnonymousBotID, IsBot: true, FirstName: "Group"}))

	assertAdmittedUntouched(t, b, mock)
	require.Len(t, admissionEvents(t, b), 1)
}

func TestAdminAdmission_ClearsStrikeFromEarlierAttempt(t *testing.T) {
	b, _ := admissionBot(t)
	require.NoError(t, b.db.RecordVerificationFailure(passChatID, passUserID))

	b.handleChatMemberUpdate(joinPerformedBy(tgbotapi.User{ID: admitAdminID}))

	strike, err := b.db.HasPreviousFailure(passChatID, passUserID)
	require.NoError(t, err)
	assert.False(t, strike)
}

func TestAdminAdmission_ClosesTheBotsOwnPendingVerification(t *testing.T) {
	for _, status := range []string{"pending", "awaiting_dm"} {
		t.Run(status, func(t *testing.T) {
			b, _ := admissionBot(t)
			queryID := "q-1"
			if status == "awaiting_dm" {
				queryID = ""
			}
			_, _, err := b.db.CreateMiniAppSession("nonce", queryID, passChatID, passUserID, time.Now().Add(5*time.Minute).Unix(), status)
			require.NoError(t, err)

			b.handleChatMemberUpdate(joinPerformedBy(tgbotapi.User{ID: admitAdminID}))

			sess, err := b.db.GetMiniAppSession("nonce")
			require.NoError(t, err)
			assert.Equal(t, "expired", sess.Status, "or the join-policy fallback would later act on a request that is gone")
		})
	}
}

func TestAdminAdmission_OnlyAnAdministratorsActionCounts(t *testing.T) {
	performers := map[string]tgbotapi.User{
		"the joiner walked in":           {ID: passUserID},
		"performer unknown":              {},
		"an ordinary member added them":  {ID: admitMemberID},
		"another bot approved them":      {ID: 555, IsBot: true},
		"this bot's own policy fallback": {ID: passBotID, IsBot: true},
	}
	for name, performer := range performers {
		t.Run(name, func(t *testing.T) {
			b, mock := admissionBot(t)

			b.handleChatMemberUpdate(joinPerformedBy(performer))

			assert.True(t, mutedOnJoin(mock, passUserID), "verified like any other joiner")
			assert.Empty(t, admissionEvents(t, b))
		})
	}
}

func TestAdminAdmission_AdminBotIsNotAnAdministratorsJudgement(t *testing.T) {
	b, mock := admissionBot(t)
	const otherGuardBot int64 = 556
	inner := mock.chatMemberFn
	mock.chatMemberFn = func(cfg tgbotapi.GetChatMemberConfig) (tgbotapi.ChatMember, error) {
		if cfg.UserID == otherGuardBot {
			return tgbotapi.ChatMember{Status: "administrator"}, nil
		}
		return inner(cfg)
	}

	b.handleChatMemberUpdate(joinPerformedBy(tgbotapi.User{ID: otherGuardBot, IsBot: true}))

	assert.True(t, mutedOnJoin(mock, passUserID))
}

func TestAdminAdmission_LookupFailureVerifiesAsUsual(t *testing.T) {
	b, mock := admissionBot(t)
	inner := mock.chatMemberFn
	mock.chatMemberFn = func(cfg tgbotapi.GetChatMemberConfig) (tgbotapi.ChatMember, error) {
		if cfg.UserID == admitAdminID {
			return tgbotapi.ChatMember{}, errors.New("Bad Gateway")
		}
		return inner(cfg)
	}

	b.handleChatMemberUpdate(joinPerformedBy(tgbotapi.User{ID: admitAdminID}))

	assert.True(t, mutedOnJoin(mock, passUserID), "when it can't be confirmed that an admin did it, nobody is waved through")
}

func TestAdminAdmission_PassListMemberIsNotRecordedAsAdminAdmission(t *testing.T) {
	b, mock := admissionBot(t)
	require.NoError(t, b.db.MarkUserVerified(passUserID, passOtherChatID, database.VerifiedSourceCaptcha))

	b.handleChatMemberUpdate(joinPerformedBy(tgbotapi.User{ID: admitAdminID}))

	assertAdmittedUntouched(t, b, mock)
	assert.Empty(t, admissionEvents(t, b), "they were entitled to enter anyway")
}
