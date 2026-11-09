// Package secret holds plugrelay's cryptographic primitives: sealing
// credential secrets and secret settings under the master key, hashing
// platform keys and admin session tokens, and hashing console passwords.
//
// The master key is 32 random bytes, written base64 in relay.yaml, the
// PLUGRELAY_MASTER_KEY environment variable or a key file. It never seals
// data directly; a subkey is derived with HKDF so the same master key can
// later feed other purposes without key reuse.
package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strings"
)

// MasterKeySize is the length of the master key in bytes.
const MasterKeySize = 32

const (
	sealInfo   = "plugrelay secret v1"
	sealPrefix = "v1:"
)

// ErrDecrypt is returned when a ciphertext does not open under the box's key,
// was tampered with, or was sealed for different associated data.
var ErrDecrypt = errors.New("secret: decryption failed")

// GenerateMasterKey returns a new random master key, base64-encoded, as
// `plugrelay init` writes it.
func GenerateMasterKey() (string, error) {
	b := make([]byte, MasterKeySize)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("secret: generate master key: %w", err)
	}
	return base64.StdEncoding.EncodeToString(b), nil
}

// ParseMasterKey decodes a base64 (standard or URL alphabet, padded or not)
// master key and checks its length.
func ParseMasterKey(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(s); err == nil {
			if len(b) != MasterKeySize {
				return nil, fmt.Errorf("secret: master key is %d bytes, want %d", len(b), MasterKeySize)
			}
			return b, nil
		}
	}
	return nil, errors.New("secret: master key is not valid base64")
}

// LoadMasterKey returns the master key from an inline value, or else from
// the file at path. Exactly one source must be non-empty.
func LoadMasterKey(value, path string) ([]byte, error) {
	switch {
	case value != "" && path != "":
		return nil, errors.New("secret: master key given both inline and as a file")
	case value != "":
		return ParseMasterKey(value)
	case path != "":
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("secret: read master key file: %w", err)
		}
		return ParseMasterKey(string(data))
	default:
		return nil, errors.New("secret: no master key configured")
	}
}

// Box seals and opens secrets with AES-256-GCM under a subkey of the
// master key. It is safe for concurrent use.
type Box struct {
	aead cipher.AEAD
}

// NewBox derives the sealing subkey from a 32-byte master key.
func NewBox(master []byte) (*Box, error) {
	if len(master) != MasterKeySize {
		return nil, fmt.Errorf("secret: master key is %d bytes, want %d", len(master), MasterKeySize)
	}
	sub, err := hkdf.Key(sha256.New, master, nil, sealInfo, 32)
	if err != nil {
		return nil, fmt.Errorf("secret: derive subkey: %w", err)
	}
	block, err := aes.NewCipher(sub)
	if err != nil {
		return nil, fmt.Errorf("secret: cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("secret: gcm: %w", err)
	}
	return &Box{aead: aead}, nil
}

// Seal encrypts plaintext bound to ad (for example "credential:<id>"), so
// a ciphertext copied onto another row fails to open. The result is
// printable: "v1:" followed by base64 of nonce‖ciphertext.
func (b *Box) Seal(plaintext []byte, ad string) (string, error) {
	nonce := make([]byte, b.aead.NonceSize(), b.aead.NonceSize()+len(plaintext)+b.aead.Overhead())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("secret: nonce: %w", err)
	}
	out := b.aead.Seal(nonce, nonce, plaintext, []byte(ad))
	return sealPrefix + base64.RawStdEncoding.EncodeToString(out), nil
}

// Open decrypts a value produced by Seal with the same ad.
func (b *Box) Open(sealed, ad string) ([]byte, error) {
	rest, ok := strings.CutPrefix(sealed, sealPrefix)
	if !ok {
		return nil, fmt.Errorf("%w: unknown format", ErrDecrypt)
	}
	raw, err := base64.RawStdEncoding.DecodeString(rest)
	if err != nil || len(raw) < b.aead.NonceSize() {
		return nil, fmt.Errorf("%w: malformed", ErrDecrypt)
	}
	n := b.aead.NonceSize()
	pt, err := b.aead.Open(nil, raw[:n], raw[n:], []byte(ad))
	if err != nil {
		return nil, ErrDecrypt
	}
	return pt, nil
}

// IsSealed reports whether s looks like a value produced by Seal.
func IsSealed(s string) bool { return strings.HasPrefix(s, sealPrefix) }
