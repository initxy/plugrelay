package admin

import (
	"encoding/json"
	"errors"
	"os"
	"regexp"
	"slices"
	"testing"

	"github.com/initxy/plugrelay/internal/store"
)

// TestErrorCodesTranslated holds the console's dictionary to the codes
// and param values the admin API answers with: each needs an
// "apiError.<code>" entry using the same placeholders, and the console
// keeps no entry for a code that is gone. (zh-CN parity with en is the
// console's own test.)
func TestErrorCodesTranslated(t *testing.T) {
	src, err := os.ReadFile("../../web/src/i18n/en.ts")
	if err != nil {
		t.Fatal(err)
	}
	entry := regexp.MustCompile(`"apiError\.([\w.]+)":\s*"((?:[^"\\]|\\.)*)"`)
	console := map[string]string{}
	for _, m := range entry.FindAllStringSubmatch(string(src), -1) {
		console[m[1]] = m[2]
	}
	if len(console) == 0 {
		t.Fatal(`no "apiError.*" entries found in en.ts`)
	}
	names := func(re *regexp.Regexp, s string) []string {
		var out []string
		for _, m := range re.FindAllStringSubmatch(s, -1) {
			if !slices.Contains(out, m[1]) {
				out = append(out, m[1])
			}
		}
		slices.Sort(out)
		return out
	}
	// The console's messages also have "{n, plural, ...}" selectors.
	consolePH := regexp.MustCompile(`\{(\w+)[},]`)
	for code, en := range errTexts {
		text, ok := console[code]
		if !ok {
			t.Errorf(`en.ts lacks "apiError.%s" (%s)`, code, en)
			continue
		}
		if got, want := names(consolePH, text), names(placeholderRe, en); !slices.Equal(got, want) {
			t.Errorf(`"apiError.%s" uses params %v, the API sends %v`, code, got, want)
		}
	}
	for key := range errValues {
		if _, ok := console[key]; !ok {
			t.Errorf(`en.ts lacks "apiError.%s"`, key)
		}
	}
	for key := range console {
		_, isCode := errTexts[key]
		_, isValue := errValues[key]
		if !isCode && !isValue {
			t.Errorf(`en.ts has "apiError.%s", which the API never sends`, key)
		}
	}
}

// TestErrorBody pins the wire shape clients translate from.
func TestErrorBody(t *testing.T) {
	var bad invalid
	bad.add("name", eLengthRange.with("min", 1, "max", 64))
	bad.addErr("currency", eCurrencyCode)
	bad.addErr("x", errors.New("plain"))
	cases := map[string]error{
		"fields":    bad.err(),
		"not_found": storeErr(store.ErrNotFound, objUser),
	}
	want := map[string]string{
		"fields": `{"status":422,"code":"invalid","params":{"count":3},"message":"3 fields are invalid","fields":[` +
			`{"path":"name","code":"length_range","params":{"max":64,"min":1},"message":"must be 1 to 64 characters"},` +
			`{"path":"currency","code":"currency_code","message":"must be a 3-letter currency code"},` +
			`{"path":"x","code":"value_rejected","params":{"reason":"plain"},"message":"not accepted: plain"}]}`,
		"not_found": `{"status":404,"code":"not_found","params":{"object":"user"},"message":"user not found"}`,
	}
	for name, err := range cases {
		ae := err.(*apiError)
		b, _ := json.Marshal(struct {
			Status int `json:"status"`
			*apiError
		}{ae.Status, ae})
		if string(b) != want[name] {
			t.Errorf("%s:\n got %s\nwant %s", name, b, want[name])
		}
	}
}
