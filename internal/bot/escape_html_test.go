package bot

import (
	"testing"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/stretchr/testify/assert"
)

func TestEscapeHTML(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{"less-than", "<", "&lt;"},
		{"greater-than", ">", "&gt;"},
		{"ampersand", "&", "&amp;"},
		{"combined tags", "<b>&</b>", "&lt;b&gt;&amp;&lt;/b&gt;"},
		{"empty string", "", ""},
		{"no special chars", "hello world", "hello world"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, escapeHTML(tc.input))
		})
	}
}

func TestFormatUserMention(t *testing.T) {
	t.Run("uses @username when available", func(t *testing.T) {
		user := &tgbotapi.User{ID: 1, UserName: "alice_test", FirstName: "Alice"}
		assert.Equal(t, "@alice_test", formatUserMention(user))
	})

	t.Run("falls back to tg mention link", func(t *testing.T) {
		user := &tgbotapi.User{ID: 42, FirstName: "Alice", LastName: "<B>"}
		assert.Equal(t, `<a href="tg://user?id=42">Alice &lt;B&gt;</a>`, formatUserMention(user))
	})
}
