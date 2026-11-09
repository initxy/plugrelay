package captcha

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// fakeVerify answers like a siteverify endpoint: "good" passes.
func fakeVerify(t *testing.T, status int) (*httptest.Server, *[]string) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		seen = append(seen, r.Form.Get("secret")+"|"+r.Form.Get("response")+"|"+r.Form.Get("remoteip")+"|"+r.Form.Get("sitekey"))
		if status != 200 {
			w.WriteHeader(status)
			return
		}
		if r.Form.Get("response") == "good" && r.Form.Get("secret") == "s3cret" {
			_, _ = w.Write([]byte(`{"success":true}`))
			return
		}
		if r.Form.Get("response") == "garbage" {
			_, _ = w.Write([]byte(`<html>`))
			return
		}
		_, _ = w.Write([]byte(`{"success":false,"error-codes":["invalid-input-response"]}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &seen
}

func TestVerify(t *testing.T) {
	ctx := context.Background()
	srv, seen := fakeVerify(t, 200)
	for _, p := range []string{Turnstile, HCaptcha} {
		c := Config{Provider: p, SiteKey: "site", Secret: "s3cret", Endpoint: srv.URL}
		if !c.Enabled() {
			t.Fatal("not enabled")
		}
		if err := Verify(ctx, srv.Client(), c, "good", "10.0.0.1"); err != nil {
			t.Fatalf("%s good: %v", p, err)
		}
		if err := Verify(ctx, srv.Client(), c, "bad", ""); !errors.Is(err, ErrFailed) {
			t.Fatalf("%s bad: %v", p, err)
		}
		if err := Verify(ctx, srv.Client(), c, " ", ""); !errors.Is(err, ErrMissing) {
			t.Fatalf("%s missing: %v", p, err)
		}
		if err := Verify(ctx, srv.Client(), c, "garbage", ""); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("%s garbage: %v", p, err)
		}
	}
	if (*seen)[0] != "s3cret|good|10.0.0.1|" || (*seen)[3] != "s3cret|good|10.0.0.1|site" {
		t.Fatalf("forms sent: %v", *seen)
	}
	down, _ := fakeVerify(t, 500)
	if err := Verify(ctx, down.Client(), Config{Provider: Turnstile, SiteKey: "s", Secret: "s3cret", Endpoint: down.URL}, "good", ""); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("500: %v", err)
	}
	if err := Verify(ctx, http.DefaultClient, Config{Provider: Turnstile, SiteKey: "s", Secret: "x", Endpoint: "http://127.0.0.1:1"}, "good", ""); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("unreachable: %v", err)
	}
	if (Config{Provider: Off, SiteKey: "s", Secret: "x"}).Enabled() || (Config{Provider: HCaptcha, SiteKey: "s"}).Enabled() {
		t.Fatal("incomplete config enabled")
	}
}
