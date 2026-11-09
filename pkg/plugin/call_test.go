package plugin

import "testing"

func TestBodyProtocol(t *testing.T) {
	c := &Call{Protocol: "anthropic"}
	if got := BodyProtocol(c, &Attempt{Candidate: Candidate{Protocol: "openai-chat"}}); got != "openai-chat" {
		t.Errorf("endpoint protocol: %q", got)
	}
	if got := BodyProtocol(c, &Attempt{}); got != "anthropic" {
		t.Errorf("no endpoint protocol: %q", got)
	}
	if got := BodyProtocol(c, nil); got != "anthropic" {
		t.Errorf("no attempt: %q", got)
	}
}
