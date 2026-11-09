package signup

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/initxy/plugrelay/plugins/internal/testkit"
)

type accounts map[string]string

func (a accounts) CreateUser(_ context.Context, email, _, _, group string) (string, error) {
	if _, ok := a[email]; ok {
		return "", ErrExists
	}
	a[email] = group
	return "u-" + email, nil
}

func setup(t *testing.T, set testkit.Settings) (*Plugin, accounts) {
	acc := accounts{}
	kv := testkit.NewState(nil)
	p := New(acc, kv)
	if err := p.Init(t.Context(), &testkit.Host{Store: kv, Set: set}); err != nil {
		t.Fatal(err)
	}
	return p, acc
}

func register(p *Plugin, ip, body string) int {
	req := httptest.NewRequest("POST", "/api/plugins/admin.signup/register", strings.NewReader(body))
	req.RemoteAddr = ip + ":1234"
	rec := httptest.NewRecorder()
	p.postRegister(rec, req)
	return rec.Code
}

func TestRoutesArePublic(t *testing.T) {
	p, _ := setup(t, nil)
	for _, rt := range p.Routes() {
		if !rt.Public {
			t.Errorf("%s %s is not public", rt.Method, rt.Path)
		}
	}
}

func TestRegister(t *testing.T) {
	p, acc := setup(t, testkit.Settings{"allowed_domains": []string{"example.com"}, "default_group": "trial", "per_ip_per_hour": 4})
	for _, c := range []struct {
		body string
		want int
	}{
		{`{"email":"ann@example.com","password":"correct-horse"}`, http.StatusCreated},
		{`{"email":"ann@example.com","password":"correct-horse"}`, http.StatusConflict},
		{`{"email":"bob@other.org","password":"correct-horse"}`, http.StatusForbidden},
		{`{"email":"not-an-email","password":"correct-horse"}`, http.StatusBadRequest},
	} {
		if got := register(p, "10.0.0.1", c.body); got != c.want {
			t.Errorf("%s: %d, want %d", c.body, got, c.want)
		}
	}
	if acc["ann@example.com"] != "trial" {
		t.Fatalf("accounts: %v", acc)
	}
	if got := register(p, "10.0.0.2", `{"email":"cy@example.com","password":"short"}`); got != http.StatusBadRequest {
		t.Fatalf("short password: %d", got)
	}
	if got := register(p, "10.0.0.1", `{"email":"dee@example.com","password":"correct-horse"}`); got != http.StatusTooManyRequests {
		t.Fatalf("5th attempt from one address: %d", got)
	}
}

func TestInviteRequired(t *testing.T) {
	p, _ := setup(t, testkit.Settings{"invite_required": true, "invite_codes": "alpha, beta"})
	if got := register(p, "10.0.0.1", `{"email":"a@x.io","password":"correct-horse"}`); got != http.StatusForbidden {
		t.Fatalf("no invite: %d", got)
	}
	if got := register(p, "10.0.0.1", `{"email":"a@x.io","password":"correct-horse","invite_code":"gamma"}`); got != http.StatusForbidden {
		t.Fatalf("wrong invite: %d", got)
	}
	if got := register(p, "10.0.0.1", `{"email":"a@x.io","password":"correct-horse","invite_code":"beta"}`); got != http.StatusCreated {
		t.Fatalf("invite: %d", got)
	}
}

// fakeSiteverify answers like Turnstile/hCaptcha siteverify; status 0
// means 200.
func fakeSiteverify(t *testing.T, status *atomic.Int32) string {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s := status.Load(); s != 0 {
			w.WriteHeader(int(s))
			return
		}
		_ = r.ParseForm()
		ok := r.Form.Get("secret") == "cap-secret" && r.Form.Get("response") == "human"
		w.Header().Set("Content-Type", "application/json")
		if ok {
			_, _ = w.Write([]byte(`{"success":true}`))
		} else {
			_, _ = w.Write([]byte(`{"success":false,"error-codes":["invalid-input-response"]}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestCaptcha(t *testing.T) {
	for _, provider := range []string{"turnstile", "hcaptcha"} {
		t.Run(provider, func(t *testing.T) {
			var status atomic.Int32
			p, acc := setup(t, testkit.Settings{"captcha_provider": provider, "captcha_site_key": "site", "captcha_secret": "cap-secret", "per_ip_per_hour": 100})
			p.captchaEndpoint = fakeSiteverify(t, &status)
			rec := httptest.NewRecorder()
			p.getConfig(rec, httptest.NewRequest("GET", "/config", nil))
			if !strings.Contains(rec.Body.String(), `"captcha":{"provider":"`+provider+`","site_key":"site"}`) || strings.Contains(rec.Body.String(), "cap-secret") {
				t.Fatalf("config: %s", rec.Body)
			}
			for _, c := range []struct {
				token string
				want  int
				down  bool
			}{
				{"", http.StatusForbidden, false},
				{"robot", http.StatusForbidden, false},
				{"human", http.StatusServiceUnavailable, true},
				{"human", http.StatusCreated, false},
			} {
				if c.down {
					status.Store(500)
				} else {
					status.Store(0)
				}
				body := `{"email":"ann@example.com","password":"correct-horse","captcha_token":"` + c.token + `"}`
				if got := register(p, "10.0.0.1", body); got != c.want {
					t.Fatalf("token %q down=%v: %d, want %d", c.token, c.down, got, c.want)
				}
			}
			if len(acc) != 1 {
				t.Fatalf("accounts %v", acc)
			}
		})
	}
	// Off by default: no captcha in the config, none asked.
	p, _ := setup(t, nil)
	rec := httptest.NewRecorder()
	p.getConfig(rec, httptest.NewRequest("GET", "/config", nil))
	if strings.Contains(rec.Body.String(), "captcha") {
		t.Fatalf("config: %s", rec.Body)
	}
	if got := register(p, "10.0.0.9", `{"email":"bo@example.com","password":"correct-horse"}`); got != http.StatusCreated {
		t.Fatalf("no captcha configured: %d", got)
	}
	// Chosen but missing its secret: sign-up refuses instead of skipping it.
	p, acc := setup(t, testkit.Settings{"captcha_provider": "turnstile", "captcha_site_key": "site"})
	if got := register(p, "10.0.0.9", `{"email":"cy@example.com","password":"correct-horse"}`); got != http.StatusServiceUnavailable || len(acc) != 0 {
		t.Fatalf("misconfigured captcha: %d, accounts %v", got, acc)
	}
}
