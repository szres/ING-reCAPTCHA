package bot

import (
	"fmt"
	"log"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// nextDailyStatsFire returns the next 00:05 in loc strictly after now.
func nextDailyStatsFire(now time.Time, loc *time.Location) time.Time {
	n := now.In(loc)
	target := time.Date(n.Year(), n.Month(), n.Day(), 0, 5, 0, 0, loc)
	if !target.After(n) {
		target = target.AddDate(0, 0, 1)
	}
	return target
}

// dailyWindowFor returns the [start, end) window for the local day that ended
// at the given fire-time. e.g. fire=2026-05-10 00:05 → 2026-05-09 00:00 .. 2026-05-10 00:00.
func dailyWindowFor(fire time.Time, loc *time.Location) (start, end time.Time) {
	f := fire.In(loc)
	end = time.Date(f.Year(), f.Month(), f.Day(), 0, 0, 0, 0, loc)
	start = end.AddDate(0, 0, -1)
	return start, end
}

func (b *Bot) dailyStatsLoop() {
	defer b.wg.Done()

	if b.cfg.LeaderboardChannelID == 0 {
		log.Printf("[INFO] Daily stats: LEADERBOARD_CHANNEL_ID not set, scheduler disabled")
		return
	}

	loc, err := time.LoadLocation(b.cfg.DailyStatsTimezone)
	if err != nil {
		log.Printf("[ERROR] Daily stats: failed to load timezone %q, falling back to UTC: %v", b.cfg.DailyStatsTimezone, err)
		loc = time.UTC
	}

	for {
		next := nextDailyStatsFire(time.Now(), loc)
		wait := time.Until(next)
		log.Printf("[INFO] Daily stats: next post scheduled at %s (in %s)", next.Format(time.RFC3339), wait.Round(time.Second))

		timer := time.NewTimer(wait)
		select {
		case <-b.stopChan:
			timer.Stop()
			return
		case <-timer.C:
		}

		start, end := dailyWindowFor(time.Now(), loc)
		if err := b.postDailyStats(start, end, loc); err != nil {
			log.Printf("[ERROR] Daily stats: %v", err)
		}
	}
}

func (b *Bot) postDailyStats(start, end time.Time, loc *time.Location) error {
	stats, err := b.db.GetDailyStats(start.Unix(), end.Unix())
	if err != nil {
		return fmt.Errorf("get stats for %s..%s: %w", start.Format("2006-01-02"), end.Format("2006-01-02"), err)
	}

	lang := b.cfg.DefaultLanguage
	dateStr := start.In(loc).Format("2006-01-02")

	var sb strings.Builder
	sb.WriteString(b.i18n.T(lang, "daily_stats_title", dateStr))
	sb.WriteString("\n\n")

	if stats.Total == 0 {
		sb.WriteString(b.i18n.T(lang, "daily_stats_empty"))
	} else {
		total := float64(stats.Total)
		sb.WriteString(b.i18n.T(lang, "daily_stats_passed", stats.Passed, float64(stats.Passed)*100/total))
		sb.WriteString("\n")
		sb.WriteString(b.i18n.T(lang, "daily_stats_failed", stats.Failed, float64(stats.Failed)*100/total))
		sb.WriteString("\n")
		sb.WriteString(b.i18n.T(lang, "daily_stats_expired", stats.Expired, float64(stats.Expired)*100/total))
		sb.WriteString("\n")

		if stats.Passed > 0 {
			sb.WriteString("\n")
			sb.WriteString(b.i18n.T(lang, "daily_stats_times",
				float64(stats.AvgPassMs)/1000,
				float64(stats.MedianPassMs)/1000,
				float64(stats.P95PassMs)/1000))
			sb.WriteString("\n")
			sb.WriteString(b.i18n.T(lang, "daily_stats_fastest", float64(stats.FastestPassMs)/1000))
			sb.WriteString("\n")
		}

		sb.WriteString(b.i18n.T(lang, "daily_stats_unique_users", stats.UniqueUsers))
	}

	msg := tgbotapi.NewMessage(b.cfg.LeaderboardChannelID, sb.String())
	msg.ParseMode = "HTML"
	if _, err := b.api.Send(msg); err != nil {
		return fmt.Errorf("post to channel %d: %w", b.cfg.LeaderboardChannelID, err)
	}

	log.Printf("[INFO] Daily stats: posted summary for %s (passed=%d failed=%d expired=%d)",
		dateStr, stats.Passed, stats.Failed, stats.Expired)
	return nil
}
