package admin

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestPublicPages(t *testing.T) {
	e := newEnv(t, sqliteStore, false)
	admin := e.setup()
	admin.must(201, "POST", "/api/v1/models", map[string]any{"name": "m1"})
	admin.must(201, "POST", "/api/v1/pricing-rules", map[string]any{"match": "m1", "prices": map[string]any{"input": 2_000_000, "output": 8_000_000}})
	e.setSite(settingName, "Acme")

	anon := e.browser()
	if code, out := anon.do("GET", "/api/v1/public/catalog", nil); code != 404 || errCode(out) != "public_pages_off" {
		t.Fatalf("off: %d %v", code, out)
	}
	if out := anon.must(200, "GET", "/api/v1/auth/providers", nil); out["public_pages"] != nil {
		t.Fatalf("login page links pages that are off: %v", out)
	}
	admin.must(200, "PATCH", "/api/v1/features/public_pages", map[string]any{"enabled": true})

	if out := anon.must(200, "GET", "/api/v1/auth/providers", nil); out["public_pages"].(map[string]any)["models"] != true {
		t.Fatalf("login page: %v", out)
	}
	cat := anon.must(200, "GET", "/api/v1/public/catalog", nil)
	if cat["site"].(map[string]any)["name"] != "Acme" || cat["currency"] != "CNY" {
		t.Fatalf("catalog %v", cat)
	}
	items := cat["items"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["price"].(map[string]any)["output"] != 8e6 {
		t.Fatalf("items %v", items)
	}
	st := anon.must(200, "GET", "/api/v1/public/status", nil)
	if st["overall"] != "ok" || len(st["items"].([]any)) != 1 {
		t.Fatalf("status %v", st)
	}
	// Nothing about upstreams leaves the public pages.
	for _, path := range []string{"/api/v1/public/catalog", "/api/v1/public/status"} {
		resp, err := http.Get(e.srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if strings.Contains(string(b), "upstream") {
			t.Fatalf("%s mentions upstreams: %s", path, b)
		}
	}

	// Loads per address are limited (60 a minute by default).
	var last int
	for range 60 {
		last, _ = anon.do("GET", "/api/v1/public/catalog", nil)
	}
	if last != 429 {
		t.Fatalf("no limit: %d", last)
	}
}

func TestPlaygroundAPI(t *testing.T) {
	e := newEnv(t, sqliteStore, false)
	admin := e.setup()
	admin.must(201, "POST", "/api/v1/users", map[string]any{"username": "bob", "role": "member", "password": "bobs-password-1"})
	adminKey := admin.must(201, "POST", "/api/v1/keys", map[string]any{"name": "admin-key"})["key"].(map[string]any)["id"].(string)
	bob := e.browser()
	bob.must(200, "POST", "/api/v1/auth/login", map[string]any{"username": "bob", "password": "bobs-password-1"})
	bobKey := bob.must(201, "POST", "/api/v1/keys", map[string]any{"name": "bob-key"})["key"].(map[string]any)["id"].(string)

	msgs := []map[string]any{{"role": "user", "content": "hi"}}
	if code, out := bob.do("POST", "/api/v1/playground", map[string]any{"protocol": "anthropic", "model": "m", "messages": msgs}); code != 422 {
		t.Fatalf("member without key: %d %v", code, out)
	}
	if code, _ := bob.do("POST", "/api/v1/playground", map[string]any{"key_id": adminKey, "protocol": "anthropic", "model": "m", "messages": msgs}); code != 404 {
		t.Fatalf("member with another's key: %d", code)
	}
	if code, _ := bob.do("POST", "/api/v1/playground", map[string]any{"key_id": bobKey, "protocol": "smoke-signals", "model": "m", "messages": msgs}); code != 422 {
		t.Fatalf("unknown protocol: %d", code)
	}

	stream := func(c *client, body string) string {
		req, _ := http.NewRequest("POST", e.srv.URL+"/api/v1/playground", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(CSRFHeader, c.csrfToken())
		resp, err := c.http.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/event-stream" {
			t.Fatalf("status %d", resp.StatusCode)
		}
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}
	out := stream(bob, `{"key_id":"`+bobKey+`","protocol":"openai-chat","model":"m","messages":[{"role":"user","content":"hi"}]}`)
	if !strings.Contains(out, "event: start\n") || !strings.Contains(out, `"text":"bob-key:m"`) || !strings.Contains(out, "event: done\n") {
		t.Fatalf("stream %q", out)
	}
	// An admin may talk without a key.
	if out := stream(admin, `{"protocol":"gemini","model":"m","messages":[{"role":"user","content":"hi"}]}`); !strings.Contains(out, `"text":"playground:m"`) {
		t.Fatalf("admin stream %q", out)
	}
}

// A price group that no longer exists fails both public pages rather than
// showing every model.
func TestPublicPagesMissingPriceGroup(t *testing.T) {
	e := newEnv(t, sqliteStore, false)
	admin := e.setup()
	admin.must(201, "POST", "/api/v1/models", map[string]any{"name": "m1"})
	admin.must(200, "PATCH", "/api/v1/features/public_pages", map[string]any{"enabled": true})
	e.rt.moduleSettings = map[string]map[string]string{publicPagesModule: {"price_group": `"gone"`}}
	anon := e.browser()
	for _, path := range []string{"/api/v1/public/catalog", "/api/v1/public/status"} {
		if code, out := anon.do("GET", path, nil); code != 404 || out["items"] != nil {
			t.Errorf("%s: %d %v", path, code, out)
		}
	}
	// An existing group still narrows the status page to its models.
	admin.must(201, "POST", "/api/v1/models", map[string]any{"name": "m2"})
	g := admin.must(201, "POST", "/api/v1/groups", map[string]any{"name": "public", "models": []string{"m2"}})["id"].(string)
	e.rt.moduleSettings[publicPagesModule]["price_group"] = `"` + g + `"`
	st := anon.must(200, "GET", "/api/v1/public/status", nil)
	if items := st["items"].([]any); len(items) != 1 || items[0].(map[string]any)["model"] != "m2" {
		t.Fatalf("status for the group: %v", items)
	}
}

// The console's model lists read a group's models the way routing does:
// any case, and "*" crossing "/".
func TestModelListMatchesLikeRouting(t *testing.T) {
	if !globMatch("openai/gpt-5", []string{"*"}) || !globMatch("GPT-5", []string{"gpt-*"}) ||
		!globMatch("any", nil) || globMatch("claude", []string{"gpt-*"}) {
		t.Fatal("model list match differs from routing")
	}
}
