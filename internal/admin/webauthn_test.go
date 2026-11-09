package admin

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"net/url"
	"runtime"
	"strings"
	"testing"
)

func FuzzCBORDecode(f *testing.F) {
	f.Add([]byte{0xa1, 0x01, 0x02})
	f.Add([]byte{0xa5, 0x01, 0x02, 0x03, 0x26, 0x20, 0x01, 0x21, 0x58, 0x20})
	f.Add([]byte{0x9f})
	f.Add([]byte{0xd8, 0x18, 0x41, 0x00})
	f.Add([]byte{0x9a, 0x00, 0x30, 0x00, 0x00, 0x00})
	f.Fuzz(func(t *testing.T, b []byte) {
		if _, rest, err := cborDecode(b, 0); err == nil && len(rest) > len(b) {
			t.Fatal("rest grew")
		}
		_, _, _ = parseCOSEKey(b)
	})
}

func FuzzParseAuthData(f *testing.F) {
	h := sha256.Sum256([]byte("example.com"))
	base := append(h[:], 0x45, 0, 0, 0, 1)
	base = append(base, make([]byte, 16)...)
	base = append(base, 0, 1, 0xaa, 0xa1, 0x01, 0x02)
	f.Add(base, true)
	f.Add(append(h[:], 0x85, 0, 0, 0, 1, 0xa0), false)
	f.Fuzz(func(t *testing.T, b []byte, want bool) {
		ad, err := parseAuthData(b, want)
		if err == nil {
			_ = ad.check("example.com", true)
			if ad.coseKey != nil {
				_, _, _ = parseCOSEKey(ad.coseKey)
			}
		}
	})
}

func FuzzVerifyRegistration(f *testing.F) {
	f.Add([]byte{0xa1, 0x68, 'a', 'u', 't', 'h', 'D', 'a', 't', 'a', 0x40})
	f.Fuzz(func(t *testing.T, att []byte) {
		cd := []byte(`{"type":"webauthn.create","challenge":"x","origin":"https://example.com"}`)
		_, _ = verifyRegistration(cd, att, "example.com", "https://example.com", func(string) bool { return true })
	})
}

// allocMB runs fn and returns how many MB it allocated.
func allocMB(fn func()) uint64 {
	var m0, m1 runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&m0)
	fn()
	runtime.ReadMemStats(&m1)
	return (m1.TotalAlloc - m0.TotalAlloc) >> 20
}

// TestCBORDeclaredLengthNotPreallocated: an array or map claiming millions
// of items reserves a small fixed amount, not the declared length; the
// first bad item ends decoding. (Valid items cost what they parse to, which
// the field size limits bound.)
func TestCBORDeclaredLengthNotPreallocated(t *testing.T) {
	n := 3 << 20
	var l [4]byte
	binary.BigEndian.PutUint32(l[:], uint32(n))
	for _, head := range []byte{0x9a, 0xba} { // array, map with 4-byte length
		b := append(append([]byte{head}, l[:]...), bytes.Repeat([]byte{0xff}, n)...)
		var err error
		if mb := allocMB(func() { _, _, err = cborDecode(b, 0) }); mb > 8 {
			t.Fatalf("head %#x: %d MB allocated for a %d KB input", head, mb, len(b)>>10)
		}
		if err == nil {
			t.Fatalf("head %#x: truncated item decoded", head)
		}
	}
}

// TestWebAuthnInputSizeLimits: oversized fields fail before any parsing.
func TestWebAuthnInputSizeLimits(t *testing.T) {
	h := sha256.Sum256([]byte("example.com"))
	big := append(append(h[:], 0x85, 0, 0, 0, 1, 0xba), make([]byte, 3<<20)...)
	if mb := allocMB(func() {
		if _, err := parseAuthData(big, false); err == nil || !strings.Contains(err.Error(), "larger than") {
			t.Errorf("auth data: %v", err)
		}
	}); mb > 8 {
		t.Fatalf("auth data: %d MB allocated", mb)
	}
	ok := func(string) bool { return true }
	cd := []byte(`{"type":"webauthn.create","challenge":"x","origin":"https://example.com"}`)
	if _, err := verifyRegistration(cd, make([]byte, maxAttestation+1), "example.com", "https://example.com", ok); err == nil ||
		!strings.Contains(err.Error(), "larger than") {
		t.Errorf("attestation: %v", err)
	}
	longCD := append([]byte(`{"type":"webauthn.get","challenge":"x","origin":"https://example.com","pad":"`),
		append(bytes.Repeat([]byte("a"), maxClientData), '"', '}')...)
	if _, err := verifyAssertion(longCD, nil, nil, nil, "example.com", "https://example.com", false, ok); err == nil ||
		!strings.Contains(err.Error(), "larger than") {
		t.Errorf("client data: %v", err)
	}
	if _, err := verifyAssertion(cd, nil, make([]byte, maxSignature+1), nil, "example.com", "https://example.com", false, ok); err == nil ||
		!strings.Contains(err.Error(), "larger than") {
		t.Errorf("signature: %v", err)
	}
}

func TestWebOrigin(t *testing.T) {
	for in, want := range map[string]string{
		"https://Relay.Example.com:443/x": "https://relay.example.com",
		"http://relay:80":                 "http://relay",
		"https://relay:8443":              "https://relay:8443",
		"http://[::1]:80/":                "http://[::1]",
		"https://[2001:db8::1]:9443":      "https://[2001:db8::1]:9443",
	} {
		u, _ := url.Parse(in)
		if got := webOrigin(u); got != want {
			t.Errorf("webOrigin(%q) = %q, want %q", in, got, want)
		}
	}
}
