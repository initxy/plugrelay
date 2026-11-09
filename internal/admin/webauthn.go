package admin

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
)

// WebAuthn Level 2 relying-party checks, standard library only. The
// relay asks for attestation "none" and never trusts an attestation
// statement, so attStmt is not read: what is registered is the public
// key the browser handed over, bound to this RP ID and origin.

// COSE algorithms accepted.
const (
	coseES256 = -7
	coseEdDSA = -8
	coseRS256 = -257
)

var coseAlgs = []int64{coseES256, coseEdDSA, coseRS256}

// Authenticator data flags.
const (
	flagUP = 0x01 // user present
	flagUV = 0x04 // user verified
	flagAT = 0x40 // attested credential data included
	flagED = 0x80 // extensions included
)

// Size limits on the browser's response fields. Real values are a few
// hundred bytes (an RSA-4096 key and a 1023-byte credential id still fit
// authenticator data in 2 KB); attestation statements may carry a short
// certificate chain.
const (
	maxClientData  = 4 << 10
	maxAuthData    = 4 << 10
	maxAttestation = 16 << 10
	maxSignature   = 1 << 10
	// cborPrealloc caps what a declared array or map length reserves up
	// front; the decoder grows past it only as items actually parse.
	cborPrealloc = 16
)

var errWebAuthn = errors.New("webauthn")

func waErr(format string, a ...any) error {
	return fmt.Errorf("%w: %s", errWebAuthn, fmt.Sprintf(format, a...))
}

// b64url decodes base64url with or without padding.
func b64url(s string) ([]byte, error) {
	if b, err := base64.RawURLEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	return base64.URLEncoding.DecodeString(s)
}

func enc64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// clientData is CollectedClientData.
type clientData struct {
	Type        string `json:"type"`
	Challenge   string `json:"challenge"`
	Origin      string `json:"origin"`
	CrossOrigin bool   `json:"crossOrigin"`
}

// checkClientData parses clientDataJSON and checks type and origin; it
// returns the challenge for the caller to redeem.
func checkClientData(raw []byte, typ, origin string) (string, error) {
	if len(raw) > maxClientData {
		return "", waErr("clientDataJSON larger than %d bytes", maxClientData)
	}
	var cd clientData
	if err := json.Unmarshal(raw, &cd); err != nil {
		return "", waErr("clientDataJSON: %v", err)
	}
	if cd.Type != typ {
		return "", waErr("client data type %q, want %q", cd.Type, typ)
	}
	if cd.Origin != origin {
		return "", waErr("origin %q is not %q", cd.Origin, origin)
	}
	if cd.CrossOrigin {
		return "", waErr("cross-origin ceremony")
	}
	if cd.Challenge == "" {
		return "", waErr("no challenge")
	}
	return cd.Challenge, nil
}

// authData is parsed authenticator data.
type authData struct {
	raw       []byte
	rpIDHash  []byte
	flags     byte
	signCount uint32
	credID    []byte
	coseKey   []byte // raw CBOR of the credential public key
}

func parseAuthData(b []byte, wantAttested bool) (*authData, error) {
	if len(b) < 37 {
		return nil, waErr("authenticator data too short")
	}
	if len(b) > maxAuthData {
		return nil, waErr("authenticator data larger than %d bytes", maxAuthData)
	}
	ad := &authData{raw: b, rpIDHash: b[:32], flags: b[32], signCount: binary.BigEndian.Uint32(b[33:37])}
	rest := b[37:]
	if ad.flags&flagAT != 0 {
		if len(rest) < 18 {
			return nil, waErr("attested credential data too short")
		}
		n := int(binary.BigEndian.Uint16(rest[16:18]))
		rest = rest[18:]
		if n == 0 || n > 1023 || len(rest) < n {
			return nil, waErr("bad credential id length")
		}
		ad.credID, rest = rest[:n], rest[n:]
		_, tail, err := cborDecode(rest, 0)
		if err != nil {
			return nil, waErr("credential public key: %v", err)
		}
		ad.coseKey, rest = rest[:len(rest)-len(tail)], tail
	} else if wantAttested {
		return nil, waErr("no attested credential data")
	}
	if ad.flags&flagED != 0 {
		_, tail, err := cborDecode(rest, 0)
		if err != nil {
			return nil, waErr("extensions: %v", err)
		}
		rest = tail
	}
	if len(rest) != 0 {
		return nil, waErr("trailing bytes in authenticator data")
	}
	return ad, nil
}

