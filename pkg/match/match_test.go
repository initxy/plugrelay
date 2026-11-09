package match

import "testing"

func TestAny(t *testing.T) {
	cases := []struct {
		name     string
		patterns []string
		want     bool
	}{
		{"openai/gpt-5", []string{"*"}, true},
		{"openai/gpt-5", []string{"openai/*"}, true},
		{"a/b/c", []string{"a*c"}, true},
		{"openai/gpt-5", []string{"anthropic/*"}, false},
		{"GPT-5", []string{" gpt-5 "}, true},
		{"gpt-5", []string{"GPT-*"}, true},
		{"github__delete_repo", []string{"github__delete_*"}, true},
		{"gpt-5", nil, false},
		{"gpt-5", []string{"[bad"}, false},
	}
	for _, c := range cases {
		if got := Any(c.name, c.patterns); got != c.want {
			t.Errorf("Any(%q, %q) = %v, want %v", c.name, c.patterns, got, c.want)
		}
	}
}
