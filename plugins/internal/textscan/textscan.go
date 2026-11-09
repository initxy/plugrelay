// Package textscan finds sensitive text for the middleware plugins that
// mask or refuse it (middleware.redact, middleware.guardrails): built-in
// detectors for personal data and secrets, keyword and pattern rules, and
// the user-written text of a request, as JSON paths in each client
// protocol's body or as the IR's user text blocks, with helpers that mask
// it in either form.
//
// Detectors are conservative: numbers count only when their checksum or
// structure holds (Luhn for cards, mod 97 for IBANs, the ISO 7064 check
// digit and a real birth date for Chinese resident IDs, the SSA's reserved
// ranges for US SSNs), so ordinary numbers, hashes and dates do not match.
package textscan

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/plugin"
)

// Rule is one compiled thing to look for; Name labels its matches.
type Rule struct {
	Name  string
	re    *regexp.Regexp
	valid func(string) bool
	// words are a keyword rule's words, folded; such a rule matches on
	// folded text (see fold). nil for patterns and detectors.
	words []string
}

// Match is one finding in a text, as byte offsets.
type Match struct {
	Rule       string
	Start, End int
}

// Detector names.
const (
	Email      = "email"
	Phone      = "phone"
	CreditCard = "credit_card"
	IBAN       = "iban"
	CNID       = "cn_id"
	USSSN      = "us_ssn"
	Secret     = "secret"
	PrivateKey = "private_key"
)

// Detectors lists every built-in detector name.
var Detectors = []string{Email, Phone, CreditCard, IBAN, CNID, USSSN, Secret, PrivateKey}

// DetectorLabels names the detectors for people.
var DetectorLabels = map[string]plugin.Text{
	Email:      plugin.T("Email addresses", "邮箱地址"),
	Phone:      plugin.T("Phone numbers", "电话号码"),
	CreditCard: plugin.T("Card numbers", "银行卡号"),
	IBAN:       plugin.T("IBANs", "国际银行账号（IBAN）"),
	CNID:       plugin.T("Chinese resident ID numbers", "居民身份证号"),
	USSSN:      plugin.T("US social security numbers", "美国社会安全号（SSN）"),
	Secret:     plugin.T("API keys and access tokens", "API 密钥和访问令牌"),
	PrivateKey: plugin.T("Private keys", "私钥"),
}

var builtin = map[string]Rule{
	// Prefixed keys (sk-…, AKIA…, ghp_…, xoxb-…, AIza…, glpat-…), bearer
	// tokens and JWTs.
	Secret: {Name: Secret, re: regexp.MustCompile(`\b(?:(?:sk|pk|rk|ak)-[A-Za-z0-9_\-]{16,}|AKIA[0-9A-Z]{16}|gh[pousr]_[A-Za-z0-9]{30,}|xox[abprs]-[A-Za-z0-9\-]{10,}|AIza[0-9A-Za-z_\-]{35}|glpat-[A-Za-z0-9_\-]{20,}|eyJ[A-Za-z0-9_\-]{8,}\.eyJ[A-Za-z0-9_\-]{8,}\.[A-Za-z0-9_\-]{8,})|(?i:bearer\s+)[A-Za-z0-9._\-]{20,}`)},
	// A PEM private key block, or its header alone when the end is missing.
	PrivateKey: {Name: PrivateKey, re: regexp.MustCompile(`-----BEGIN (?:[A-Z0-9]+ )*PRIVATE KEY-----(?:[A-Za-z0-9+/=\s:\-,]*?-----END (?:[A-Z0-9]+ )*PRIVATE KEY-----)?`)},
	Email:      {Name: Email, re: regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)},
	// International numbers written with a leading +, 11-digit mainland
	// China mobiles, and North American numbers written with separators.
	Phone:      {Name: Phone, re: regexp.MustCompile(`\+\d{1,3}[\s\-]?\(?\d{1,4}\)?(?:[\s\-]?\d{2,4}){2,3}\b|\b1[3-9]\d{9}\b|(?:\(\d{3}\)\s?|\b\d{3}[\-.])\d{3}[\-.]\d{4}\b`)},
	CreditCard: {Name: CreditCard, re: regexp.MustCompile(`\b\d(?:[ \-]?\d){12,18}\b`), valid: validCard},
	IBAN:       {Name: IBAN, re: regexp.MustCompile(`\b[A-Z]{2}\d{2}(?: ?[A-Z0-9]){11,30}\b`), valid: validIBAN},
	CNID:       {Name: CNID, re: regexp.MustCompile(`\b\d{17}[\dXx]\b`), valid: validCNID},
	USSSN:      {Name: USSSN, re: regexp.MustCompile(`\b\d{3}-\d{2}-\d{4}\b`), valid: validSSN},
}

