package builtin

import (
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/initxy/plugrelay/pkg/plugin"
)

// consoleCall is one pluginJSON call in the console: the plugin, method
// and path it asks for.
type consoleCall struct{ file, plugin, method, path string }

var (
	pluginConst = regexp.MustCompile(`export const ([A-Z_]+) = "([a-z.-]+)";`)
	topMethod   = regexp.MustCompile(`\bmethod:\s*"([A-Z]+)"`)
	topBody     = regexp.MustCompile(`\bbody\b`)
	tsParam     = regexp.MustCompile(`\$\{[^}]*\}`)
	goParam     = regexp.MustCompile(`\{[^}]*\}`)
)

// The billing pages call the sell kit's plugin routes directly (hard
// rule 24): every method and path they ask for must be one a sell-kit
// plugin registers, so renaming or dropping a route fails here rather
// than in the browser.
func TestConsoleBillingCallsExist(t *testing.T) {
	dir := filepath.Join("..", "..", "web", "src", "features", "billing")
	api, err := os.ReadFile(filepath.Join(dir, "api.ts"))
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]string{}
	for _, m := range pluginConst.FindAllStringSubmatch(string(api), -1) {
		ids[m[1]] = m[2]
	}
	if len(ids) < len(sellKit) {
		t.Fatalf("plugin ids in api.ts: %v", ids)
	}
	var calls []consoleCall
	err = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || strings.Contains(p, ".test.") || !(strings.HasSuffix(p, ".ts") || strings.HasSuffix(p, ".tsx")) {
			return err
		}
		src, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		calls = append(calls, pluginCalls(t, p, string(src), ids)...)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) < 40 {
		t.Fatalf("found only %d pluginJSON calls; is the parser out of date?", len(calls))
	}
	routes := map[string]bool{}
	for _, id := range sellKit {
		f, _ := plugin.Lookup(id)
		ad, ok := f().(plugin.Admin)
		if !ok {
			continue
		}
		for _, rt := range ad.Routes() {
			routes[id+" "+rt.Method+" "+goParam.ReplaceAllString(strings.TrimPrefix(rt.Path, "/"), "{}")] = true
		}
	}
	for _, c := range calls {
		if k := c.plugin + " " + c.method + " " + c.path; !routes[k] {
			t.Errorf("%s calls %s %s on %s, which registers no such route", c.file, c.method, c.path, c.plugin)
		}
	}
}

// pluginCalls finds the pluginJSON(ID, path, options?) calls in src.
func pluginCalls(t *testing.T, file, src string, ids map[string]string) []consoleCall {
	var out []consoleCall
	for i := 0; ; {
		j := strings.Index(src[i:], "pluginJSON")
		if j < 0 {
			return out
		}
		i += j + len("pluginJSON")
		if strings.HasSuffix(strings.TrimSpace(src[:i-len("pluginJSON")]), "function") {
			continue // the definition
		}
		k := i
		if k < len(src) && src[k] == '<' {
			k = skipBalanced(src, k, '<', '>')
		}
		if k >= len(src) || src[k] != '(' {
			continue // an import
		}
		end := skipBalanced(src, k, '(', ')')
		args := splitTop(src[k+1 : end-1])
		if len(args) < 2 {
			t.Errorf("%s: unreadable pluginJSON call %q", file, src[k:end])
			continue
		}
		id, ok := ids[args[0]]
		lit := args[1]
		if !ok || len(lit) < 2 || !strings.ContainsRune(`"`+"`", rune(lit[0])) {
			t.Errorf("%s: pluginJSON call the test cannot read: %q", file, src[k:end])
			continue
		}
		c := consoleCall{file: file, plugin: id, method: http.MethodGet, path: tsParam.ReplaceAllString(lit[1:len(lit)-1], "{}")}
		if len(args) > 2 {
			opts := topLevel(args[2])
			if m := topMethod.FindStringSubmatch(opts); m != nil {
				c.method = m[1]
			} else if topBody.MatchString(opts) {
				c.method = http.MethodPost
			}
		}
		out = append(out, c)
		i = end
	}
}

// skipBalanced returns the index just past the bracket closing the one at
// src[at], skipping string literals.
func skipBalanced(src string, at int, open, closing byte) int {
	depth := 0
	for i := at; i < len(src); i++ {
		switch c := src[i]; c {
		case '"', '\'', '`':
			for i++; i < len(src) && src[i] != c; i++ {
				if src[i] == '\\' {
					i++
				}
			}
		case open:
			depth++
		case closing:
			if depth--; depth == 0 {
				return i + 1
			}
		}
	}
	return len(src)
}

// splitTop splits a call's arguments at its top-level commas.
func splitTop(s string) []string {
	var out []string
	depth, start := 0, 0
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '"', '\'', '`':
			for i++; i < len(s) && s[i] != c; i++ {
				if s[i] == '\\' {
					i++
				}
			}
		case '(', '{', '[':
			depth++
		case ')', '}', ']':
			depth--
		case ',':
			if depth == 0 {
				out = append(out, strings.TrimSpace(s[start:i]))
				start = i + 1
			}
		}
	}
	if rest := strings.TrimSpace(s[start:]); rest != "" {
		out = append(out, rest)
	}
	return out
}

// topLevel keeps the top level of an object literal, dropping what nested
// braces hold (a body's own fields).
func topLevel(obj string) string {
	var b strings.Builder
	depth := 0
	for i := 0; i < len(obj); i++ {
		switch c := obj[i]; c {
		case '{', '[', '(':
			depth++
		case '}', ']', ')':
			depth--
		default:
			if depth <= 1 {
				b.WriteByte(c)
			}
		}
	}
	return b.String()
}
