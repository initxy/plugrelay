package mcp

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"slices"
	"strconv"
	"strings"

	"github.com/tidwall/gjson"

	"github.com/initxy/plugrelay/pkg/match"
)

// Sep joins a server id and a tool name in the aggregated endpoint and in
// access patterns ("github__create_issue"). Server ids have no
// underscores, so the first Sep always ends the server id.
const Sep = "__"

// maxName keeps aggregated names within what model APIs accept for
// function names.
const maxName = 64

// aliases names a server's tools for the aggregated endpoint:
// <server>__<tool>, with characters outside [A-Za-z0-9_-] replaced, cut
// to 64 characters with a hash when longer, and numbered when two tools
// still end up with the same name. Tools are taken in name order, so the
// result does not depend on the order a server lists them in.
func aliases(server string, tools []string) map[string]string {
	sorted := slices.Clone(tools)
	slices.Sort(sorted)
	out := map[string]string{}
	for _, t := range sorted {
		base := server + Sep + sanitize(t)
		if len(base) > maxName {
			base = base[:maxName-9] + "_" + hash8(server+"/"+t)
		}
		name := base
		for n := 2; out[name] != ""; n++ {
			suffix := "_" + strconv.Itoa(n)
			name = base[:min(len(base), maxName-len(suffix))] + suffix
		}
		out[name] = t
	}
	return out
}

func sanitize(s string) string {
	b := []byte(s)
	for i, c := range b {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			b[i] = '_'
		}
	}
	if len(b) == 0 {
		return "_"
	}
	return string(b)
}

func hash8(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:4])
}

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

// Access is what one caller may reach: granted servers ("*" = all) and
// tool patterns over "<server>__<tool>". ToolsAllow holds one list per
// settings layer, and a tool must match every one of them.
type Access struct {
	Servers    []string
	ToolsAllow [][]string
	ToolsDeny  []string
}

// Server reports whether the server is granted.
func (a Access) Server(id string) bool {
	return slices.Contains(a.Servers, "*") || slices.Contains(a.Servers, id)
}

// Tool reports whether the caller may use a server's tool. The server's
// own allow and deny lists are matched against the bare tool name.
func (a Access) Tool(s *Server, tool string) bool {
	full := s.ID + Sep + tool
	for _, allow := range a.ToolsAllow {
		if len(allow) > 0 && !match.Any(full, allow) {
			return false
		}
	}
	if match.Any(full, a.ToolsDeny) {
		return false
	}
	if len(s.ToolsAllow) > 0 && !match.Any(tool, s.ToolsAllow) {
		return false
	}
	return !match.Any(tool, s.ToolsDeny)
}

func jsonString(raw []byte, key string) string { return gjson.GetBytes(raw, key).String() }

func short(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 200 {
		return s[:200] + "…"
	}
	return s
}
