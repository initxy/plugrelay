package admin

import (
	"context"
	"encoding/base64"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/initxy/plugrelay/internal/store"
)

func TestSSOPolicyReview(t *testing.T) {
	for name, open := range stores(t) {
		t.Run(name, func(t *testing.T) {
			t.Run("TrustEmail", func(t *testing.T) { testSSOTrustEmail(t, open) })
			t.Run("LinkAdmins", func(t *testing.T) { testSSOLinkAdmins(t, open) })
			t.Run("ClaimRules", func(t *testing.T) { testSSOClaimRules(t, open) })
			t.Run("IssuerChange", func(t *testing.T) { testSSOIssuerChange(t, open) })
			t.Run("CreateRace", func(t *testing.T) { testSSOCreateRace(t, open) })
		})
	}
}

func (e *env) ssoTry(idp *fakeIdP, provider string, claims map[string]any) (*client, string) {
	e.t.Helper()
	idp.set(func(f *fakeIdP) { f.claims = claims })
	e.h.logins.reset("127.0.0.1")
	b := noFollow(e.browser())
	final, _ := b.ssoFlow(provider, "")
	return b, final
}

func userID(b *client) any {
	return b.must(200, "GET", "/api/v1/me", nil)["user"].(map[string]any)["id"]
}

// trust_email fills in only when the IdP is silent; an explicit "not
// verified" is never upgraded.
func testSSOTrustEmail(t *testing.T, open opener) {
	e, idp, admin := ssoEnv(t, open)
	ctx := context.Background()
	carol := &store.User{Name: "Carol", Email: "carol@example.com", Role: store.RoleMember}
	if err := e.st.CreateUser(ctx, carol, "carol-password-1"); err != nil {
		t.Fatal(err)
	}
	admin.must(201, "POST", "/api/v1/sso/providers", idp.provider(map[string]any{"link_by_email": true, "trust_email": true}))
	if _, got := e.ssoTry(idp, "corp", person("m1", "carol@example.com", false)); got != "/login?sso_error=email_unverified" {
		t.Fatalf("explicit false: %q", got)
	}
	silent := map[string]any{"sub": "c1", "email": "carol@example.com", "name": "Carol"}
	b, got := e.ssoTry(idp, "corp", silent)
	if got != "/" || userID(b) != carol.ID {
		t.Fatalf("silent claim with trust_email: %q", got)
	}
	// Feishu/Lark contact emails are admin-typed in any tenant: trusting
	// them needs a tenant allowlist.
	_, out := admin.do("POST", "/api/v1/sso/providers", map[string]any{"id": "fs", "preset": "feishu",
		"client_id": "c", "client_secret": "s", "trust_email": true})
	if !hasField(out, "allowed_tenants") {
		t.Fatalf("feishu trust_email without tenants: %v", out)
	}
	admin.must(201, "POST", "/api/v1/sso/providers", map[string]any{"id": "fs", "preset": "feishu",
		"client_id": "c", "client_secret": "s", "trust_email": true, "allowed_tenants": []string{"t1"}})
	admin.must(422, "PATCH", "/api/v1/sso/providers/fs", map[string]any{"allowed_tenants": []string{}})
}

// link_by_email never reaches an admin unless link_admins says so.
func testSSOLinkAdmins(t *testing.T, open opener) {
	e, idp, admin := ssoEnv(t, open)
	boss := &store.User{Name: "Boss", Email: "boss@example.com", Role: store.RoleAdmin}
	if err := e.st.CreateUser(context.Background(), boss, "boss-password-123"); err != nil {
		t.Fatal(err)
	}
	admin.must(201, "POST", "/api/v1/sso/providers", idp.provider(map[string]any{"link_by_email": true}))
	if _, got := e.ssoTry(idp, "corp", person("b1", "boss@example.com", true)); got != "/login?sso_error=account_exists" {
		t.Fatalf("admin linked by email: %q", got)
	}
	if ids, _ := e.st.ListIdentities(context.Background(), boss.ID); len(ids) != 0 {
		t.Fatalf("admin identity linked: %v", ids)
	}
	admin.must(200, "PATCH", "/api/v1/sso/providers/corp", map[string]any{"link_admins": true})
	b, got := e.ssoTry(idp, "corp", person("b1", "boss@example.com", true))
	if got != "/" || userID(b) != boss.ID {
		t.Fatalf("link_admins on: %q", got)
	}
}

