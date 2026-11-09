// Package seal encrypts upstream-private state so it can travel through a
// client that speaks another protocol and come back intact.
//
// A seal is ir.SealPrefix followed by base64url (no padding) of
// nonce || AES-256-GCM(plaintext). The plaintext binds the issuer, the kind
// and the verbatim data, so a client can neither read, alter nor re-attribute
// the state. Anything that fails to open is reported as an error; callers
// drop such state and never fail the request because of it.
package seal

import (
	"bytes"
	"compress/flate"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/initxy/plugrelay/pkg/ir"
)

// KeySize is the required master key length in bytes.
const KeySize = 32

const (
	hkdfInfo = "plugrelay seal v1"
	// compressAbove is the data size from which deflate is tried; it is kept
	// only when it actually shrinks the plaintext.
	compressAbove = 1024
	// maxPlain bounds decompression so a forged seal cannot be a zip bomb.
	// Only reachable with a valid key, but cheap to enforce.
	maxPlain = 16 << 20

	flagDeflate byte = 1
)

// ErrInvalid is returned for anything that is not an intact seal made with
// one of the Sealer's keys.
var ErrInvalid = errors.New("seal: invalid or foreign seal")

// Sealer seals and opens opaque state. It is safe for concurrent use.
type Sealer struct {
	keys []cipher.AEAD // keys[0] seals; all open
}

// New builds a Sealer from a 32-byte master key. Previous master keys, if
// given, are accepted by Open only, so seals issued before a rotation keep
// working. The AES key is derived from each master key with HKDF-SHA256;
// the master key itself never touches the cipher.
func New(masterKey []byte, previous ...[]byte) (*Sealer, error) {
	s := &Sealer{}
	for i, k := range append([][]byte{masterKey}, previous...) {
		a, err := derive(k)
		if err != nil {
			return nil, fmt.Errorf("seal: key %d: %w", i, err)
		}
		s.keys = append(s.keys, a)
	}
	return s, nil
}

func derive(master []byte) (cipher.AEAD, error) {
	if len(master) != KeySize {
		return nil, fmt.Errorf("master key must be %d bytes, got %d", KeySize, len(master))
	}
	sub, err := hkdf.Key(sha256.New, master, nil, hkdfInfo, KeySize)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(sub)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// Seal encrypts the state's issuer, kind and data. The Sealed flag of st is
// ignored.
func (s *Sealer) Seal(st ir.OpaqueState) (string, error) {
	if s == nil || len(s.keys) == 0 {
		return "", errors.New("seal: no key configured")
	}
	body := make([]byte, 0, len(st.Issuer)+len(st.Kind)+len(st.Data)+8)
	body = appendString(body, st.Issuer)
	body = appendString(body, st.Kind)
	body = append(body, st.Data...)

	flags := byte(0)
	if len(st.Data) > compressAbove {
		if z, ok := deflate(body); ok {
			body, flags = z, flagDeflate
		}
	}
	plain := append([]byte{flags}, body...)

	a := s.keys[0]
	nonce := make([]byte, a.NonceSize(), a.NonceSize()+len(plain)+a.Overhead())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("seal: nonce: %w", err)
	}
	out := a.Seal(nonce, nonce, plain, []byte(ir.SealPrefix))
	return ir.SealPrefix + base64.RawURLEncoding.EncodeToString(out), nil
}

// Open reverses Seal. The returned state has Sealed false and Data exactly
// as it was sealed.
func (s *Sealer) Open(sealed string) (ir.OpaqueState, error) {
	if s == nil || len(s.keys) == 0 {
		return ir.OpaqueState{}, ErrInvalid
	}
	enc, ok := strings.CutPrefix(sealed, ir.SealPrefix)
	if !ok {
		return ir.OpaqueState{}, ErrInvalid
	}
	raw, err := base64.RawURLEncoding.DecodeString(enc)
	if err != nil {
		return ir.OpaqueState{}, ErrInvalid
	}
	var plain []byte
	for _, a := range s.keys {
		ns := a.NonceSize()
		if len(raw) < ns+a.Overhead()+1 {
			return ir.OpaqueState{}, ErrInvalid
		}
		if p, err := a.Open(nil, raw[:ns], raw[ns:], []byte(ir.SealPrefix)); err == nil {
			plain = p
			break
		}
	}
	if plain == nil {
		return ir.OpaqueState{}, ErrInvalid
	}
	flags, body := plain[0], plain[1:]
	switch flags {
	case 0:
	case flagDeflate:
		if body, err = inflate(body); err != nil {
			return ir.OpaqueState{}, ErrInvalid
		}
	default:
		return ir.OpaqueState{}, ErrInvalid
	}
	issuer, body, ok := readString(body)
	if !ok {
		return ir.OpaqueState{}, ErrInvalid
	}
	kind, body, ok := readString(body)
	if !ok {
		return ir.OpaqueState{}, ErrInvalid
	}
	st := ir.OpaqueState{Issuer: issuer, Kind: kind}
	if len(body) > 0 {
		st.Data = json.RawMessage(bytes.Clone(body))
	}
	return st, nil
}

func appendString(b []byte, s string) []byte {
	b = binary.AppendUvarint(b, uint64(len(s)))
	return append(b, s...)
}

func readString(b []byte) (string, []byte, bool) {
	n, w := binary.Uvarint(b)
	if w <= 0 || n > uint64(len(b)-w) {
		return "", nil, false
	}
	b = b[w:]
	return string(b[:n]), b[n:], true
}

func deflate(b []byte) ([]byte, bool) {
	var buf bytes.Buffer
	w, err := flate.NewWriter(&buf, flate.BestCompression)
	if err != nil {
		return nil, false
	}
	if _, err := w.Write(b); err != nil {
		return nil, false
	}
	if err := w.Close(); err != nil {
		return nil, false
	}
	if buf.Len() >= len(b) {
		return nil, false
	}
	return buf.Bytes(), true
}

func inflate(b []byte) ([]byte, error) {
	r := flate.NewReader(bytes.NewReader(b))
	defer r.Close()
	out, err := io.ReadAll(io.LimitReader(r, maxPlain+1))
	if err != nil {
		return nil, err
	}
	if len(out) > maxPlain {
		return nil, ErrInvalid
	}
	return out, nil
}
