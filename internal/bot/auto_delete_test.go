package bot

import (
	"errors"
	"fmt"
	"testing"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/szres/ing-recaptcha/internal/database"
)

const (
	noticeChatID    int64 = -100123 // the chat commandMsg uses
	noticeUserID    int64 = 4242
	noticeMessageID       = 777
)

func TestParsePassNoticeTTL(t *testing.T) {
	accepted := map[string]time.Duration{
		"off":      0,
		"0":        0,
		"disable":  0,
		"disabled": 0,
		"false":    0,
		"5":        5 * time.Second,
		"30":       30 * time.Second,
		"90s":      90 * time.Second,
		"5m":       5 * time.Minute,
		"1h":       time.Hour,
		"1h30m":    90 * time.Minute,
		"86400":    24 * time.Hour,
		"24h":      24 * time.Hour,
	}
	for value, want := range accepted {
		got, ok := parsePassNoticeTTL(value)
		assert.True(t, ok, "%q should be accepted", value)
		assert.Equal(t, want, got, "%q", value)
	}

	rejected := []string{
		"", "on", "yes", "soon", "4", "4s", "-30", "-5m", "86401", "25h",
		"1.5s", "500ms", "30x", "99999999999999999999",
	}
	for _, value := range rejected {
		_, ok := parsePassNoticeTTL(value)
		assert.False(t, ok, "%q should be rejected", value)
	}
}

// settingBot is a bot whose every chat member is a group administrator, so
// /setting passes its authorization gate.
func settingBot(t *testing.T) (*Bot, *mockTelegramAPI) {
	t.Helper()
	b, mock := newTestBot(t)
	mock.sentMessageID = noticeMessageID
	mock.chatMemberFn = func(tgbotapi.GetChatMemberConfig) (tgbotapi.ChatMember, error) {
		return tgbotapi.ChatMember{Status: "administrator"}, nil
	}
	return b, mock
}

func storedPassNoticeTTL(t *testing.T, b *Bot) time.Duration {
	t.Helper()
	return b.resolveChatSettings(noticeChatID).PassNoticeTTL
}

func TestCmdSetting_AutoDeleteSetShowAndTurnOff(t *testing.T) {
	b, mock := settingBot(t)

	b.cmdSetting(commandMsg("supergroup"), "autodelete 30")
	assert.Equal(t, 30*time.Second, storedPassNoticeTTL(t, b))
	assert.Contains(t, lastSentMessage(t, mock).Text, "<code>30s</code>")

	b.cmdSetting(commandMsg("supergroup"), "AutoDelete 5M")
	assert.Equal(t, 5*time.Minute, storedPassNoticeTTL(t, b), "units and any letter case are accepted")

	b.cmdSetting(commandMsg("supergroup"), "")
	shown := lastSentMessage(t, mock).Text
	assert.Contains(t, shown, "<code>autodelete</code>: <code>300s</code>")
	assert.Contains(t, shown, "/setting autodelete &lt;seconds&gt;|off")

	b.cmdSetting(commandMsg("supergroup"), "autodelete off")
	assert.Zero(t, storedPassNoticeTTL(t, b))
	b.cmdSetting(commandMsg("supergroup"), "")
	assert.Contains(t, lastSentMessage(t, mock).Text, "<code>autodelete</code>: <code>off</code>")
}

func TestCmdSetting_AutoDeleteRejectsBadValuesWithoutChangingAnything(t *testing.T) {
	b, mock := settingBot(t)
	b.cmdSetting(commandMsg("supergroup"), "autodelete 60")

	for _, args := range []string{"autodelete soon", "autodelete 2", "autodelete 2d", "autodelete 25h"} {
		b.cmdSetting(commandMsg("supergroup"), args)
		reply := lastSentMessage(t, mock).Text
		assert.Contains(t, reply, "❌", args)
		assert.Contains(t, reply, "<code>5s</code>", "the reply states the allowed range (%s)", args)
		assert.Equal(t, 60*time.Second, storedPassNoticeTTL(t, b), args)
	}

	b.cmdSetting(commandMsg("supergroup"), "autodelete")
	assert.Contains(t, lastSentMessage(t, mock).Text, "Usage:")
	assert.Equal(t, 60*time.Second, storedPassNoticeTTL(t, b))
}

func TestCmdSetting_AutoDeleteNeedsAGroupAdmin(t *testing.T) {
	b, mock := settingBot(t)
	mock.chatMemberFn = func(tgbotapi.GetChatMemberConfig) (tgbotapi.ChatMember, error) {
		return tgbotapi.ChatMember{Status: "member"}, nil
	}

	b.cmdSetting(commandMsg("supergroup"), "autodelete 30")

	assert.Zero(t, storedPassNoticeTTL(t, b))
	assert.Contains(t, lastSentMessage(t, mock).Text, "Only administrators")
}

