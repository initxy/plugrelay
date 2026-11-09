package seal

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/initxy/plugrelay/pkg/ir"
)

func key(b byte) []byte { return bytes.Repeat([]byte{b}, KeySize) }

func mustNew(t *testing.T, k []byte, prev ...[]byte) *Sealer {
	t.Helper()
	s, err := New(k, prev...)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestRoundTrip(t *testing.T) {
	s := mustNew(t, key(1))
	big, _ := json.Marshal(strings.Repeat("abcdefgh", 2000))
	for _, st := range []ir.OpaqueState{
		{Issuer: "grp-a", Kind: "signature", Data: json.RawMessage(`"EqQBCkgIARABGAIiQ"`)},
		{Kind: "compaction", Data: json.RawMessage(`{"a":[1,2,3]}`)},
		{Issuer: "x", Kind: "k"},
		{Issuer: "grp-b", Kind: "encrypted_content", Data: big},
	} {
		sealed, err := s.Seal(st)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(sealed, ir.SealPrefix) {
			t.Fatalf("missing prefix: %q", sealed)
		}
		got, err := s.Open(sealed)
		if err != nil {
			t.Fatal(err)
		}
		if got.Issuer != st.Issuer || got.Kind != st.Kind || !bytes.Equal(got.Data, st.Data) || got.Sealed {
			t.Fatalf("round trip mismatch: %+v vs %+v", got, st)
		}
	}
}

func TestCompressionShrinks(t *testing.T) {
	s := mustNew(t, key(1))
	data, _ := json.Marshal(strings.Repeat("x", 8000))
	sealed, err := s.Seal(ir.OpaqueState{Kind: "k", Data: data})
	if err != nil {
		t.Fatal(err)
	}
	if len(sealed) > len(data)/4 {
		t.Fatalf("seal not compressed: %d bytes for %d", len(sealed), len(data))
	}
}

func TestNonceIsRandom(t *testing.T) {
	s := mustNew(t, key(1))
	st := ir.OpaqueState{Kind: "k", Data: json.RawMessage(`"v"`)}
	a, _ := s.Seal(st)
	b, _ := s.Seal(st)
	if a == b {
		t.Fatal("two seals of the same state are identical")
	}
}

func TestTamper(t *testing.T) {
	s := mustNew(t, key(1))
	sealed, _ := s.Seal(ir.OpaqueState{Issuer: "a", Kind: "k", Data: json.RawMessage(`"v"`)})
	raw, _ := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(sealed, ir.SealPrefix))
	for i := range raw {
		mut := bytes.Clone(raw)
		mut[i] ^= 0x01
		if _, err := s.Open(ir.SealPrefix + base64.RawURLEncoding.EncodeToString(mut)); err == nil {
			t.Fatalf("tampered byte %d accepted", i)
		}
	}
	if _, err := s.Open(sealed[:len(sealed)-3]); err == nil {
		t.Fatal("truncated seal accepted")
	}
}

func TestGarbageAndPrefix(t *testing.T) {
	s := mustNew(t, key(1))
	sealed, _ := s.Seal(ir.OpaqueState{Kind: "k"})
	for _, in := range []string{
		"", "prs1.", "prs1.!!!", "prs1.AAAA", "hello",
		strings.TrimPrefix(sealed, ir.SealPrefix), // valid body, no prefix
		"prs2." + strings.TrimPrefix(sealed, ir.SealPrefix),
	} {
		if _, err := s.Open(in); err == nil {
			t.Fatalf("accepted %q", in)
		}
	}
	var nilS *Sealer
	if _, err := nilS.Open(sealed); err == nil {
		t.Fatal("nil sealer opened")
	}
	if _, err := nilS.Seal(ir.OpaqueState{}); err == nil {
		t.Fatal("nil sealer sealed")
	}
}

func TestWrongKey(t *testing.T) {
	a := mustNew(t, key(1))
	b := mustNew(t, key(2))
	sealed, _ := a.Seal(ir.OpaqueState{Kind: "k", Data: json.RawMessage(`1`)})
	if _, err := b.Open(sealed); err == nil {
		t.Fatal("foreign key opened seal")
	}
}

func TestRotation(t *testing.T) {
	old := mustNew(t, key(1))
	sealedOld, _ := old.Seal(ir.OpaqueState{Issuer: "i", Kind: "k", Data: json.RawMessage(`"old"`)})
	rotated := mustNew(t, key(2), key(1))
	got, err := rotated.Open(sealedOld)
	if err != nil || string(got.Data) != `"old"` {
		t.Fatalf("rotated sealer cannot open old seal: %v", err)
	}
	sealedNew, _ := rotated.Seal(ir.OpaqueState{Kind: "k"})
	if _, err := old.Open(sealedNew); err == nil {
		t.Fatal("new seals must use the new key")
	}
	if _, err := mustNew(t, key(2)).Open(sealedNew); err != nil {
		t.Fatal(err)
	}
}

func TestBadKey(t *testing.T) {
	if _, err := New(make([]byte, 16)); err == nil {
		t.Fatal("short key accepted")
	}
	if _, err := New(key(1), make([]byte, 31)); err == nil {
		t.Fatal("short previous key accepted")
	}
}
