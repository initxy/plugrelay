package secret

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/bcrypt"
)

// KeyPrefix starts every platform key.
const KeyPrefix = "pr_"

const (
	keyBodyLen = 32
	base62     = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
)

// GenerateKey returns a new platform key "pr_" + 32 base62 characters
// (about 190 bits of entropy).
func GenerateKey() (string, error) {
	body, err := randomBase62(keyBodyLen)
	if err != nil {
		return "", fmt.Errorf("secret: generate key: %w", err)
	}
	return KeyPrefix + body, nil
}

// GenerateToken returns a random opaque token (admin sessions, CSRF), 43
// base62 characters.
func GenerateToken() (string, error) {
	t, err := randomBase62(43)
	if err != nil {
		return "", fmt.Errorf("secret: generate token: %w", err)
	}
	return t, nil
}

func randomBase62(n int) (string, error) {
	// Rejection sampling keeps the distribution uniform: 248 = 4*62.
	out := make([]byte, 0, n)
	buf := make([]byte, n*2)
	for len(out) < n {
		if _, err := rand.Read(buf); err != nil {
			return "", err
		}
		for _, c := range buf {
			if c < 248 && len(out) < n {
				out = append(out, base62[c%62])
			}
		}
	}
	return string(out), nil
}

// ValidKeyFormat reports whether s has the shape of a platform key. It is a
// cheap pre-check before hashing and lookup.
func ValidKeyFormat(s string) bool {
	body, ok := strings.CutPrefix(s, KeyPrefix)
	if !ok || len(body) != keyBodyLen {
		return false
	}
	for i := 0; i < len(body); i++ {
		if strings.IndexByte(base62, body[i]) < 0 {
			return false
		}
	}
	return true
}

// ImportedKeyPrefix starts keys imported from new-api / one-api, which
// clients send as "sk-" plus the 48 characters those relays store.
const ImportedKeyPrefix = "sk-"

// ValidImportedKeyFormat reports whether s has the shape of an imported
// new-api / one-api key. Such keys authenticate only when an import stored
// their hash.
func ValidImportedKeyFormat(s string) bool {
	body, ok := strings.CutPrefix(s, ImportedKeyPrefix)
	if !ok || len(body) != 48 {
		return false
	}
	for i := 0; i < len(body); i++ {
		if strings.IndexByte(base62, body[i]) < 0 {
			return false
		}
	}
	return true
}

// HashKey returns the hex SHA-256 of a platform key or session token; only
// this hash is stored. Keys carry enough entropy that a slow hash adds
// nothing.
func HashKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// Last4 returns the last four characters of a key for display.
func Last4(key string) string {
	if len(key) <= 4 {
		return key
	}
	return key[len(key)-4:]
}

// EqualHash compares two hashes in constant time.
func EqualHash(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// ErrPasswordMismatch is returned by CheckPassword for a wrong password.
var ErrPasswordMismatch = errors.New("secret: password mismatch")

// HashPassword hashes a console password with bcrypt.
func HashPassword(password string) (string, error) {
	if password == "" {
		return "", errors.New("secret: empty password")
	}
	h, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", fmt.Errorf("secret: hash password: %w", err)
	}
	return string(h), nil
}

// CheckPassword verifies password against a bcrypt hash.
func CheckPassword(hash, password string) error {
	if hash == "" {
		return ErrPasswordMismatch
	}
	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)); err != nil {
		if errors.Is(err, bcrypt.ErrMismatchedHashAndPassword) {
			return ErrPasswordMismatch
		}
		return fmt.Errorf("secret: check password: %w", err)
	}
	return nil
}