func TestCmdSetting_UnknownSettingListsAutoDelete(t *testing.T) {
	b, mock := settingBot(t)

	b.cmdSetting(commandMsg("supergroup"), "nonsense on")

	assert.Contains(t, lastSentMessage(t, mock).Text, "/setting autodelete")
}

func TestCmdSetup_ShowsAutoDelete(t *testing.T) {
	b, mock := setupTestBot(t)
	mock.chatMemberFn = botMemberFn(b.self.ID, tgbotapi.ChatMember{
		Status: "administrator", CanRestrictMembers: true, CanDeleteMessages: true,
	})

	b.cmdSetup(commandMsg("supergroup"), "")
	assert.Contains(t, lastSentMessage(t, mock).Text, `Auto-delete "passed" notice: <code>off</code>`)

	require.NoError(t, b.db.SetChatPassNoticeTTL(noticeChatID, 45, 42))
	b.cmdSetup(commandMsg("supergroup"), "")
	assert.Contains(t, lastSentMessage(t, mock).Text, `Auto-delete "passed" notice: <code>45s</code>`)
}

func scheduledDeletions(t *testing.T, b *Bot) []database.ScheduledDeletion {
	t.Helper()
	all, err := b.db.GetDueMessageDeletions(time.Now().Add(48*time.Hour), 100)
	require.NoError(t, err)
	return all
}

// requireScheduledIn asserts exactly one deletion is scheduled, for the
// notice the mock just "sent", about ttl from now.
func requireScheduledIn(t *testing.T, b *Bot, chatID int64, ttl time.Duration) {
	t.Helper()
	all := scheduledDeletions(t, b)
	require.Len(t, all, 1)
	assert.Equal(t, chatID, all[0].ChatID)
	assert.Equal(t, noticeMessageID, all[0].MessageID)
	assert.WithinDuration(t, time.Now().Add(ttl), all[0].DeleteAt, 5*time.Second)
}

func TestPassNotice_ScheduledForDeletionWhenTheGroupAsksForIt(t *testing.T) {
	b, _ := settingBot(t)
	require.NoError(t, b.db.SetChatPassNoticeTTL(noticeChatID, 30, 42))
	pv := seedPendingChallenge(t, b, noticeChatID, noticeUserID, false)

	b.evaluateVerification(makeQuery(noticeUserID, noticeChatID), pv, []string{"cat", "dog", "bird"})

	requireScheduledIn(t, b, noticeChatID, 30*time.Second)
}

func TestPassNotice_KeptWhenAutoDeleteIsOff(t *testing.T) {
	b, mock := settingBot(t)
	pv := seedPendingChallenge(t, b, noticeChatID, noticeUserID, false)

	b.evaluateVerification(makeQuery(noticeUserID, noticeChatID), pv, []string{"cat", "dog", "bird"})

	require.NotEmpty(t, lastSentMessage(t, mock).Text, "the notice itself is still posted")
	assert.Empty(t, scheduledDeletions(t, b))
}

func TestPassNotice_OtherGroupsSettingDoesNotApply(t *testing.T) {
	b, _ := settingBot(t)
	require.NoError(t, b.db.SetChatPassNoticeTTL(-100999, 30, 42))
	pv := seedPendingChallenge(t, b, noticeChatID, noticeUserID, false)

	b.evaluateVerification(makeQuery(noticeUserID, noticeChatID), pv, []string{"cat", "dog", "bird"})

	assert.Empty(t, scheduledDeletions(t, b))
}

func TestPassNotice_FailedCaptchaSchedulesNothing(t *testing.T) {
	b, _ := settingBot(t)
	require.NoError(t, b.db.SetChatPassNoticeTTL(noticeChatID, 30, 42))
	mockMember := func(tgbotapi.GetChatMemberConfig) (tgbotapi.ChatMember, error) {
		return tgbotapi.ChatMember{Status: "restricted"}, nil
	}
	b.api.(*mockTelegramAPI).chatMemberFn = mockMember
	pv := seedPendingChallenge(t, b, noticeChatID, noticeUserID, false)

	b.evaluateVerification(makeQuery(noticeUserID, noticeChatID), pv, []string{"fish", "lion", "bear"})

	assert.Empty(t, scheduledDeletions(t, b))
}

func TestPassNotice_AdminApprovalNoticeIsScheduledToo(t *testing.T) {
	b, mock := settingBot(t)
	require.NoError(t, b.db.SetChatPassNoticeTTL(noticeChatID, 120, 42))
	pv := seedPendingChallenge(t, b, noticeChatID, noticeUserID, false)
	query := makeQuery(7, noticeChatID)
	query.Data = fmt.Sprintf("a:%d:%d", noticeUserID, pv.ChallengeVersion)

	b.handleAdminApproveCallback(query)

	notice := lastSentMessage(t, mock)
	assert.Equal(t, toast(b, "verify_admin_approved_msg"), notice.Text)
	assert.Equal(t, "HTML", notice.ParseMode)
	requireScheduledIn(t, b, noticeChatID, 120*time.Second)
}