func (ad *authData) check(rpID string, requireUV bool) error {
	want := sha256.Sum256([]byte(rpID))
	if subtle.ConstantTimeCompare(ad.rpIDHash, want[:]) != 1 {
		return waErr("RP ID hash does not match %q", rpID)
	}
	if ad.flags&flagUP == 0 {
		return waErr("user presence flag not set")
	}
	if requireUV && ad.flags&flagUV == 0 {
		return waErr("user verification required")
	}
	return nil
}

// registration is a verified attestation response.
type registration struct {
	credID    []byte
	coseKey   []byte
	alg       int64
	signCount uint32
}

// verifyRegistration checks an attestation response; redeem consumes the
// challenge and reports whether it was issued for this ceremony.
func verifyRegistration(clientDataJSON, attObj []byte, rpID, origin string, redeem func(challenge string) bool) (*registration, error) {
	ch, err := checkClientData(clientDataJSON, "webauthn.create", origin)
	if err != nil {
		return nil, err
	}
	if !redeem(ch) {
		return nil, waErr("unknown, expired or used challenge")
	}
	if len(attObj) > maxAttestation {
		return nil, waErr("attestation object larger than %d bytes", maxAttestation)
	}
	v, rest, err := cborDecode(attObj, 0)
	if err != nil || len(rest) != 0 {
		return nil, waErr("attestation object: %v", err)
	}
	m, ok := v.(map[any]any)
	if !ok {
		return nil, waErr("attestation object is not a map")
	}
	raw, ok := m["authData"].([]byte)
	if !ok {
		return nil, waErr("attestation object has no authData")
	}
	ad, err := parseAuthData(raw, true)
	if err != nil {
		return nil, err
	}
	if err := ad.check(rpID, false); err != nil {
		return nil, err
	}
	_, alg, err := parseCOSEKey(ad.coseKey)
	if err != nil {
		return nil, err
	}
	return &registration{credID: ad.credID, coseKey: ad.coseKey, alg: alg, signCount: ad.signCount}, nil
}

// assertion is a verified assertion response.
type assertion struct {
	signCount uint32
}

// verifyAssertion checks an assertion against a stored COSE key.
func verifyAssertion(clientDataJSON, authenticatorData, sig, coseKey []byte, rpID, origin string, requireUV bool,
	redeem func(challenge string) bool) (*assertion, error) {
	if len(sig) > maxSignature {
		return nil, waErr("signature larger than %d bytes", maxSignature)
	}
	ch, err := checkClientData(clientDataJSON, "webauthn.get", origin)
	if err != nil {
		return nil, err
	}
	if !redeem(ch) {
		return nil, waErr("unknown, expired or used challenge")
	}
	ad, err := parseAuthData(authenticatorData, false)
	if err != nil {
		return nil, err
	}
	if err := ad.check(rpID, requireUV); err != nil {
		return nil, err
	}
	pub, alg, err := parseCOSEKey(coseKey)
	if err != nil {
		return nil, err
	}
	cdh := sha256.Sum256(clientDataJSON)
	signed := append(append([]byte{}, authenticatorData...), cdh[:]...)
	if !verifySig(pub, alg, signed, sig) {
		return nil, waErr("bad signature")
	}
	return &assertion{signCount: ad.signCount}, nil
}

func verifySig(pub crypto.PublicKey, alg int64, msg, sig []byte) bool {
	switch alg {
	case coseES256:
		k, ok := pub.(*ecdsa.PublicKey)
		h := sha256.Sum256(msg)
		return ok && ecdsa.VerifyASN1(k, h[:], sig)
	case coseRS256:
		k, ok := pub.(*rsa.PublicKey)
		h := sha256.Sum256(msg)
		return ok && rsa.VerifyPKCS1v15(k, crypto.SHA256, h[:], sig) == nil
	case coseEdDSA:
		k, ok := pub.(ed25519.PublicKey)
		return ok && ed25519.Verify(k, msg, sig)
	}
	return false
}

