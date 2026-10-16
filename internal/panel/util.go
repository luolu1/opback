package panel

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"time"
)

// newToken returns a fresh random agent token.
func newToken() string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return hex.EncodeToString([]byte(time.Now().String()))
	}
	return hex.EncodeToString(b)
}

// constantTimeEqual compares two strings without leaking their length or
// content through timing.
func constantTimeEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
