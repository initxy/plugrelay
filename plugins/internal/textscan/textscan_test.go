package textscan

import (
	"fmt"
	"math"
	"slices"
	"strings"
	"testing"
	"time"
)

func rules(names ...string) []Rule {
	var out []Rule
	for _, n := range names {
		r, ok := Builtin(n)
		if !ok {
			panic(n)
		}
		out = append(out, r)
	}
	return out
}

func found(rs []Rule, s string) []string {
	var out []string
	for _, m := range Find(rs, s) {
		out = append(out, m.Rule+"="+s[m.Start:m.End])
	}
	return out
}

func TestDetectors(t *testing.T) {
	tests := []struct {
		det  string
		in   string
		want []string
	}{
		{Email, "write to a.b+c@example.co.uk now", []string{"email=a.b+c@example.co.uk"}},
		{Email, "user at example dot com", nil},
		{Phone, "call +1 415-555-0123 or 13812345678", []string{"phone=+1 415-555-0123", "phone=13812345678"}},
		{Phone, "office (415) 555-0123 or 415.555.0123", []string{"phone=(415) 555-0123", "phone=415.555.0123"}},
		{Phone, "order 4155550123 shipped in 2026", nil},
		{CreditCard, "card 4111 1111 1111 1111 exp", []string{"credit_card=4111 1111 1111 1111"}},
		{CreditCard, "amex 3782-822463-10005", []string{"credit_card=3782-822463-10005"}},
		{CreditCard, "visa 4111111111111111", []string{"credit_card=4111111111111111"}},
		{CreditCard, "not luhn 4111 1111 1111 1112", nil},
		{CreditCard, "timestamp 1730851200000 and id 1234567890123", nil},
		{CreditCard, "mixed 4111-1111 1111-1111", nil},
		{IBAN, "pay GB82 WEST 1234 5698 7654 32 today", []string{"iban=GB82 WEST 1234 5698 7654 32"}},
		{IBAN, "DE89370400440532013000", []string{"iban=DE89370400440532013000"}},
		{IBAN, "bad GB82WEST12345698765433", nil},
		{CNID, "身份证 11010519491231002X 号", []string{"cn_id=11010519491231002X"}},
		{CNID, "wrong check 110105194912310021", nil},
		{CNID, "bad date 110105194913310028", nil},
		{USSSN, "ssn 123-45-6789", []string{"us_ssn=123-45-6789"}},
		{USSSN, "never issued 000-12-3456 666-12-3456 900-12-3456 123-00-4567 123-45-0000", nil},
		{Secret, "key sk-abcdefghijklmnopqrstuv end", []string{"secret=sk-abcdefghijklmnopqrstuv"}},
		{Secret, "aws AKIAABCDEFGHIJKLMNOP", []string{"secret=AKIAABCDEFGHIJKLMNOP"}},
		{Secret, "google AIzaSyA1234567890abcdefghijklmnopqrstuv", []string{"secret=AIzaSyA1234567890abcdefghijklmnopqrstuv"}},
		{Secret, "hash 0123456789abcdef0123456789abcdef and sk-short", nil},
		{PrivateKey, "x -----BEGIN RSA PRIVATE KEY-----\nMIIEow==\n-----END RSA PRIVATE KEY----- y",
			[]string{"private_key=-----BEGIN RSA PRIVATE KEY-----\nMIIEow==\n-----END RSA PRIVATE KEY-----"}},
		{PrivateKey, "-----BEGIN PRIVATE KEY-----\nMIIE", []string{"private_key=-----BEGIN PRIVATE KEY-----"}},
		{PrivateKey, "-----BEGIN PUBLIC KEY-----", nil},
	}
	for _, tt := range tests {
		if got := found(rules(tt.det), tt.in); !slices.Equal(got, tt.want) {
			t.Errorf("%s in %q:\n got %q\nwant %q", tt.det, tt.in, got, tt.want)
		}
	}
}

