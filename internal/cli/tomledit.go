package cli

import (
	"regexp"
	"strings"
)

// The TOML edits below work on lines so comments and layout survive. They
// assume table headers and owned keys sit on their own lines and are not
// inside multi-line strings, which holds for Codex's config.toml.

var tomlHeader = regexp.MustCompile(`^\s*\[`)

// tomlTableName returns the normalized name of a "[a.b]" header line.
func tomlTableName(line string) (string, bool) {
	t := strings.TrimSpace(line)
	if i := strings.Index(t, "#"); i >= 0 {
		t = strings.TrimSpace(t[:i])
	}
	if !strings.HasPrefix(t, "[") || strings.HasPrefix(t, "[[") || !strings.HasSuffix(t, "]") {
		return "", false
	}
	name := strings.TrimSpace(t[1 : len(t)-1])
	var parts []string
	for _, p := range strings.Split(name, ".") {
		parts = append(parts, strings.Trim(strings.TrimSpace(p), `"'`))
	}
	return strings.Join(parts, "."), true
}

// tomlSetTopLevel sets key = value (value already encoded) in the
// top-level table, replacing an existing assignment or inserting one
// before the first table header.
func tomlSetTopLevel(lines []string, key, value string) []string {
	assign := regexp.MustCompile(`^\s*"?` + regexp.QuoteMeta(key) + `"?\s*=`)
	end := len(lines)
	for i, l := range lines {
		if tomlHeader.MatchString(l) {
			end = i
			break
		}
	}
	newLine := key + " = " + value
	for i := 0; i < end; i++ {
		if assign.MatchString(lines[i]) {
			lines[i] = newLine
			return lines
		}
	}
	at := end
	for at > 0 && strings.TrimSpace(lines[at-1]) == "" {
		at--
	}
	ins := []string{newLine}
	if at == end && end < len(lines) {
		ins = append(ins, "")
	}
	return append(lines[:at], append(ins, lines[at:]...)...)
}

// tomlReplaceTable replaces the body of table name with body (header
// included), in place, or appends it.
func tomlReplaceTable(lines []string, name string, body []string) []string {
	start := -1
	for i, l := range lines {
		if n, ok := tomlTableName(l); ok && n == name {
			start = i
			break
		}
	}
	if start < 0 {
		for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
			lines = lines[:len(lines)-1]
		}
		if len(lines) > 0 {
			lines = append(lines, "")
		}
		return append(lines, body...)
	}
	end := len(lines)
	for i := start + 1; i < len(lines); i++ {
		if tomlHeader.MatchString(lines[i]) {
			end = i
			break
		}
	}
	// Keep the blank lines that separated the old table from the next.
	for end > start+1 && strings.TrimSpace(lines[end-1]) == "" {
		end--
	}
	out := append([]string{}, lines[:start]...)
	out = append(out, body...)
	return append(out, lines[end:]...)
}

// tomlString encodes s as a TOML basic string.
func tomlString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, c := range s {
		switch c {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\t':
			b.WriteString(`\t`)
		default:
			b.WriteRune(c)
		}
	}
	b.WriteByte('"')
	return b.String()
}

func splitLines(src []byte) []string {
	s := strings.TrimRight(strings.ReplaceAll(string(src), "\r\n", "\n"), "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

func joinLines(lines []string) []byte {
	return []byte(strings.Join(lines, "\n") + "\n")
}
