package panel

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base32"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// TOTP parameters (RFC 6238 with the common authenticator defaults).
const (
	totpDigits = 6
	totpPeriod = 30 // seconds
	// recoveryCodeCount is how many one-time codes are issued alongside 2FA.
	recoveryCodeCount = 8
)

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// newTOTPSecret returns a fresh 160-bit base32 seed (no padding).
func newTOTPSecret() string {
	b := make([]byte, 20)
	if _, err := rand.Read(b); err != nil {
		return ""
	}
	return b32.EncodeToString(b)
}

// normalizeTOTPSecret folds case and strips spaces users may introduce.
func normalizeTOTPSecret(secret string) string {
	return strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(secret), " ", ""))
}

// totpAt computes the TOTP value for secret at time t.
func totpAt(secret string, t time.Time) string {
	key, err := b32.DecodeString(normalizeTOTPSecret(secret))
	if err != nil {
		return ""
	}
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], uint64(t.Unix()/totpPeriod))
	mac := hmac.New(sha1.New, key)
	mac.Write(buf[:])
	sum := mac.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	code := (uint32(sum[off]&0x7f) << 24) |
		(uint32(sum[off+1]) << 16) |
		(uint32(sum[off+2]) << 8) |
		uint32(sum[off+3])
	return fmt.Sprintf("%0*d", totpDigits, code%1_000_000)
}

// verifyTOTP accepts a code from the current step or its immediate
// neighbours, tolerating small clock skew.
func verifyTOTP(secret, code string, t time.Time) bool {
	code = strings.TrimSpace(code)
	if len(code) != totpDigits || secret == "" {
		return false
	}
	for _, d := range []int{-1, 0, 1} {
		want := totpAt(secret, t.Add(time.Duration(d)*totpPeriod*time.Second))
		if want != "" && hmac.Equal([]byte(want), []byte(code)) {
			return true
		}
	}
	return false
}

// otpauthURL renders the otpauth:// URI that authenticator apps scan or parse.
func otpauthURL(issuer, account, secret string) string {
	label := url.PathEscape(issuer + ":" + account)
	q := url.Values{}
	q.Set("secret", normalizeTOTPSecret(secret))
	q.Set("issuer", issuer)
	q.Set("algorithm", "SHA1")
	q.Set("digits", fmt.Sprintf("%d", totpDigits))
	q.Set("period", fmt.Sprintf("%d", totpPeriod))
	return "otpauth://totp/" + label + "?" + q.Encode()
}

// newRecoveryCodes returns n human-friendly one-time codes.
func newRecoveryCodes(n int) []string {
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		b := make([]byte, 8)
		_, _ = rand.Read(b)
		raw := strings.ToUpper(hex.EncodeToString(b))[:10]
		out = append(out, raw[:5]+"-"+raw[5:])
	}
	return out
}
