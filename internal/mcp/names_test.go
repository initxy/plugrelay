package mcp

import "testing"

// TestAccessToolGlobCrossesSlash: "*" in a tool pattern matches "/", so a
// deny pattern cannot be stepped around with a slashed tool name.
func TestAccessToolGlobCrossesSlash(t *testing.T) {
	gh := &Server{ID: "gh"}
	all := []string{"*"}
	cases := []struct {
		name string
		a    Access
		srv  *Server
		tool string
		want bool
	}{
		{"deny prefix", Access{Servers: all, ToolsDeny: []string{"gh__*"}}, gh, "repo/delete", false},
		{"deny infix", Access{Servers: all, ToolsDeny: []string{"gh__*delete*"}}, gh, "repo/delete_all", false},
		{"server deny all", Access{Servers: all}, &Server{ID: "gh", ToolsDeny: []string{"*"}}, "a/b", false},
		{"server allow pattern", Access{Servers: all}, &Server{ID: "gh", ToolsAllow: []string{"repo/*"}}, "repo/x/y", true},
		{"allow prefix", Access{Servers: all, ToolsAllow: [][]string{{"gh__repo*"}}}, gh, "repo/read", true},
		{"question mark", Access{Servers: all, ToolsDeny: []string{"gh__a?b"}}, gh, "a/b", false},
		// Every allow layer must match: a narrower key layer of exact names
		// still sits under the group's pattern.
		{"layers both match", Access{Servers: all, ToolsAllow: [][]string{{"gh__read_*"}, {"gh__read_file"}}}, gh, "read_file", true},
		{"layers disjoint", Access{Servers: all, ToolsAllow: [][]string{{"gh__read_*"}, {"gh__list_issues"}}}, gh, "list_issues", false},
	}
	for _, tc := range cases {
		if got := tc.a.Tool(tc.srv, tc.tool); got != tc.want {
			t.Errorf("%s: Tool(%q) = %v, want %v", tc.name, tc.tool, got, tc.want)
		}
	}
}

// TestHeaderValueEncoding: values that are not plain ASCII, have edge
// whitespace or look like the marker are base64-encoded and decode back.
func TestHeaderValueEncoding(t *testing.T) {
	for in, encoded := range map[string]bool{"now": false, "a/b c": false, "Hello, 世界": true, " padded ": true, "=?base64?literal?=": true} {
		v := headerValue(in)
		if (v != in) != encoded {
			t.Errorf("headerValue(%q) = %q", in, v)
		}
		if out, ok := decodeHeaderValue(v); !ok || out != in {
			t.Errorf("decode(%q) = %q, %v", v, out, ok)
		}
	}
	if v := headerValue("Hello, 世界"); v != "=?base64?SGVsbG8sIOS4lueVjA==?=" {
		t.Errorf("spec example encodes to %q", v)
	}
	if _, ok := decodeHeaderValue("=?base64?***?="); ok {
		t.Error("bad base64 accepted")
	}
}
