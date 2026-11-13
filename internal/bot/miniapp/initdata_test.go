package miniapp

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"testing"
	"time"
)

const testBotToken = "1234567890:AAEhBP0av28EtcRtmIxOHs90daDoZWzbBog"

// signInitData reproduces Telegram's signing algorithm and returns the full
// query-string form of initData. Tests use it to mint valid payloads.
//
// This duplicates the validator's algorithm by design: if either side
// drifts, the round-trip tests will fail loudly.
func signInitData(t *testing.T, botToken string, fields map[string]string) string {
	t.Helper()

	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(fields[k])
	}

	mac := hmac.New(sha256.New, []byte("WebAppData"))
	mac.Write([]byte(botToken))
	secret := mac.Sum(nil)

	mac = hmac.New(sha256.New, secret)
	mac.Write([]byte(b.String()))
	hash := hex.EncodeToString(mac.Sum(nil))

	values := url.Values{}
	for k, v := range fields {
		values.Set(k, v)
	}
	values.Set("hash", hash)
	return values.Encode()
}

func TestValidateInitData_HappyPath(t *testing.T) {
	now := time.Unix(1_700_000_500, 0)
	fields := map[string]string{
		"auth_date": "1700000400",
		"query_id":  "AAH123",
		"user":      `{"id":12345,"first_name":"Test"}`,
	}
	initData := signInitData(t, testBotToken, fields)

	uid, authDate, err := ValidateInitData(initData, testBotToken, now)
	if err != nil {
		t.Fatalf("expected ok, got %v", err)
	}
	if uid != 12345 {
		t.Errorf("user_id mismatch: got %d", uid)
	}
	if authDate != 1700000400 {
		t.Errorf("auth_date mismatch: got %d", authDate)
	}
}

func TestValidateInitData_RejectsTamperedHash(t *testing.T) {
	now := time.Unix(1_700_000_500, 0)
	fields := map[string]string{
		"auth_date": "1700000400",
		"user":      `{"id":12345}`,
	}
	initData := signInitData(t, testBotToken, fields)

	// Flip a bit in the hash.
	tampered := strings.Replace(initData, "hash=", "hash=0", 1)
	if _, _, err := ValidateInitData(tampered, testBotToken, now); err != ErrInvalidInitData {
		t.Fatalf("expected ErrInvalidInitData, got %v", err)
	}
}

func TestValidateInitData_RejectsTamperedField(t *testing.T) {
	now := time.Unix(1_700_000_500, 0)
	fields := map[string]string{
		"auth_date": "1700000400",
		"user":      `{"id":12345}`,
	}
	initData := signInitData(t, testBotToken, fields)

	// Change user_id but keep the original hash.
	tampered := strings.Replace(initData, "12345", "99999", 1)
	if _, _, err := ValidateInitData(tampered, testBotToken, now); err != ErrInvalidInitData {
		t.Fatalf("expected ErrInvalidInitData, got %v", err)
	}
}

func TestValidateInitData_RejectsWrongBotToken(t *testing.T) {
	now := time.Unix(1_700_000_500, 0)
	fields := map[string]string{
		"auth_date": "1700000400",
		"user":      `{"id":12345}`,
	}
	initData := signInitData(t, testBotToken, fields)

	if _, _, err := ValidateInitData(initData, "wrong:token", now); err != ErrInvalidInitData {
		t.Fatalf("expected ErrInvalidInitData with wrong token, got %v", err)
	}
}

func TestValidateInitData_RejectsStale(t *testing.T) {
	authDate := int64(1_700_000_000)
	// 11 minutes later — outside the 10-minute window.
	now := time.Unix(authDate+11*60, 0)
	fields := map[string]string{
		"auth_date": fmt.Sprintf("%d", authDate),
		"user":      `{"id":12345}`,
	}
	initData := signInitData(t, testBotToken, fields)

	if _, _, err := ValidateInitData(initData, testBotToken, now); err != ErrStaleInitData {
		t.Fatalf("expected ErrStaleInitData, got %v", err)
	}
}

func TestValidateInitData_AllowsSmallClockSkew(t *testing.T) {
	authDate := int64(1_700_000_000)
	// Server thinks it's 30 seconds BEFORE the client claimed timestamp;
	// minor skew should still validate.
	now := time.Unix(authDate-30, 0)
	fields := map[string]string{
		"auth_date": fmt.Sprintf("%d", authDate),
		"user":      `{"id":12345}`,
	}
	initData := signInitData(t, testBotToken, fields)

	if _, _, err := ValidateInitData(initData, testBotToken, now); err != nil {
		t.Fatalf("expected accept with small skew, got %v", err)
	}
}

func TestValidateInitData_RejectsMissingHash(t *testing.T) {
	now := time.Now()
	raw := "auth_date=1700000400&user=%7B%22id%22%3A1%7D"
	if _, _, err := ValidateInitData(raw, testBotToken, now); err != ErrInvalidInitData {
		t.Fatalf("expected ErrInvalidInitData, got %v", err)
	}
}

func TestValidateInitData_RejectsMissingInitData(t *testing.T) {
	if _, _, err := ValidateInitData("", testBotToken, time.Now()); err != ErrMissingInitData {
		t.Fatalf("expected ErrMissingInitData, got %v", err)
	}
}

func TestParseUserID_BoundaryCases(t *testing.T) {
	cases := []struct {
		in      string
		want    int64
		wantErr bool
	}{
		{`{"id":12345}`, 12345, false},
		{`{"id":-100}`, -100, false},
		{`{"first_name":"A","id":1,"last_name":"B"}`, 1, false},
		{`{"id": 42 }`, 42, false},
		{`{}`, 0, true},
		{`{"id":"oops"}`, 0, true},
	}
	for _, c := range cases {
		got, err := parseUserID(c.in)
		if (err != nil) != c.wantErr {
			t.Errorf("parseUserID(%q): err=%v wantErr=%v", c.in, err, c.wantErr)
			continue
		}
		if !c.wantErr && got != c.want {
			t.Errorf("parseUserID(%q): got %d want %d", c.in, got, c.want)
		}
	}
}
