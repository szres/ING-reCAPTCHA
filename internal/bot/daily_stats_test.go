package bot

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNextDailyStatsFire(t *testing.T) {
	loc, err := time.LoadLocation("Asia/Shanghai")
	require.NoError(t, err)

	t.Run("before 00:05 fires same day at 00:05", func(t *testing.T) {
		now := time.Date(2026, 5, 10, 0, 0, 0, 0, loc)
		want := time.Date(2026, 5, 10, 0, 5, 0, 0, loc)
		assert.True(t, nextDailyStatsFire(now, loc).Equal(want))
	})

	t.Run("after 00:05 fires next day at 00:05", func(t *testing.T) {
		now := time.Date(2026, 5, 10, 0, 5, 0, 0, loc) // exactly at; should roll
		want := time.Date(2026, 5, 11, 0, 5, 0, 0, loc)
		assert.True(t, nextDailyStatsFire(now, loc).Equal(want))
	})

	t.Run("midday fires next day at 00:05", func(t *testing.T) {
		now := time.Date(2026, 5, 10, 14, 30, 0, 0, loc)
		want := time.Date(2026, 5, 11, 0, 5, 0, 0, loc)
		assert.True(t, nextDailyStatsFire(now, loc).Equal(want))
	})

	t.Run("works across timezones (now in UTC, fire in Shanghai)", func(t *testing.T) {
		// Shanghai is UTC+8 (no DST). 16:00 UTC = 00:00 next-day Shanghai.
		now := time.Date(2026, 5, 10, 16, 0, 0, 0, time.UTC)
		want := time.Date(2026, 5, 11, 0, 5, 0, 0, loc)
		assert.True(t, nextDailyStatsFire(now, loc).Equal(want))
	})
}

func TestDailyWindowFor(t *testing.T) {
	loc, err := time.LoadLocation("Asia/Shanghai")
	require.NoError(t, err)

	fire := time.Date(2026, 5, 10, 0, 5, 0, 0, loc)
	start, end := dailyWindowFor(fire, loc)
	assert.True(t, start.Equal(time.Date(2026, 5, 9, 0, 0, 0, 0, loc)))
	assert.True(t, end.Equal(time.Date(2026, 5, 10, 0, 0, 0, 0, loc)))
}
