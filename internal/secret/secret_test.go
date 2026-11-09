package secret

import (
	"bytes"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testBox(t *testing.T) *Box {
	t.Helper()
	k, err := GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	mk, err := ParseMasterKey(k)
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewBox(mk)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestSealOpen(t *testing.T) {
	b := testBox(t)
	s, err := b.Seal([]byte("sk-abc"), "credential:1")
	if err != nil {
		t.Fatal(err)
	}
	if !IsSealed(s) {
		t.Fatalf("not sealed: %q", s)
	}
	pt, err := b.Open(s, "credential:1")
	if err != nil || string(pt) != "sk-abc" {
		t.Fatalf("open = %q, %v", pt, err)
	}
	if _, err := b.Open(s, "credential:2"); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("wrong ad: %v", err)
	}
	if _, err := testBox(t).Open(s, "credential:1"); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("wrong key: %v", err)
	}
	s2, _ := b.Seal([]byte("sk-abc"), "credential:1")
	if s == s2 {
		t.Fatal("nonce reused")
	}
	raw, err := base64.RawStdEncoding.DecodeString(strings.TrimPrefix(s, sealPrefix))
	if err != nil {
		t.Fatal(err)
	}
	for i := range raw {
		mut := bytes.Clone(raw)
		mut[i] ^= 0x01
		if _, err := b.Open(sealPrefix+base64.RawStdEncoding.EncodeToString(mut), "credential:1"); !errors.Is(err, ErrDecrypt) {
			t.Fatalf("tampered byte %d: %v", i, err)
		}
	}
}

func TestLoadMasterKey(t *testing.T) {
	k, _ := GenerateMasterKey()
	p := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(p, []byte(k+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	a, err := LoadMasterKey("", p)
	if err != nil {
		t.Fatal(err)
	}
	b, err := LoadMasterKey(k, "")
	if err != nil || string(a) != string(b) {
		t.Fatalf("mismatch: %v", err)
	}
	if _, err := LoadMasterKey(k, p); err == nil {
		t.Fatal("both sources accepted")
	}
	if _, err := LoadMasterKey("", ""); err == nil {
		t.Fatal("no source accepted")
	}
	if _, err := ParseMasterKey("c2hvcnQ="); err == nil {
		t.Fatal("short key accepted")
	}
}

func TestKeys(t *testing.T) {
	k, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	if !ValidKeyFormat(k) || len(k) != 35 {
		t.Fatalf("bad key %q", k)
	}
	for _, bad := range []string{"", "pr_", "sk_" + k[3:], k + "x", k[:34] + "-"} {
		if ValidKeyFormat(bad) {
			t.Errorf("accepted %q", bad)
		}
	}
	if h := HashKey(k); h != HashKey(k) || !EqualHash(HashKey(k), HashKey(k)) || EqualHash(HashKey(k), HashKey(k+"1")) {
		t.Fatal("hash compare")
	}
	if Last4(k) != k[31:] {
		t.Fatal("last4")
	}
	k2, _ := GenerateKey()
	if k == k2 {
		t.Fatal("duplicate key")
	}
}

func TestPassword(t *testing.T) {
	h, err := HashPassword("hunter2")
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckPassword(h, "hunter2"); err != nil {
		t.Fatal(err)
	}
	if err := CheckPassword(h, "nope"); !errors.Is(err, ErrPasswordMismatch) {
		t.Fatal(err)
	}
}
