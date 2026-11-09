// Package match is the one name matcher for operator-written lists, shared
// by the core and the plugins: model allowlists, blocked models and tools,
// MCP tool grants.
package match

import (
	"path"
	"strings"
)

// Any reports whether name equals or glob-matches one of patterns,
// ignoring case and surrounding spaces. "*" also matches "/" ("*" covers
// "openai/gpt-5"), so a deny list built on it cannot be stepped around.
// An empty list matches nothing.
func Any(name string, patterns []string) bool {
	name = strings.ToLower(name)
	for _, p := range patterns {
		p = strings.ToLower(strings.TrimSpace(p))
		if p == name || glob(p, name) {
			return true
		}
	}
	return false
}

// glob is path.Match with "/" as an ordinary character: both sides map it
// to NUL, which path.Match does not treat specially.
func glob(pattern, name string) bool {
	ok, err := path.Match(strings.ReplaceAll(pattern, "/", "\x00"), strings.ReplaceAll(name, "/", "\x00"))
	return err == nil && ok
}