// Builtin returns the named detector.
func Builtin(name string) (Rule, bool) {
	r, ok := builtin[name]
	return r, ok
}

var (
	cacheMu sync.Mutex
	cache   = map[string]*regexp.Regexp{}
)

func compile(expr string) (*regexp.Regexp, error) {
	cacheMu.Lock()
	defer cacheMu.Unlock()
	if re, ok := cache[expr]; ok {
		return re, nil
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		return nil, err
	}
	if len(cache) >= 512 {
		clear(cache)
	}
	cache[expr] = re
	return re, nil
}

// Pattern compiles an operator's RE2 expression into a rule named name.
func Pattern(name, expr string) (Rule, error) {
	re, err := compile(expr)
	if err != nil {
		return Rule{}, err
	}
	return Rule{Name: name, re: re}, nil
}

// Keywords is one rule matching any of words as plain substrings, ignoring
// case, zero-width characters (U+200B–U+200D, U+2060, U+FEFF, soft hyphen)
// and full-width forms of ASCII ("ｆｏｒｂｉｄ"). Other look-alikes
// (homoglyphs, other compatibility forms) are not folded. It reports false
// when words holds nothing to match.
func Keywords(name string, words []string) (Rule, bool) {
	var alts, folded []string
	for _, w := range words {
		if w = strings.TrimSpace(fold(w).s); w != "" {
			alts = append(alts, regexp.QuoteMeta(w))
			folded = append(folded, w)
		}
	}
	if len(alts) == 0 {
		return Rule{}, false
	}
	re, err := compile(`(?i)(?:` + strings.Join(alts, "|") + `)`)
	if err != nil {
		return Rule{}, false
	}
	return Rule{Name: name, re: re, words: folded}, true
}

// folded is a text with zero-width characters dropped and full-width ASCII
// mapped to ASCII; span maps a byte range of s back to the original. When
// nothing needed folding, s is the original and from is nil.
type folded struct {
	s        string
	from, to []int // per byte of s: start and end of its source rune
}

func foldRune(r rune) (rune, bool) {
	switch {
	case r >= 0x200B && r <= 0x200D, r == 0x2060, r == 0xFEFF, r == 0x00AD:
		return 0, false
	case r >= 0xFF01 && r <= 0xFF5E:
		return r - 0xFEE0, true
	case r == 0x3000:
		return ' ', true
	}
	return r, true
}

func fold(orig string) folded {
	if !strings.ContainsFunc(orig, func(r rune) bool { f, ok := foldRune(r); return !ok || f != r }) {
		return folded{s: orig}
	}
	var b strings.Builder
	var from, to []int
	for i := 0; i < len(orig); {
		// An invalid byte decodes as U+FFFD of width 1, as the regexp
		// matcher reads it.
		r, w := utf8.DecodeRuneInString(orig[i:])
		if f, ok := foldRune(r); ok {
			n := b.Len()
			b.WriteRune(f)
			for range b.Len() - n {
				from, to = append(from, i), append(to, i+w)
			}
		}
		i += w
	}
	return folded{s: b.String(), from: from, to: to}
}

// span maps [a, b) of the folded text to the original.
func (f folded) span(a, b int) (int, int) {
	if f.from == nil {
		return a, b
	}
	if a >= b {
		if a >= len(f.from) {
			return f.to[len(f.to)-1], f.to[len(f.to)-1]
		}
		return f.from[a], f.from[a]
	}
	return f.from[a], f.to[b-1]
}

// find returns r's matches in s as original offsets.
func (r Rule) find(s string, f *folded) [][]int {
	if r.words == nil {
		return r.re.FindAllStringIndex(s, -1)
	}
	if f.s == "" && s != "" {
		*f = fold(s)
	}
	locs := r.re.FindAllStringIndex(f.s, -1)
	for _, loc := range locs {
		loc[0], loc[1] = f.span(loc[0], loc[1])
	}
	return locs
}