// Group rules may not key on claims the signing-in user can edit.
func testSSOClaimRules(t *testing.T, open opener) {
	e, idp, admin := ssoEnv(t, open)
	g := admin.must(201, "POST", "/api/v1/groups", map[string]any{"name": "VIP"})
	rule := func(claim, value string) []map[string]any {
		return []map[string]any{{"claim": claim, "value": value, "group_id": g["id"]}}
	}
	_, out := admin.do("POST", "/api/v1/sso/providers", map[string]any{"id": "gh", "preset": "github",
		"client_id": idp.clientID, "client_secret": idp.secret, "group_rules": rule("company", "acme")})
	if !hasField(out, "group_rules.0.claim") {
		t.Fatalf("github claim rule accepted: %v", out)
	}
	_, out = admin.do("POST", "/api/v1/sso/providers", map[string]any{"id": "fs", "preset": "lark",
		"client_id": idp.clientID, "client_secret": idp.secret, "group_rules": rule("name", "x")})
	if !hasField(out, "group_rules.0.claim") {
		t.Fatalf("lark name rule accepted: %v", out)
	}
	admin.must(201, "POST", "/api/v1/sso/providers", map[string]any{"id": "fs", "preset": "feishu",
		"client_id": idp.clientID, "client_secret": idp.secret, "group_rules": rule("tenant_key", "t1")})

	// A rule that skipped validation (written to the store directly) is
	// ignored at sign-in.
	admin.must(201, "POST", "/api/v1/sso/providers", map[string]any{"id": "gh", "preset": "github",
		"client_id": idp.clientID, "client_secret": idp.secret, "auto_create": true})
	ctx := context.Background()
	cfg, err := loadSSO(ctx, e.st)
	if err != nil {
		t.Fatal(err)
	}
	_, p := cfg.find("gh")
	p.GroupRules = []ssoGroupRule{{Claim: "company", Value: "acme", GroupID: g["id"].(string)}}
	if err := saveSSO(ctx, e.st, cfg); err != nil {
		t.Fatal(err)
	}
	idp.set(func(f *fakeIdP) {
		f.ghUser = map[string]any{"id": 7, "login": "eve", "company": "acme"}
		f.ghEmails = []map[string]any{{"email": "eve@users.example", "primary": true, "verified": true}}
	})
	b, got := e.ssoTry(idp, "gh", nil)
	if got != "/" {
		t.Fatalf("github sign-in: %q", got)
	}
	if gid := b.must(200, "GET", "/api/v1/me", nil)["user"].(map[string]any)["group_id"]; gid == g["id"] {
		t.Fatal("user-edited company placed the user in a group")
	}
}

// A subject is only someone under the issuer and client it came from.
func testSSOIssuerChange(t *testing.T, open opener) {
	e, idp, admin := ssoEnv(t, open)
	admin.must(201, "POST", "/api/v1/sso/providers", idp.provider(nil))
	b, got := e.ssoTry(idp, "corp", person("42", "a@example.com", true))
	if got != "/" {
		t.Fatalf("first sign-in: %q", got)
	}
	first := userID(b)
	// Unrelated edits keep links.
	admin.must(200, "PATCH", "/api/v1/sso/providers/corp", map[string]any{"name": "Renamed"})
	if b, got := e.ssoTry(idp, "corp", person("42", "a@example.com", true)); got != "/" || userID(b) != first {
		t.Fatalf("after rename: %q", got)
	}
	idp2 := newFakeIdP(t)
	admin.must(200, "PATCH", "/api/v1/sso/providers/corp", map[string]any{"issuer": idp2.srv.URL})
	if ids, _ := e.st.ListIdentities(context.Background(), first.(string)); len(ids) != 0 {
		t.Fatalf("identities kept across issuer change: %v", ids)
	}
	b, got = e.ssoTry(idp2, "corp", person("42", "other@else.example", true))
	if got != "/" || userID(b) == first {
		t.Fatalf("same sub at a new issuer signed in as the old user: %q", got)
	}
	audit := admin.must(200, "GET", "/api/v1/audit?action=sso.provider.update", nil)["items"].([]any)
	found := false
	for _, a := range audit {
		for _, ch := range a.(map[string]any)["changes"].([]any) {
			found = found || ch.(map[string]any)["field"] == "identities_unlinked"
		}
	}
	if !found {
		t.Fatalf("no unlink in audit: %v", audit)
	}
}

