package admin

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/initxy/plugrelay/internal/secret"
)

// TOTP per RFC 6238 with the parameters every authenticator app supports:
// HMAC-SHA1, 6 digits, 30-second steps.
const (
	totpPeriod    = 30
	totpDigits    = 6
	totpSkew      = 1 // steps accepted either side of now
	totpSecretLen = 20
)

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

func newTOTPSecret() ([]byte, error) {
	b := make([]byte, totpSecretLen)
	_, err := rand.Read(b)
	return b, err
}

// hotp is RFC 4226 section 5.3.
func hotp(key []byte, counter int64) string {
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], uint64(counter))
	m := hmac.New(sha1.New, key)
	m.Write(msg[:])
	sum := m.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	v := binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff
	return fmt.Sprintf("%0*d", totpDigits, v%1_000_000)
}

func totpStep(t time.Time) int64 { return t.Unix() / totpPeriod }

// matchTOTP returns the time step code is valid for around now, or
// false. Whether that step was already used is the caller's check.
func matchTOTP(key []byte, code string, now time.Time) (int64, bool) {
	code = normalizeCode(code)
	if len(code) != totpDigits {
		return 0, false
	}
	cur := totpStep(now)
	found, step := 0, int64(0)
	for d := int64(-totpSkew); d <= totpSkew; d++ {
		if subtle.ConstantTimeCompare([]byte(hotp(key, cur+d)), []byte(code)) == 1 {
			found, step = 1, cur+d
		}
	}
	return step, found == 1
}

func normalizeCode(s string) string {
	return strings.Map(func(r rune) rune {
		if r == ' ' || r == '-' || r == '\t' {
			return -1
		}
		return r
	}, strings.TrimSpace(s))
}

// otpauthURI is the key-uri format authenticator apps scan.
func otpauthURI(issuer, account string, key []byte) string {
	label := url.PathEscape(issuer) + ":" + url.PathEscape(account)
	q := url.Values{"secret": {b32.EncodeToString(key)}, "issuer": {issuer}, "algorithm": {"SHA1"},
		"digits": {fmt.Sprint(totpDigits)}, "period": {fmt.Sprint(totpPeriod)}}
	return "otpauth://totp/" + label + "?" + q.Encode()
}

// Recovery codes: ten one-time codes of 12 base32 characters (60 bits),
// shown once and stored hashed with the user id.
const (
	recoveryCount = 10
	recoveryLen   = 12
)

const recoveryAlphabet = "abcdefghijkmnpqrstuvwxyz23456789"

func newRecoveryCodes() ([]string, error) {
	out := make([]string, recoveryCount)
	buf := make([]byte, recoveryLen)
	for i := range out {
		if _, err := rand.Read(buf); err != nil {
			return nil, err
		}
		var sb strings.Builder
		for j, b := range buf {
			if j > 0 && j%4 == 0 {
				sb.WriteByte('-')
			}
			sb.WriteByte(recoveryAlphabet[int(b)%len(recoveryAlphabet)])
		}
		out[i] = sb.String()
	}
	return out, nil
}

func hashRecoveryCode(userID, code string) string {
	return secret.HashKey(userID + ":" + strings.ToLower(normalizeCode(code)))
}

func hashRecoveryCodes(userID string, codes []string) []string {
	out := make([]string, len(codes))
	for i, c := range codes {
		out[i] = hashRecoveryCode(userID, c)
	}
	return out
}
