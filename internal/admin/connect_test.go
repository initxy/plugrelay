package admin

import (
	"encoding/base64"
	"net/url"
	"strings"
	"testing"
)

// The CC Switch links follow its v1 deep-link format (docs/user-manual
// 5.3-deeplink and the site integration guide of farion1231/cc-switch,
// November 2026): resource=provider, app, name, endpoint, apiKey,
// apiFormat, optional model and Base64 config.
func TestCCSwitchLinks(t *testing.T) {
	links := map[string]string{}
	for _, c := range connectClients("https://relay.example.com", "macos", "claude-sonnet-4-6", "Acme AI") {
		if c.CCSwitchURL != "" {
			links[c.Client] = c.CCSwitchURL
		}
	}
	want := map[string]struct{ app, endpoint, format string }{
		"claude-code": {"claude", "https://relay.example.com", "anthropic"},
		"codex":       {"codex", "https://relay.example.com/v1", "openai_responses"},
		"gemini-cli":  {"gemini", "https://relay.example.com", "gemini_native"},
	}
	if len(links) != len(want) {
		t.Fatalf("links for %v", links)
	}
	for client, w := range want {
		raw := links[client]
		rest, ok := strings.CutPrefix(raw, "ccswitch://v1/import?")
		if !ok {
			t.Fatalf("%s: %s", client, raw)
		}
		if !strings.Contains(rest, "apiKey="+apiKeyPlaceholder) {
			t.Errorf("%s: key placeholder not literal: %s", client, rest)
		}
		// The console puts the URL-escaped secret in place of the placeholder.
		q, err := url.ParseQuery(strings.ReplaceAll(rest, apiKeyPlaceholder, url.QueryEscape("pr_a+b")))
		if err != nil {
			t.Fatal(err)
		}
		if q.Get("resource") != "provider" || q.Get("app") != w.app || q.Get("name") != "Acme AI" || q.Get("endpoint") != w.endpoint ||
			q.Get("apiKey") != "pr_a+b" || q.Get("apiFormat") != w.format {
			t.Errorf("%s: %v", client, q)
		}
		if cfg := q.Get("config"); cfg != "" {
			b, err := base64.StdEncoding.DecodeString(cfg)
			if err != nil {
				t.Fatalf("%s config: %v", client, err)
			}
			switch client {
			case "claude-code":
				if q.Get("configFormat") != "json" || !strings.Contains(string(b), "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC") {
					t.Errorf("claude config %s", b)
				}
			case "codex":
				// The link's own key is used; the table must not read one
				// from the environment (CC Switch rejects env_http_headers
				// and drops env_key).
				if q.Get("configFormat") != "toml" || !strings.Contains(string(b), `wire_api = "responses"`) || strings.Contains(string(b), "env_") {
					t.Errorf("codex config %s", b)
				}
			}
		}
	}
	if q, _ := url.ParseQuery(strings.TrimPrefix(links["codex"], "ccswitch://v1/import?")); q.Get("model") != "claude-sonnet-4-6" {
		t.Errorf("codex model %q", q.Get("model"))
	}
}
