package bot

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"testing"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// documentedMemberPermissions is every field of the Bot API's ChatPermissions
// object. A permission missing from a restrictChatMember call is denied or
// derived from another one, so lifting a restriction has to name them all.
var documentedMemberPermissions = []string{
	"can_send_messages",
	"can_send_audios",
	"can_send_documents",
	"can_send_photos",
	"can_send_videos",
	"can_send_video_notes",
	"can_send_voice_notes",
	"can_send_polls",
	"can_send_other_messages",
	"can_add_web_page_previews",
	"can_react_to_messages",
	"can_edit_tag",
	"can_change_info",
	"can_invite_users",
	"can_pin_messages",
	"can_manage_topics",
}

func rawRequests(mock *mockTelegramAPI) []rawRequest {
	mock.mu.Lock()
	defer mock.mu.Unlock()
	return append([]rawRequest(nil), mock.rawRequests...)
}

// liftedFor returns the permissions the bot granted userID in chatID through
// restrictChatMember, failing the test unless it did so exactly once.
func liftedFor(t *testing.T, mock *mockTelegramAPI, chatID, userID int64) map[string]bool {
	t.Helper()
	var lifts []rawRequest
	for _, r := range rawRequests(mock) {
		if r.Endpoint == "restrictChatMember" {
			lifts = append(lifts, r)
		}
	}
	require.Len(t, lifts, 1, "expected exactly one restrictChatMember call")
	assert.Equal(t, strconv.FormatInt(chatID, 10), lifts[0].Params["chat_id"])
	assert.Equal(t, strconv.FormatInt(userID, 10), lifts[0].Params["user_id"])

	var granted map[string]bool
	require.NoError(t, json.Unmarshal([]byte(lifts[0].Params["permissions"]), &granted))
	return granted
}

func TestUnrestrictUser_GrantsEveryDocumentedPermission(t *testing.T) {
	b, mock := newTestBot(t)

	require.NoError(t, b.unrestrictUser(passChatID, passUserID))

	granted := liftedFor(t, mock, passChatID, passUserID)
	for _, permission := range documentedMemberPermissions {
		assert.True(t, granted[permission], "%s must be granted, or the member stays restricted", permission)
	}
	assert.Len(t, granted, len(documentedMemberPermissions), "nothing but the documented permissions is sent")
	for permission, value := range granted {
		assert.True(t, value, "%s: lifting a restriction never denies anything", permission)
	}
}

func TestUnrestrictUser_ReportsTelegramError(t *testing.T) {
	b, mock := newTestBot(t)
	mock.rawRequestErr = &tgbotapi.Error{Code: 400, Message: "Bad Request: not enough rights to restrict/unrestrict chat member"}

	err := b.unrestrictUser(passChatID, passUserID)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "not enough rights")
}

func TestVerifiedMembersCanEditTheirTag(t *testing.T) {
	// A member who is verified must end up able to edit their own tag: the
	// restore step used to hold back can_pin_messages, which can_edit_tag
	// follows when it is not named.
	paths := map[string]func(t *testing.T, b *Bot){
		"solved the captcha": func(t *testing.T, b *Bot) {
			pv := seedPendingChallenge(t, b, passChatID, passUserID, false)
			b.evaluateVerification(makeQuery(passUserID, passChatID), pv, []string{"cat", "dog", "bird"})
		},
		"approved by an admin": func(t *testing.T, b *Bot) {
			b.api.(*mockTelegramAPI).chatMemberFn = func(tgbotapi.GetChatMemberConfig) (tgbotapi.ChatMember, error) {
				return tgbotapi.ChatMember{Status: "administrator"}, nil
			}
			pv := seedPendingChallenge(t, b, passChatID, passUserID, false)
			query := makeQuery(7, passChatID)
			query.Data = fmt.Sprintf("a:%d:%d", passUserID, pv.ChallengeVersion)
			b.handleAdminApproveCallback(query)
		},
		"let in because no captcha could be built": func(t *testing.T, b *Bot) {
			// No image sets are configured, so the join is auto-approved.
			require.NoError(t, b.startVerification(passChatID, passUserID, &tgbotapi.User{ID: passUserID}))
		},
	}
	for name, verify := range paths {
		t.Run(name, func(t *testing.T) {
			b, mock := newTestBot(t)

			verify(t, b)

			granted := liftedFor(t, mock, passChatID, passUserID)
			assert.True(t, granted["can_edit_tag"])
			assert.True(t, granted["can_pin_messages"], "can_edit_tag and can_manage_topics follow this one when omitted")
		})
	}
}

func TestUnrestrictUser_FailureDoesNotBlockThePass(t *testing.T) {
	b, mock := newTestBot(t)
	mock.rawRequestErr = errors.New("dial tcp: i/o timeout")
	pv := seedPendingChallenge(t, b, passChatID, passUserID, false)

	b.evaluateVerification(makeQuery(passUserID, passChatID), pv, []string{"cat", "dog", "bird"})

	gone, err := b.db.GetPendingVerification(passChatID, passUserID)
	require.NoError(t, err)
	assert.Nil(t, gone, "the verification is still completed")
	assertVerified(t, b, passUserID, true, "and the pass is still recorded")
}