func TestMaskIdempotent(t *testing.T) {
	rs := rules(Detectors...)
	kw, _ := Keywords("keyword", []string{"Project X", " "})
	rs = append(rs, kw)
	in := "mail a@b.io about project x, card 4111111111111111"
	once := Mask(rs, in)
	if want := "mail [REDACTED:email] about [REDACTED:keyword], card [REDACTED:credit_card]"; once != want {
		t.Fatalf("got %q", once)
	}
	if twice := Mask(rs, once); twice != once {
		t.Fatalf("second mask changed text: %q", twice)
	}
	// A number failing its checksum is left alone by Mask too.
	if got := Mask(rules(CreditCard), "4111111111111112"); got != "4111111111111112" {
		t.Fatalf("invalid card masked: %q", got)
	}
}

func TestKeywordsAndPatterns(t *testing.T) {
	if _, ok := Keywords("keyword", []string{"", "  "}); ok {
		t.Fatal("empty keyword list made a rule")
	}
	kw, _ := Keywords("keyword", []string{"a.b", "机密"})
	if got := found([]Rule{kw}, "aXb A.B 这是机密"); !slices.Equal(got, []string{"keyword=A.B", "keyword=机密"}) {
		t.Fatalf("keywords are literal and case-insensitive: %q", got)
	}
	if _, err := Pattern("pattern", "("); err == nil {
		t.Fatal("invalid pattern compiled")
	}
	p, _ := Pattern("pattern", `order-\d+`)
	if got := found([]Rule{p}, "order-12 order-x"); !slices.Equal(got, []string{"pattern=order-12"}) {
		t.Fatal(got)
	}
}

func TestUserTextPaths(t *testing.T) {
	tests := []struct{ proto, body string }{
		{"anthropic", `{"messages":[{"role":"user","content":"a"},{"role":"assistant","content":"b"},{"role":"user","content":[{"type":"text","text":"c"},{"type":"image"}]}]}`},
		{"openai-responses", `{"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"a"}]},{"type":"function_call_output","output":"b"}]}`},
		{"gemini", `{"contents":[{"role":"user","parts":[{"text":"a"}]},{"role":"model","parts":[{"text":"b"}]}]}`},
		{"openai-completions", `{"prompt":"a","suffix":"b"}`},
	}
	want := map[string][]string{
		"anthropic":          {"messages.0.content", "messages.2.content.0.text"},
		"openai-responses":   {"input.0.content.0.text"},
		"gemini":             {"contents.0.parts.0.text"},
		"openai-completions": {"prompt", "suffix"},
	}
	for _, tt := range tests {
		if got := UserTextPaths([]byte(tt.body), tt.proto); !slices.Equal(got, want[tt.proto]) {
			t.Errorf("%s: %v", tt.proto, got)
		}
	}
}

func TestHoldBack(t *testing.T) {
	kw, _ := Keywords("k", []string{"forbidden-word", "secret"})
	for s, want := range map[string]int{"say ": 0, "say forbid": 6, "say FORB": 4, "say sec": 3, "say forbidden-word ok": 0, "x forb\u200b": 7, "s": 1} {
		if n, ok := HoldBack([]Rule{kw}, s); !ok || n != want {
			t.Errorf("HoldBack(%q) = %d, %v; want %d", s, n, ok, want)
		}
	}
	// Case folding as the matcher does it: the Kelvin sign is a k, the
	// long s an s, though their bytes differ in length.
	kill, _ := Keywords("k", []string{"kill", "mass"})
	for s, want := range map[string]int{"we Ki": 4, "we K": 3, "the maſ": 4, "we Ki": 2} {
		if n, ok := HoldBack([]Rule{kill}, s); !ok || n != want {
			t.Errorf("HoldBack(%q) = %d, %v; want %d", s, n, ok, want)
		}
	}
	if m := Find([]Rule{kill}, "we \u212Aill it"); len(m) != 1 {
		t.Errorf("Kelvin sign not matched: %v", m)
	}
	email, _ := Builtin(Email)
	if n, ok := HoldBack([]Rule{kw, email}, "abc"); ok || n != 3 {
		t.Errorf("detector: %d %v", n, ok)
	}
}