// HoldBack returns how many bytes at the end of s could be the start of a
// keyword that later text completes. ok is false when a rule cannot tell
// (patterns and detectors): the caller must then treat all of s as
// possibly incomplete.
func HoldBack(rules []Rule, s string) (n int, ok bool) {
	for _, r := range rules {
		if r.words == nil {
			return len(s), false
		}
	}
	f := fold(s)
	for _, r := range rules {
		for _, w := range r.words {
			// The longest suffix of the text that is a proper prefix of w
			// under the matcher's case folding, where a rune's fold may
			// differ in byte length (K and the Kelvin sign).
			for i := max(0, len(f.s)-utf8.UTFMax*utf8.RuneCountInString(w)); i < len(f.s); i++ {
				if !utf8.RuneStart(f.s[i]) || !foldPrefix(f.s[i:], w) {
					continue
				}
				start, _ := f.span(i, len(f.s))
				n = max(n, len(s)-start)
				break
			}
		}
	}
	return n, true
}

// foldPrefix reports whether t is a proper prefix of w, rune by rune under
// simple case folding (as (?i) matches).
func foldPrefix(t, w string) bool {
	for t != "" {
		if w == "" {
			return false
		}
		tr, tn := utf8.DecodeRuneInString(t)
		wr, wn := utf8.DecodeRuneInString(w)
		if tr != wr && !strings.EqualFold(string(tr), string(wr)) {
			return false
		}
		t, w = t[tn:], w[wn:]
	}
	return w != ""
}

