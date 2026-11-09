package parampolicy

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/plugin"
	"github.com/initxy/plugrelay/plugins/internal/testkit"
	"github.com/tidwall/gjson"
)

func call(set testkit.Settings, proto string, req *ir.Request) *plugin.Call {
	return testkit.NewCall(ID, testkit.CallOpts{Settings: set, Protocol: proto, Request: req})
}

func TestMutateBody(t *testing.T) {
	tests := []struct {
		name  string
		proto string
		set   testkit.Settings
		in    string
		want  string
	}{
		{"no policy keeps bytes", protoAnthropic, nil,
			`{"model":"m", "max_tokens":99999}`, `{"model":"m", "max_tokens":99999}`},
		{"anthropic cap and budget", protoAnthropic, testkit.Settings{"max_output_tokens": 4096},
			`{"model":"m","max_tokens":32000,"thinking":{"type":"enabled","budget_tokens":16000},"system":[{"type":"text","text":"s","cache_control":{"type":"ephemeral"}}]}`,
			`{"model":"m","max_tokens":4096,"thinking":{"type":"enabled","budget_tokens":4095},"system":[{"type":"text","text":"s","cache_control":{"type":"ephemeral"}}]}`},
		{"anthropic under cap untouched", protoAnthropic, testkit.Settings{"max_output_tokens": 4096},
			`{"max_tokens":100,"model":"m"}`, `{"max_tokens":100,"model":"m"}`},
		{"chat fills missing cap", protoChat, testkit.Settings{"max_output_tokens": 500},
			`{"model":"m","messages":[]}`, `{"model":"m","messages":[],"max_tokens":500}`},
		{"chat caps both fields", protoChat, testkit.Settings{"max_output_tokens": 500},
			`{"max_completion_tokens":900,"max_tokens":200}`, `{"max_completion_tokens":500,"max_tokens":200}`},
		{"chat effort", protoChat, testkit.Settings{"max_reasoning_effort": "medium"},
			`{"reasoning_effort":"xhigh"}`, `{"reasoning_effort":"medium"}`},
		{"chat effort below cap", protoChat, testkit.Settings{"max_reasoning_effort": "medium"},
			`{"reasoning_effort":"low"}`, `{"reasoning_effort":"low"}`},
		{"responses effort", protoResponses, testkit.Settings{"max_reasoning_effort": "high"},
			`{"reasoning":{"effort":"max","summary":"auto"}}`, `{"reasoning":{"effort":"high","summary":"auto"}}`},
		{"anthropic effort", protoAnthropic, testkit.Settings{"max_reasoning_effort": "low"},
			`{"output_config":{"effort":"high"}}`, `{"output_config":{"effort":"low"}}`},
		{"gemini", protoGemini, testkit.Settings{"max_output_tokens": 1000, "strip_params": []string{"temperature"}},
			`{"generationConfig":{"maxOutputTokens":8192,"temperature":0.3,"thinkingConfig":{"thinkingBudget":-1}}}`,
			`{"generationConfig":{"maxOutputTokens":1000,"thinkingConfig":{"thinkingBudget":-1}}}`},
		{"strip", protoResponses, testkit.Settings{"strip_params": []string{"service_tier", "temperature"}},
			`{"model":"m","service_tier":"priority","temperature":1,"input":"hi"}`, `{"model":"m","input":"hi"}`},
		{"anthropic stop maps to stop_sequences", protoAnthropic, testkit.Settings{"strip_params": []string{"stop"}},
			`{"stop_sequences":["x"],"model":"m"}`, `{"model":"m"}`},
		{"unknown protocol untouched", "other", testkit.Settings{"max_output_tokens": 1},
			`{"max_tokens":5}`, `{"max_tokens":5}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := call(tt.set, tt.proto, nil)
			a := &plugin.Attempt{Tier: plugin.TierPatch, Candidate: plugin.Candidate{Protocol: tt.proto}}
			got, err := New().MutateBody(c, a, []byte(tt.in))
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tt.want {
				t.Errorf("\n got %s\nwant %s", got, tt.want)
			}
			again, _ := New().MutateBody(c, a, got)
			if string(again) != string(got) {
				t.Errorf("not idempotent: %s", again)
			}
		})
	}
}

func TestMutatesCoversEveryEdit(t *testing.T) {
	m := New().Info().Mutates
	for proto, w := range wires {
		paths := append(slices.Clone(w.maxTokens), w.effort)
		for _, ps := range w.strip {
			paths = append(paths, ps...)
		}
		for _, p := range paths {
			if !slices.Contains(m, p) {
				t.Errorf("%s: %s not declared in Mutates", proto, p)
			}
		}
	}
}

func TestConvertTierEditsIR(t *testing.T) {
	big, budget, temp := 64000, 50000, 0.5
	req := &ir.Request{MaxOutputTokens: &big, Temperature: &temp,
		Reasoning: &ir.Reasoning{Enabled: true, Effort: "max", Budget: &budget},
		Extra:     ir.Extra{Origin: protoChat, Fields: ir.RawFields{"seed": json.RawMessage(`1`), "x": json.RawMessage(`2`)}}}
	set := testkit.Settings{"max_output_tokens": 8000, "max_reasoning_effort": "high", "strip_params": []string{"temperature", "seed"}}
	c := call(set, protoChat, req)
	p := New()
	// Patch tier leaves the IR alone.
	if err := p.OnAttempt(c, &plugin.Attempt{Tier: plugin.TierPatch}); err != nil || *req.MaxOutputTokens != big {
		t.Fatal("patch tier touched IR")
	}
	if err := p.OnAttempt(c, &plugin.Attempt{Tier: plugin.TierConvert}); err != nil {
		t.Fatal(err)
	}
	if *req.MaxOutputTokens != 8000 || *req.Reasoning.Budget != 7999 || req.Reasoning.Effort != "high" || req.Temperature != nil {
		t.Fatalf("IR not capped: %+v %+v", req, req.Reasoning)
	}
	if _, ok := req.Extra.Fields["seed"]; ok {
		t.Fatal("seed not stripped from extra")
	}
	if _, ok := req.Extra.Fields["x"]; !ok {
		t.Fatal("unrelated extra removed")
	}

	// Both paths agree: the same policy on the wire form yields the same caps.
	body := []byte(`{"max_tokens":64000,"reasoning_effort":"max","temperature":0.5,"seed":1}`)
	out, _ := p.MutateBody(c, &plugin.Attempt{Tier: plugin.TierPatch, Candidate: plugin.Candidate{Protocol: protoChat}}, body)
	if gjson.GetBytes(out, "max_tokens").Int() != 8000 || gjson.GetBytes(out, "reasoning_effort").Str != "high" ||
		gjson.GetBytes(out, "temperature").Exists() || gjson.GetBytes(out, "seed").Exists() {
		t.Fatalf("wire path disagrees: %s", out)
	}
}

func TestMissingLimitFilledInIR(t *testing.T) {
	req := &ir.Request{}
	c := call(testkit.Settings{"max_output_tokens": 100}, protoAnthropic, req)
	if err := New().OnAttempt(c, &plugin.Attempt{Tier: plugin.TierConvert}); err != nil {
		t.Fatal(err)
	}
	if req.MaxOutputTokens == nil || *req.MaxOutputTokens != 100 {
		t.Fatal("cap not filled in")
	}
}