// TestKeywordsCost: a thousand keywords over a megabyte stay fast, zero-width
// folding included.
// Matching a large keyword list is linear in the text: eight times the
// text costs about eight times as much, never the 64 of a quadratic scan.
// Both sizes are timed in the same run, the fastest of a few tries each,
// so a loaded machine slows them alike.
func TestKeywordsCost(t *testing.T) {
	var words []string
	for i := range 1000 {
		words = append(words, fmt.Sprintf("word%dxyz", i))
	}
	r, _ := Keywords("k", words)
	cost := func(repeat int) time.Duration {
		text := strings.Repeat("word1 word2 abc ", repeat) + "word999xyz"
		best := time.Duration(math.MaxInt64)
		for range 3 {
			start := time.Now()
			if m := Find([]Rule{r}, text); len(m) != 1 {
				t.Fatalf("matches %v", m)
			}
			best = min(best, time.Since(start))
		}
		return best
	}
	small, large := cost(1<<9), cost(1<<12)
	if large > 24*small {
		t.Fatalf("8x the text took %.1fx as long (%v vs %v)", float64(large)/float64(small), large, small)
	}
	if _, ok := HoldBack([]Rule{r}, strings.Repeat("abc ", 512)); !ok {
		t.Fatal("keywords unbounded")
	}
}

func TestKeywordFoldedOffsets(t *testing.T) {
	kw, _ := Keywords("k", []string{"ｆorbidden"})
	s := "say Ｆorb\u200bidden! and forbidden"
	if got := Mask([]Rule{kw}, s); got != "say [REDACTED:k]! and [REDACTED:k]" {
		t.Fatalf("Mask = %q", got)
	}
	m := Find([]Rule{kw}, s)
	if len(m) != 2 || s[m[0].Start:m[0].End] != "Ｆorb\u200bidden" {
		t.Fatalf("Find = %v", m)
	}
}

// Invalid UTF-8 next to folded characters maps back to its own bytes.
func TestKeywordInvalidUTF8(t *testing.T) {
	kw, _ := Keywords("k", []string{"\uFFFD"})
	for s, want := range map[string]string{"\u200b\xff": "\u200b[REDACTED:k]", "a\u200b\xffb": "a\u200b[REDACTED:k]b"} {
		if got := Mask([]Rule{kw}, s); got != want {
			t.Errorf("Mask(%q) = %q", s, got)
		}
		for _, m := range Find([]Rule{kw}, s) {
			if m.Start < 0 || m.End > len(s) || m.Start > m.End {
				t.Errorf("Find(%q) = %v", s, m)
			}
		}
	}
}

func FuzzKeywordOffsets(f *testing.F) {
	f.Add("xx \uff46\uff4f\uff52\u200b\uff42\uff49\uff44 yy", "forbid")
	f.Add("a\xffforbid", "forbid")
	f.Add("\xff\xfe", "\uFFFD")
	f.Add("we \u212Ai", "kill")
	f.Fuzz(func(t *testing.T, s, word string) {
		r, ok := Keywords("k", []string{word})
		if !ok {
			return
		}
		for _, m := range Find([]Rule{r}, s) {
			if m.Start < 0 || m.End > len(s) || m.Start > m.End {
				t.Fatalf("match %v out of range of %d bytes", m, len(s))
			}
		}
		_ = Mask([]Rule{r}, s)
		if n, _ := HoldBack([]Rule{r}, s); n < 0 || n > len(s) {
			t.Fatalf("HoldBack %d of %d bytes", n, len(s))
		}
	})
}