// Find returns every match of rules in s, ordered by position.
func Find(rules []Rule, s string) []Match {
	var out []Match
	var f folded
	for _, r := range rules {
		for _, loc := range r.find(s, &f) {
			if r.valid == nil || r.valid(s[loc[0]:loc[1]]) {
				out = append(out, Match{Rule: r.Name, Start: loc[0], End: loc[1]})
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Start < out[j].Start })
	return out
}

// Mask replaces every match of rules in s with "[REDACTED:<rule>]". Masked
// text does not match again, so masking twice changes nothing.
func Mask(rules []Rule, s string) string {
	for _, r := range rules {
		if r.words != nil {
			var f folded
			locs := r.find(s, &f)
			for i := len(locs) - 1; i >= 0; i-- {
				s = s[:locs[i][0]] + "[REDACTED:" + r.Name + "]" + s[locs[i][1]:]
			}
			continue
		}
		s = r.re.ReplaceAllStringFunc(s, func(m string) string {
			if r.valid != nil && !r.valid(m) {
				return m
			}
			return "[REDACTED:" + r.Name + "]"
		})
	}
	return s
}

func digits(s string) string {
	var b strings.Builder
	for _, c := range s {
		if c >= '0' && c <= '9' {
			b.WriteRune(c)
		}
	}
	return b.String()
}

// validCard: 13–19 digits with a card-network prefix and a Luhn checksum;
// separators must be used consistently (all spaces or all dashes).
func validCard(m string) bool {
	if strings.Contains(m, " ") && strings.Contains(m, "-") {
		return false
	}
	d := digits(m)
	if len(d) < 13 || len(d) > 19 || strings.Count(d, d[:1]) == len(d) {
		return false
	}
	switch {
	case d[0] == '4', d[0] == '5' && d[1] >= '1' && d[1] <= '5', d[0] == '2' && d[1] >= '2' && d[1] <= '7',
		d[0] == '3' && (d[1] == '4' || d[1] == '7'), d[0] == '6', d[0] == '3' && (d[1] == '5' || d[1] == '0' || d[1] == '6' || d[1] == '8'):
	default:
		return false
	}
	sum := 0
	for i := range len(d) {
		n := int(d[len(d)-1-i] - '0')
		if i%2 == 1 {
			if n *= 2; n > 9 {
				n -= 9
			}
		}
		sum += n
	}
	return sum%10 == 0
}

// validIBAN: 15–34 characters and the ISO 13616 mod-97 check.
func validIBAN(m string) bool {
	s := strings.ReplaceAll(m, " ", "")
	if len(s) < 15 || len(s) > 34 {
		return false
	}
	s = s[4:] + s[:4]
	rem := 0
	for _, c := range s {
		switch {
		case c >= '0' && c <= '9':
			rem = (rem*10 + int(c-'0')) % 97
		case c >= 'A' && c <= 'Z':
			rem = (rem*100 + int(c-'A'+10)) % 97
		default:
			return false
		}
	}
	return rem == 1
}

// validCNID: an 18-character resident ID with a plausible birth date and
// the GB 11643 (ISO 7064 MOD 11-2) check character.
func validCNID(m string) bool {
	if len(m) != 18 {
		return false
	}
	y, _ := strconv.Atoi(m[6:10])
	mo, _ := strconv.Atoi(m[10:12])
	d, _ := strconv.Atoi(m[12:14])
	if y < 1900 || y > 2100 || mo < 1 || mo > 12 || d < 1 || d > 31 {
		return false
	}
	weights := []int{7, 9, 10, 5, 8, 4, 2, 1, 6, 3, 7, 9, 10, 5, 8, 4, 2}
	sum := 0
	for i, w := range weights {
		sum += int(m[i]-'0') * w
	}
	return "10X98765432"[sum%11] == strings.ToUpper(m[17:])[0]
}

// validSSN rejects the ranges the SSA never issues.
func validSSN(m string) bool {
	area, group, serial := m[0:3], m[4:6], m[7:11]
	return area != "000" && area != "666" && area[0] != '9' && group != "00" && serial != "0000"
}

// UserTextFields are the gjson patterns every path UserTextPaths returns
// matches, for a masking plugin's Info.Mutates.
var UserTextFields = []string{
	"messages.#.content", "messages.#.content.#.text", // anthropic, openai-chat
	"input", "input.#.content", "input.#.content.#.text", // openai-responses
	"contents.#.parts.#.text", // gemini
	"prompt", "suffix",        // openai-completions
}

// MaskBody rewrites the user-written text of a request body of protocol
// proto (UserTextPaths) with mask; every other byte stays as sent.
func MaskBody(body []byte, proto string, mask func(string) string) ([]byte, error) {
	var err error
	for _, p := range UserTextPaths(body, proto) {
		old := gjson.GetBytes(body, p).Str
		if nu := mask(old); nu != old {
			if body, err = sjson.SetBytes(body, p, nu); err != nil {
				return nil, err
			}
		}
	}
	return body, nil
}

// MaskRequest rewrites the text blocks of req's user turns with mask.
func MaskRequest(req *ir.Request, mask func(string) string) {
	for i := range req.Messages {
		m := &req.Messages[i]
		if m.Role != ir.RoleUser {
			continue
		}
		for j := range m.Content {
			if b := &m.Content[j]; b.Type == ir.BlockText {
				b.Text = mask(b.Text)
			}
		}
	}
}

// UserTextPaths finds the JSON paths of user-written text in a request
// body of protocol proto: message text (a string content or text parts),
// not tool results, system prompts or other roles' turns.
func UserTextPaths(body []byte, proto string) []string {
	var paths []string
	switch proto {
	case "anthropic", "openai-chat":
		paths = rolePaths(body, "messages", "content", "text")
	case "openai-responses":
		if in := gjson.GetBytes(body, "input"); in.Type == gjson.String {
			paths = []string{"input"}
		} else {
			paths = rolePaths(body, "input", "content", "input_text")
		}
	case "openai-completions":
		for _, p := range []string{"prompt", "prompt.0", "suffix"} {
			if gjson.GetBytes(body, p).Type == gjson.String {
				paths = append(paths, p)
			}
		}
	case "gemini":
		gjson.GetBytes(body, "contents").ForEach(func(i, m gjson.Result) bool {
			if role := m.Get("role").Str; role == "" || role == "user" {
				m.Get("parts").ForEach(func(j, p gjson.Result) bool {
					if p.Get("text").Type == gjson.String {
						paths = append(paths, "contents."+strconv.Itoa(int(i.Int()))+".parts."+strconv.Itoa(int(j.Int()))+".text")
					}
					return true
				})
			}
			return true
		})
	}
	return paths
}

// rolePaths finds user-role text under list[i].content, where content is
// a string or an array of parts of type partType carrying "text".
func rolePaths(body []byte, list, content, partType string) []string {
	var out []string
	gjson.GetBytes(body, list).ForEach(func(i, m gjson.Result) bool {
		if m.Get("role").Str != "user" {
			return true
		}
		base := list + "." + strconv.Itoa(int(i.Int())) + "." + content
		cv := m.Get(content)
		switch {
		case cv.Type == gjson.String:
			out = append(out, base)
		case cv.IsArray():
			cv.ForEach(func(j, p gjson.Result) bool {
				if p.Get("type").Str == partType && p.Get("text").Type == gjson.String {
					out = append(out, base+"."+strconv.Itoa(int(j.Int()))+".text")
				}
				return true
			})
		}
		return true
	})
	return out
}