// parseCOSEKey reads an EC2 P-256, RSA or OKP Ed25519 COSE_Key.
func parseCOSEKey(b []byte) (crypto.PublicKey, int64, error) {
	v, rest, err := cborDecode(b, 0)
	if err != nil || len(rest) != 0 {
		return nil, 0, waErr("COSE key: %v", err)
	}
	m, ok := v.(map[any]any)
	if !ok {
		return nil, 0, waErr("COSE key is not a map")
	}
	kty, _ := m[int64(1)].(int64)
	alg, _ := m[int64(3)].(int64)
	bytesAt := func(k int64) []byte { b, _ := m[k].([]byte); return b }
	switch {
	case kty == 2 && alg == coseES256:
		crv, _ := m[int64(-1)].(int64)
		x, y := bytesAt(-2), bytesAt(-3)
		if crv != 1 || len(x) != 32 || len(y) != 32 {
			return nil, 0, waErr("EC2 key is not P-256")
		}
		k, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), append(append([]byte{4}, x...), y...))
		if err != nil {
			return nil, 0, waErr("EC2 key: %v", err)
		}
		return k, alg, nil
	case kty == 3 && alg == coseRS256:
		n, e := bytesAt(-1), bytesAt(-2)
		if len(n) < 256 || len(e) == 0 || len(e) > 4 {
			return nil, 0, waErr("RSA key under 2048 bits or bad exponent")
		}
		ev := int(new(big.Int).SetBytes(e).Int64())
		if ev < 3 || ev%2 == 0 {
			return nil, 0, waErr("bad RSA exponent")
		}
		return &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: ev}, alg, nil
	case kty == 1 && alg == coseEdDSA:
		crv, _ := m[int64(-1)].(int64)
		x := bytesAt(-2)
		if crv != 6 || len(x) != ed25519.PublicKeySize {
			return nil, 0, waErr("OKP key is not Ed25519")
		}
		return ed25519.PublicKey(x), alg, nil
	}
	return nil, 0, waErr("unsupported key type %d / algorithm %d", kty, alg)
}

// ---------------------------------------------------------------- CBOR

// cborDecode decodes one definite-length CBOR item (RFC 8949): integers,
// byte and text strings, arrays, maps, false/true/null and floats. Map
// keys are int64 or string. It returns the bytes after the item.
func cborDecode(b []byte, depth int) (any, []byte, error) {
	if depth > 16 {
		return nil, nil, errors.New("cbor: nesting too deep")
	}
	if len(b) == 0 {
		return nil, nil, errors.New("cbor: unexpected end")
	}
	major, info := b[0]>>5, b[0]&0x1f
	b = b[1:]
	var arg uint64
	switch {
	case info < 24:
		arg = uint64(info)
	case info <= 27:
		n := 1 << (info - 24)
		if len(b) < n {
			return nil, nil, errors.New("cbor: unexpected end")
		}
		for _, c := range b[:n] {
			arg = arg<<8 | uint64(c)
		}
		b = b[n:]
	default:
		return nil, nil, errors.New("cbor: indefinite length or reserved value")
	}
	switch major {
	case 0:
		if arg > 1<<63-1 {
			return nil, nil, errors.New("cbor: integer overflow")
		}
		return int64(arg), b, nil
	case 1:
		if arg > 1<<63-1 {
			return nil, nil, errors.New("cbor: integer overflow")
		}
		return -1 - int64(arg), b, nil
	case 2, 3:
		if arg > uint64(len(b)) {
			return nil, nil, errors.New("cbor: string past end")
		}
		s := b[:arg]
		if major == 3 {
			return string(s), b[arg:], nil
		}
		return bytes.Clone(s), b[arg:], nil
	case 4:
		if arg > uint64(len(b)) {
			return nil, nil, errors.New("cbor: array past end")
		}
		out := make([]any, 0, min(arg, cborPrealloc))
		for range arg {
			v, rest, err := cborDecode(b, depth+1)
			if err != nil {
				return nil, nil, err
			}
			out, b = append(out, v), rest
		}
		return out, b, nil
	case 5:
		if arg > uint64(len(b)) {
			return nil, nil, errors.New("cbor: map past end")
		}
		out := make(map[any]any, min(arg, cborPrealloc))
		for range arg {
			k, rest, err := cborDecode(b, depth+1)
			if err != nil {
				return nil, nil, err
			}
			switch k.(type) {
			case int64, string:
			default:
				return nil, nil, errors.New("cbor: unsupported map key")
			}
			v, rest, err := cborDecode(rest, depth+1)
			if err != nil {
				return nil, nil, err
			}
			if _, dup := out[k]; dup {
				return nil, nil, errors.New("cbor: duplicate map key")
			}
			out[k], b = v, rest
		}
		return out, b, nil
	case 6: // tag: decode the tagged item
		return cborDecode(b, depth+1)
	case 7:
		switch info {
		case 20:
			return false, b, nil
		case 21:
			return true, b, nil
		case 22, 23:
			return nil, b, nil
		case 25, 26, 27:
			return float64(0), b, nil // value unused by WebAuthn
		}
		return nil, nil, errors.New("cbor: unsupported simple value")
	}
	return nil, nil, errors.New("cbor: unknown major type")
}
