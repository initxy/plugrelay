//go:build e2e

package e2e

// The sell kit against a live upstream: a relay with meter.balance on
// refuses a request from an empty balance with the client protocol's own
// out-of-credit error, serves it after a top-up, and takes exactly the
// request's charge off the balance.

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const e2eAdminToken = "e2e-admin-token-0123456789"

func TestSellKitBalance(t *testing.T) {
	upBase, upKey := os.Getenv("PLUGRELAY_E2E_BASE"), os.Getenv("PLUGRELAY_E2E_KEY")
	if upBase == "" || upKey == "" {
		t.Skip("set PLUGRELAY_E2E_BASE and PLUGRELAY_E2E_KEY to run against a live upstream")
	}
	model := envOr("PLUGRELAY_E2E_MODEL", defaultModel)
	r := startWith(t, upBase, []string{"PLUGRELAY_ADMIN_TOKEN=" + e2eAdminToken}, model)

	// Turn the balance on and price the model: 10 USD per million input
	// and output tokens, so a short request costs a few hundred micro-USD.
	doc := filepath.Join(t.TempDir(), "sellkit.yaml")
	_ = os.WriteFile(doc, []byte("features:\n  - {id: billing, enabled: true}\n"+
		"prices:\n  - {model: "+model+", input: 10000000, output: 10000000, currency: USD}\n"), 0o600)
	r.run("apply", "-f", doc)

	chat := map[string]any{"model": model, "max_tokens": 16,
		"messages": []any{map[string]any{"role": "user", "content": prompt}}}

	// Empty balance: OpenAI clients get 429 insufficient_quota, never a
	// retryable error. The plugin applies on the next reload, so wait for it.
	deadline := time.Now().Add(20 * time.Second)
	for {
		code, body := r.raw(t, "/v1/chat/completions", chat)
		if code == http.StatusTooManyRequests && strings.Contains(body, "insufficient_quota") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("empty balance: status %d %s, want 429 insufficient_quota", code, body)
		}
		time.Sleep(500 * time.Millisecond)
	}

	// Top up 1 USD for the key's account (a key without a user pays itself).
	subject := "key:" + r.keyID
	r.admin(t, "POST", "/api/plugins/meter.balance/adjust", map[string]any{
		"subject": subject, "amount_micro": 1_000_000, "note": "e2e", "idempotency_key": "e2e-topup"}, nil)

	resp := r.post(t, "/v1/chat/completions", chat, nil)
	var out struct {
		Choices []struct {
			Message struct{ Content string } `json:"message"`
		} `json:"choices"`
	}
	decode(t, resp, &out)
	if len(out.Choices) == 0 || strings.TrimSpace(out.Choices[0].Message.Content) == "" {
		t.Fatalf("no text after top-up: %+v", out)
	}

	// The balance went down by exactly the request's charge.
	var accts struct {
		Items []struct {
			Subject string `json:"subject"`
			Balance struct {
				Micro int64 `json:"micro"`
			} `json:"balance"`
		} `json:"items"`
	}
	var logs struct {
		Items []struct {
			Status int `json:"status"`
			Charge struct {
				Micro int64 `json:"micro"`
			} `json:"charge"`
		} `json:"items"`
	}
	deadline = time.Now().Add(10 * time.Second) // settle and the request log are written off the request path
	for {
		r.admin(t, "GET", "/api/plugins/meter.balance/accounts", nil, &accts)
		r.admin(t, "GET", "/api/v1/logs?limit=1", nil, &logs)
		var bal int64 = -1
		for _, a := range accts.Items {
			if a.Subject == subject {
				bal = a.Balance.Micro
			}
		}
		if len(logs.Items) == 1 && logs.Items[0].Status == 200 && logs.Items[0].Charge.Micro > 0 &&
			bal == 1_000_000-logs.Items[0].Charge.Micro {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("balance %d, last request %+v; want 1000000 minus its charge", bal, logs.Items)
		}
		time.Sleep(300 * time.Millisecond)
	}
}

// raw posts a JSON request with the platform key and returns status and body.
func (r *relay) raw(t *testing.T, path string, body any) (int, string) {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, r.base+path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+r.key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	return resp.StatusCode, string(msg)
}

// admin calls the admin API with the operator token and decodes into v.
func (r *relay) admin(t *testing.T, method, path string, body, v any) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, r.base+path, rd)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+e2eAdminToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		t.Fatalf("%s %s: %d %s", method, path, resp.StatusCode, data)
	}
	if v != nil {
		if err := json.Unmarshal(data, v); err != nil {
			t.Fatalf("%s %s: %v: %s", method, path, err, data)
		}
	}
}
