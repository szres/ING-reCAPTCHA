package miniapp

import (
	"crypto/rand"
	"encoding/base64"
)

// generateNonceBytes returns n bytes of crypto-random data, URL-safe
// base64-encoded with padding stripped. n=32 → 43 chars.
func generateNonceBytes(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
