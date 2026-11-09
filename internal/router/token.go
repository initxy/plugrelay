package router

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"strings"
	"time"

	"github.com/initxy/plugrelay/pkg/plugin"
)

const turnTokenMAC = 16

// IssueTurnToken returns a signed token naming the candidate, for clients
// that echo a per-turn header back (it pins the next request of the same turn
// to the same credential).
func (r *Router) IssueTurnToken(c plugin.Candidate) string {
	key := Key(c)
	buf := make([]byte, 8+len(key))
	binary.BigEndian.PutUint64(buf, uint64(r.now().Unix()))
	copy(buf[8:], key)
	return base64.RawURLEncoding.EncodeToString(buf) + "." + base64.RawURLEncoding.EncodeToString(r.mac(buf))
}

// ParseTurnToken verifies a token from IssueTurnToken and returns the
// candidate key it names. Tokens older than Options.TurnTokenMaxAge are
// rejected.
func (r *Router) ParseTurnToken(s string) (candidateKey string, ok bool) {
	body, sig, found := strings.Cut(s, ".")
	if !found {
		return "", false
	}
	buf, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil || len(buf) < 9 {
		return "", false
	}
	mac, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil || !hmac.Equal(mac, r.mac(buf)) {
		return "", false
	}
	issued := time.Unix(int64(binary.BigEndian.Uint64(buf)), 0)
	if age := r.now().Sub(issued); age > r.opt.TurnTokenMaxAge || age < -time.Minute {
		return "", false
	}
	return string(buf[8:]), true
}

func (r *Router) mac(b []byte) []byte {
	h := hmac.New(sha256.New, r.opt.Secret)
	h.Write([]byte("plugrelay-turn\x00"))
	h.Write(b)
	return h.Sum(nil)[:turnTokenMAC]
}