func TestPassNotice_MiniAppWelcomeIsScheduledToo(t *testing.T) {
	b, mock := settingBot(t)
	b.cfg.WelcomeMessageInGroup = true
	require.NoError(t, b.db.SetChatPassNoticeTTL(noticeChatID, 60, 42))
	mock.chatMemberFn = func(cfg tgbotapi.GetChatMemberConfig) (tgbotapi.ChatMember, error) {
		return tgbotapi.ChatMember{Status: "member", User: &tgbotapi.User{ID: cfg.UserID, FirstName: "Joiner"}}, nil
	}

	b.onMiniAppApproved(noticeChatID, noticeUserID)

	assert.Contains(t, lastSentMessage(t, mock).Text, "Joiner")
	requireScheduledIn(t, b, noticeChatID, 60*time.Second)
}

func TestPassNotice_FailedSendSchedulesNothing(t *testing.T) {
	b, mock := settingBot(t)
	require.NoError(t, b.db.SetChatPassNoticeTTL(noticeChatID, 30, 42))
	mock.sendErr = errors.New("Forbidden: bot was kicked from the supergroup chat")

	b.sendPassNotice(tgbotapi.NewMessage(noticeChatID, "welcome"))

	assert.Empty(t, scheduledDeletions(t, b))
}

func TestDeleteDueMessages_DeletesWhatIsDueAndLeavesTheRest(t *testing.T) {
	b, mock := newTestBot(t)
	now := time.Now()
	require.NoError(t, b.db.ScheduleMessageDeletion(noticeChatID, 1, now.Add(-time.Second)))
	require.NoError(t, b.db.ScheduleMessageDeletion(noticeChatID, 2, now.Add(time.Minute)))

	b.deleteDueMessages(now)

	assert.Equal(t, []int{1}, deletedRegularMessages(mock))
	left := scheduledDeletions(t, b)
	require.Len(t, left, 1)
	assert.Equal(t, 2, left[0].MessageID)

	// Nothing is deleted twice.
	b.deleteDueMessages(now)
	assert.Equal(t, []int{1}, deletedRegularMessages(mock))
}

func TestDeleteDueMessages_FailureHandling(t *testing.T) {
	const overdue = time.Minute
	cases := map[string]struct {
		err      error
		overdue  time.Duration
		wantKept bool
	}{
		"message already gone":            {err: &tgbotapi.Error{Code: 400, Message: "Bad Request: message to delete not found"}, overdue: overdue},
		"bot lost its rights":             {err: &tgbotapi.Error{Code: 403, Message: "Forbidden: bot is not a member of the supergroup chat"}, overdue: overdue},
		"rate limited":                    {err: &tgbotapi.Error{Code: 429, Message: "Too Many Requests: retry after 3"}, overdue: overdue, wantKept: true},
		"telegram server error":           {err: &tgbotapi.Error{Code: 502, Message: "Bad Gateway"}, overdue: overdue, wantKept: true},
		"network failure":                 {err: errors.New("dial tcp: i/o timeout"), overdue: overdue, wantKept: true},
		"network failure, long overdue":   {err: errors.New("dial tcp: i/o timeout"), overdue: autoDeleteGiveUp + time.Second},
		"rate limited, long overdue":      {err: &tgbotapi.Error{Code: 429, Message: "Too Many Requests"}, overdue: autoDeleteGiveUp + time.Second},
		"network failure, just in window": {err: errors.New("EOF"), overdue: autoDeleteGiveUp - time.Second, wantKept: true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			b, mock := newTestBot(t)
			now := time.Now()
			require.NoError(t, b.db.ScheduleMessageDeletion(noticeChatID, 1, now.Add(-tc.overdue)))
			mock.requestErr = tc.err

			b.deleteDueMessages(now)

			assert.Equal(t, []int{1}, deletedRegularMessages(mock), "one attempt per tick")
			if tc.wantKept {
				assert.Len(t, scheduledDeletions(t, b), 1, "kept for the next tick")
			} else {
				assert.Empty(t, scheduledDeletions(t, b), "nothing more to gain from retrying")
			}
		})
	}
}

func TestDeleteDueMessages_OneFailureDoesNotBlockTheOthers(t *testing.T) {
	b, mock := newTestBot(t)
	now := time.Now()
	for id := 1; id <= 3; id++ {
		require.NoError(t, b.db.ScheduleMessageDeletion(noticeChatID, id, now.Add(-time.Duration(4-id)*time.Second)))
	}
	mock.requestErr = errors.New("dial tcp: i/o timeout")

	b.deleteDueMessages(now)

	assert.Equal(t, []int{1, 2, 3}, deletedRegularMessages(mock), "every due message gets its attempt")
}
