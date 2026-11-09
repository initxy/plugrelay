package pipeline

import "testing"

// The record names the client from the request headers, not only the
// User-Agent: an editor extension on an SDK is told by its title header.
func TestRecordNamesClient(t *testing.T) {
	up := newFake(t, jsonReply(200, anthropicThinkingResp))
	e := newEnv(t, "claude-x", []upSpec{{id: "a", protocol: "anthropic", issuer: "i", fake: up}})
	e.call("anthropic", "/v1/messages", anthropicReq, "User-Agent", "Anthropic/JS 0.65.0", "X-Title", "Kilo Code")
	if rec := e.records.last(t); rec.Client != "kilo" || rec.ClientVersion != "" {
		t.Fatalf("client %q %q", rec.Client, rec.ClientVersion)
	}
}
