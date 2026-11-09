package pipeline

import (
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/initxy/plugrelay/pkg/plugin"
)

func codexRequest(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("../../pkg/protocol/openairesponses/testdata/codex_request.json")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := sjson.SetBytes(b, "model", "gpt-x")
	return string(body)
}

const chatNamespacedCalls = `data: {"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"mcp__notes__add_note","arguments":"{\"text\":\"x\"}"}}]},"finish_reason":null}]}

data: {"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"call_2","type":"function","function":{"name":"apply_patch","arguments":"{\"input\":\"*** Begin Patch\\n*** End Patch\\n\"}"}}]},"finish_reason":null}]}

data: {"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}

data: {"id":"c1","object":"chat.completion.chunk","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}

data: [DONE]

`

// A Codex request served by a chat-only endpoint: the offered web_search is
// dropped, namespaced MCP tools are flattened and their calls come back with
// name and namespace, apply_patch is wrapped and unwrapped.
func TestCodexToChat(t *testing.T) {
	up := newFake(t, sseReply(0, chatNamespacedCalls))
	e := newEnv(t, "gpt-x", []upSpec{{id: "o", protocol: "openai-chat", issuer: "iss-o", fake: up, caps: "{freeform_tools: false}"}})
	resp, body := e.call("openai-responses", "/v1/responses", codexRequest(t))
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	ub := up.body(0)
	var names []string
	for _, tool := range gjson.GetBytes(ub, "tools").Array() {
		if tool.Get("type").Str != "function" {
			t.Errorf("non-function tool sent: %s", tool.Raw)
		}
		names = append(names, tool.Get("function.name").Str)
	}
	for _, want := range []string{"mcp__notes__add_note", "multi_agent_v1__spawn_agent", "apply_patch", "exec_command"} {
		if !slices.Contains(names, want) {
			t.Errorf("tool %s missing from %v", want, names)
		}
	}
	if strings.Contains(string(ub), "web_search") {
		t.Error("offered web_search sent to the chat endpoint")
	}
	if !strings.Contains(string(ub), `"name":"mcp__notes__add_note"`) {
		t.Errorf("history call not flattened: %s", ub)
	}
	s := string(body)
	for _, want := range []string{`"name":"add_note"`, `"namespace":"mcp__notes"`, `"type":"custom_tool_call"`, `"input":"*** Begin Patch\n*** End Patch\n"`} {
		if !strings.Contains(s, want) {
			t.Errorf("client stream lacks %s:\n%s", want, s)
		}
	}
	if strings.Contains(s, "mcp__notes__add_note") {
		t.Errorf("flattened name leaked to the client:\n%s", s)
	}
	a := e.records.last(t).Attempts[0]
	if a.Tier != plugin.TierConvert || a.Diagnostic == nil || !slices.Contains(a.Diagnostic.Dropped, "server_tool:web_search") {
		t.Fatalf("attempt %+v diag %+v", a, a.Diagnostic)
	}
}

// Same protocol: an endpoint without web_search gets the request converted
// with the tool removed and namespaces kept; a full endpoint gets the bytes.
func TestCodexSameProtocolServerTools(t *testing.T) {
	stream := "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n"
	up := newFake(t, sseReply(0, stream))
	e := newEnv(t, "gpt-x", []upSpec{{id: "r", protocol: "openai-responses", issuer: "iss-r", fake: up, caps: "{server_tools: [tool_search]}"}})
	req := codexRequest(t)
	if resp, body := e.call("openai-responses", "/v1/responses", req); resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	ub := up.body(0)
	if strings.Contains(string(ub), `"web_search"`) || !strings.Contains(string(ub), `"type":"namespace","name":"mcp__notes"`) {
		t.Errorf("upstream body: %s", ub)
	}
	if tier := e.records.last(t).Attempts[0].Tier; tier != plugin.TierConvert {
		t.Errorf("tier %s", tier)
	}

	up2 := newFake(t, sseReply(0, stream))
	e2 := newEnv(t, "gpt-x", []upSpec{{id: "r", protocol: "openai-responses", issuer: "iss-r", fake: up2}})
	if resp, body := e2.call("openai-responses", "/v1/responses", req); resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	if string(up2.body(0)) != req {
		t.Error("full endpoint did not get the client's bytes")
	}
	if tier := e2.records.last(t).Attempts[0].Tier; tier != plugin.TierPassthrough {
		t.Errorf("tier %s", tier)
	}
}