// missStore hides a just-made link from the first lookup, as when two
// callbacks for one subject run at once.
type missStore struct {
	store.Store
	missed, missedEmail atomic.Bool
}

func (s *missStore) GetIdentity(ctx context.Context, provider, subject string) (*store.Identity, error) {
	if s.missed.CompareAndSwap(false, true) {
		return nil, store.ErrNotFound
	}
	return s.Store.GetIdentity(ctx, provider, subject)
}

// UserByEmail misses too: the racing winner had not created its user yet.
func (s *missStore) UserByEmail(ctx context.Context, email string) (*store.User, error) {
	if s.missedEmail.CompareAndSwap(false, true) {
		return nil, store.ErrNotFound
	}
	return s.Store.UserByEmail(ctx, email)
}

func testSSOCreateRace(t *testing.T, open opener) {
	// With an email the loser's CreateUser hits the unique address; without
	// one (Feishu, tenant allowlist) its LinkIdentity hits the subject.
	for _, preset := range []string{"oidc", "feishu"} {
		e, idp, admin := ssoEnv(t, open)
		provider := "corp"
		if preset == "oidc" {
			admin.must(201, "POST", "/api/v1/sso/providers", idp.provider(nil))
		} else {
			provider = "fs"
			admin.must(201, "POST", "/api/v1/sso/providers", map[string]any{"id": "fs", "preset": "feishu",
				"client_id": idp.clientID, "client_secret": idp.secret, "auto_create": true, "allowed_tenants": []string{"t1"}})
			idp.set(func(f *fakeIdP) { f.feishu = map[string]any{"open_id": "ou_w", "name": "W", "tenant_key": "t1"} })
		}
		claims := person("w1", "w@example.com", true)
		b, got := e.ssoTry(idp, provider, claims)
		if got != "/" {
			t.Fatalf("%s winner: %q", preset, got)
		}
		winner := userID(b)
		before, _ := e.st.ListUsers(context.Background())
		e.h.d.Store = &missStore{Store: e.st}
		b, got = e.ssoTry(idp, provider, claims)
		e.h.d.Store = e.st
		if got != "/" || userID(b) != winner {
			t.Fatalf("%s loser: %q, want the winner's account", preset, got)
		}
		if after, _ := e.st.ListUsers(context.Background()); len(after) != len(before) {
			t.Fatalf("%s loser left a user behind: %d -> %d", preset, len(before), len(after))
		}
	}
}

func hasField(out map[string]any, path string) bool {
	e, _ := out["error"].(map[string]any)
	fs, _ := e["fields"].([]any)
	for _, f := range fs {
		if f.(map[string]any)["path"] == path {
			return true
		}
	}
	return false
}

func TestJWKSFetchCoalesced(t *testing.T) {
	key := newRSAKey(t)
	var hits atomic.Int32
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		<-release
		writeJSON(w, 200, map[string]any{"keys": []map[string]any{{"kty": "RSA", "kid": "k1",
			"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()), "e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes())}}})
	}))
	defer srv.Close()
	var c ssoCache
	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for range 20 {
		wg.Go(func() {
			_, err := c.key(context.Background(), idpClient{hc: srv.Client()}, srv.URL, "k1", time.Now())
			errs <- err
		})
	}
	time.Sleep(100 * time.Millisecond)
	close(release)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if n := hits.Load(); n != 1 {
		t.Fatalf("JWKS fetched %d times, want 1", n)
	}
}

func TestSSOStartRateLimited(t *testing.T) {
	e, idp, admin := ssoEnv(t, sqliteStore)
	admin.must(201, "POST", "/api/v1/sso/providers", idp.provider(nil))
	b := noFollow(e.browser())
	for i := range ssoMaxStarts {
		if loc := b.get(e.srv.URL + "/api/v1/auth/sso/corp/start").Header.Get("Location"); loc == "/login?sso_error=rate_limited" {
			t.Fatalf("limited after %d starts", i)
		}
	}
	if loc := b.get(e.srv.URL + "/api/v1/auth/sso/corp/start").Header.Get("Location"); loc != "/login?sso_error=rate_limited" {
		t.Fatalf("start %d: %q", ssoMaxStarts+1, loc)
	}
}
