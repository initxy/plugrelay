package admin

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/initxy/plugrelay/internal/secret"
	"github.com/initxy/plugrelay/internal/store"
	"github.com/initxy/plugrelay/pkg/plugin"
)

// ---------------------------------------------------------------- unit

func TestHOTPVectors(t *testing.T) {
	// RFC 6238 appendix B (SHA-1), last six digits.
	key := []byte("12345678901234567890")
	for _, v := range []struct {
		at   int64
		code string
	}{{59, "287082"}, {1111111109, "081804"}, {1111111111, "050471"}, {1234567890, "005924"}, {2000000000, "279037"}} {
		if got := hotp(key, v.at/30); got != v.code {
			t.Errorf("t=%d: %s, want %s", v.at, got, v.code)
		}
	}
	now := time.Unix(1111111109, 0)
	if step, ok := matchTOTP(key, "081 804", now); !ok || step != 1111111109/30 {
		t.Fatalf("match current: %d %v", step, ok)
	}
	if _, ok := matchTOTP(key, hotp(key, totpStep(now)-2), now); ok {
		t.Fatal("code two steps old accepted")
	}
	if _, ok := matchTOTP(key, hotp(key, totpStep(now)+1), now); !ok {
		t.Fatal("next step refused")
	}
	u := otpauthURI("My Relay", "ann@example.com", key)
	if !strings.HasPrefix(u, "otpauth://totp/My%20Relay:ann@example.com?") || !strings.Contains(u, "secret=GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ") {
		t.Fatalf("uri %s", u)
	}
	codes, _ := newRecoveryCodes()
	if len(codes) != 10 || len(codes[0]) != 14 || codes[0] == codes[1] {
		t.Fatalf("codes %v", codes)
	}
	if hashRecoveryCode("u", strings.ToUpper(codes[0])) != hashRecoveryCode("u", codes[0]) || hashRecoveryCode("v", codes[0]) == hashRecoveryCode("u", codes[0]) {
		t.Fatal("recovery hash")
	}
}

func TestCBORRejectsMalformed(t *testing.T) {
	for _, b := range [][]byte{{}, {0x5f}, {0x9f}, {0x59, 0xff}, {0xa1, 0x01}, {0xa2, 0x01, 0x01, 0x01, 0x02}, {0x1b, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}, {0x7a, 0xff, 0xff, 0xff, 0xff}} {
		if _, _, err := cborDecode(b, 0); err == nil {
			t.Errorf("% x decoded", b)
		}
	}
	deep := make([]byte, 40)
	for i := range deep {
		deep[i] = 0x81
	}
	if _, _, err := cborDecode(deep, 0); err == nil {
		t.Fatal("deep nesting decoded")
	}
}

// ---------------------------------------------------------------- harness

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func mfaEnv(t *testing.T, open opener) (*env, *fakeClock, *client) {
	clk := &fakeClock{t: time.Now()}
	e := newEnv(t, open, false, func(d *Deps) { d.Now = clk.now })
	e.setSite(SettingPublicURL, e.srv.URL)
	return e, clk, e.setup()
}

func (c *client) cookie(name string) string {
	u, _ := url.Parse(c.e.srv.URL + Prefix + "/auth/")
	for _, ck := range c.http.Jar.Cookies(u) {
		if ck.Name == name {
			return ck.Value
		}
	}
	return ""
}

func (c *client) dropReauth() {
	c.e.t.Helper()
	if err := c.e.st.KV(mfaKV).Delete(context.Background(), "reauth:"+secret.HashKey(c.cookie(SessionCookie))); err != nil {
		c.e.t.Fatal(err)
	}
}

// wrongTOTP returns a code matchTOTP rejects at now; a fixed "wrong" code
// is valid for about one random key in 300,000.
func wrongTOTP(key []byte, now time.Time) string {
	for i := 0; ; i++ {
		c := fmt.Sprintf("%06d", i)
		if _, ok := matchTOTP(key, c, now); !ok {
			return c
		}
	}
}

func enrollTOTP(t *testing.T, c *client, clk *fakeClock) ([]byte, []string) {
	t.Helper()
	setup := c.must(200, "POST", "/api/v1/me/totp/setup", nil)
	key, err := b32.DecodeString(setup["secret"].(string))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(setup["otpauth_uri"].(string), "otpauth://totp/") {
		t.Fatalf("setup %v", setup)
	}
	if code, out := c.do("POST", "/api/v1/me/totp", map[string]any{"code": wrongTOTP(key, clk.now())}); code != 422 {
		t.Fatalf("wrong enable code: %d %v", code, out)
	}
	out := c.must(200, "POST", "/api/v1/me/totp", map[string]any{"code": hotp(key, totpStep(clk.now()))})
	var codes []string
	for _, v := range out["recovery_codes"].([]any) {
		codes = append(codes, v.(string))
	}
	if len(codes) != recoveryCount {
		t.Fatalf("codes %v", out)
	}
	return key, codes
}

func (e *env) loginStep1(user, pw string) (*client, int, map[string]any) {
	b := e.browser()
	code, out := b.do("POST", "/api/v1/auth/login", map[string]any{"username": user, "password": pw})
	return b, code, out
}

func TestMFA(t *testing.T) {
	for name, open := range stores(t) {
		t.Run(name, func(t *testing.T) {
			t.Run("TOTP", func(t *testing.T) { testMFATOTP(t, open) })
			t.Run("Passkeys", func(t *testing.T) { testMFAPasskeys(t, open) })
			t.Run("Policy", func(t *testing.T) { testMFAPolicy(t, open) })
			t.Run("PolicyPlugins", func(t *testing.T) { testMFAPolicyPlugins(t, open) })
			t.Run("AdminReauth", func(t *testing.T) { testAdminReauth(t, open) })
			t.Run("OwnPassword", func(t *testing.T) { testOwnPassword(t, open) })
			t.Run("ReenableAdminAndEmail", func(t *testing.T) { testReenableAdminAndEmail(t, open) })
			t.Run("PasskeyOptionsLimit", func(t *testing.T) { testPasskeyOptionsLimit(t, open) })
			t.Run("SSO", func(t *testing.T) { testMFASSO(t, open) })
			t.Run("LoginCaptcha", func(t *testing.T) { testLoginCaptcha(t, open) })
		})
	}
}

// testOwnPassword: anyone signed in changes their own password after
// confirming it is them; too short a password is refused first.
func testOwnPassword(t *testing.T, open opener) {
	e, _, admin := mfaEnv(t, open)
	admin.dropReauth()
	if code, out := admin.do("POST", "/api/v1/me/password", map[string]any{"password": "short"}); code != 422 {
		t.Fatalf("short password: %d %v", code, out)
	}
	if code, out := admin.do("POST", "/api/v1/me/password", map[string]any{"password": "brand-new-pass-1"}); code != 403 || errCode(out) != "reauth_required" {
		t.Fatalf("without reauth: %d %v", code, out)
	}
	admin.must(200, "POST", "/api/v1/me/reauth", map[string]any{"method": "password", "password": "correct-horse-1"})
	admin.must(204, "POST", "/api/v1/me/password", map[string]any{"password": "brand-new-pass-1"})
	if _, code, _ := e.loginStep1("admin", "correct-horse-1"); code != 401 {
		t.Fatalf("old password still works: %d", code)
	}
	if _, code, _ := e.loginStep1("admin", "brand-new-pass-1"); code != 200 {
		t.Fatalf("new password login: %d", code)
	}
}

// testAdminReauth: password, role and second-factor changes made through
// the user admin routes need a recent re-authentication, the admin's own
// account included.
func testAdminReauth(t *testing.T, open opener) {
	e, _, admin := mfaEnv(t, open)
	me := admin.must(200, "GET", "/api/v1/me", nil)["user"].(map[string]any)["id"].(string)
	bob := admin.must(201, "POST", "/api/v1/users", map[string]any{"username": "bob", "role": "member", "password": "bobs-password-1"})["id"].(string)
	admin.dropReauth()
	for _, tc := range []struct {
		method, path string
		body         map[string]any
	}{
		{"PATCH", "/api/v1/users/" + me, map[string]any{"password": "another-password-1"}},
		{"PATCH", "/api/v1/users/" + bob, map[string]any{"password": "another-password-1"}},
		{"PATCH", "/api/v1/users/" + bob, map[string]any{"role": "admin"}},
		{"DELETE", "/api/v1/users/" + bob + "/mfa", nil},
		{"POST", "/api/v1/users", map[string]any{"username": "mallory", "role": "admin", "password": "mallory-password-1"}},
		{"PATCH", "/api/v1/users/" + bob, map[string]any{"status": "disabled"}},
		{"DELETE", "/api/v1/users/" + bob, nil},
	} {
		if code, out := admin.do(tc.method, tc.path, tc.body); code != 403 || errCode(out) != "reauth_required" {
			t.Fatalf("%s %s %v without reauth: %d %v", tc.method, tc.path, tc.body, code, out)
		}
	}
	if _, code, _ := e.loginStep1("mallory", "mallory-password-1"); code != 401 {
		t.Fatalf("admin created without reauth can sign in: %d", code)
	}
	// Other fields need none.
	admin.must(200, "PATCH", "/api/v1/users/"+bob, map[string]any{"display_name": "Bob", "role": "member", "status": "active"})
	admin.must(201, "POST", "/api/v1/users", map[string]any{"username": "carol", "role": "member"})
	admin.must(200, "POST", "/api/v1/me/reauth", map[string]any{"method": "password", "password": "correct-horse-1"})
	admin.must(200, "PATCH", "/api/v1/users/"+bob, map[string]any{"role": "admin"})
	admin.must(204, "DELETE", "/api/v1/users/"+bob+"/mfa", nil)
	admin.must(201, "POST", "/api/v1/users", map[string]any{"username": "dave", "role": "admin"})
	admin.must(200, "PATCH", "/api/v1/users/"+bob, map[string]any{"status": "disabled"})
	admin.must(204, "DELETE", "/api/v1/users/"+bob, nil)
	admin.must(200, "PATCH", "/api/v1/users/"+me, map[string]any{"password": "another-password-1"})
	if _, code, _ := e.loginStep1("admin", "another-password-1"); code != 200 {
		t.Fatalf("new password login: %d", code)
	}
}

// roleRuntime serves one admin-role plugin route that records the role
// header it was called with.
type roleRuntime struct {
	*fakeRuntime
	role *string
}

func (p roleRuntime) Plugins() []PluginEntry {
	return []PluginEntry{{Info: plugin.Info{ID: "admin.demo", Kind: plugin.KindAdmin, Version: "0.1.0"},
		Source: SourceBuiltin, Enabled: true,
		Routes: []plugin.AdminRoute{{Method: "POST", Path: "/mint", Role: "admin", Handler: func(w http.ResponseWriter, r *http.Request) {
			*p.role = r.Header.Get("X-Plugrelay-Role")
			_, _ = w.Write([]byte(`{"ok":true}`))
		}}}}}
}

// testMFAPolicyPlugins: an admin past the enrollment deadline holds no
// admin power on plugin routes either.
func testMFAPolicyPlugins(t *testing.T, open opener) {
	e, clk, admin := mfaEnv(t, open)
	var role string
	e.h.d.Runtime = roleRuntime{fakeRuntime: e.rt, role: &role}
	admin.must(200, "PATCH", "/api/v1/settings", map[string]any{"values": map[string]any{
		"security.require_2fa_admins": true, "security.require_2fa_grace_days": 1}})
	admin.must(200, "POST", "/api/plugins/admin.demo/mint", map[string]any{})
	if role != "admin" {
		t.Fatalf("inside grace role %q", role)
	}
	clk.add(49 * time.Hour)
	role = ""
	if code, out := admin.do("POST", "/api/plugins/admin.demo/mint", map[string]any{}); code != 403 || role != "" {
		t.Fatalf("past grace, session: %d %v role %q", code, out, role)
	}
	enrollTOTP(t, admin, clk)
	admin.must(200, "POST", "/api/plugins/admin.demo/mint", map[string]any{})
}

// testPasskeyOptionsLimit: unauthenticated passwordless challenges are
// bounded per address, since each one stores a row.
func testPasskeyOptionsLimit(t *testing.T, open opener) {
	e, _, _ := mfaEnv(t, open)
	b := e.browser()
	for i := range ssoMaxStarts {
		if code, out := b.do("POST", "/api/v1/auth/passkey/options", map[string]any{"purpose": "login"}); code != 200 {
			t.Fatalf("challenge %d: %d %v", i, code, out)
		}
	}
	if code, out := b.do("POST", "/api/v1/auth/passkey/options", map[string]any{"purpose": "login"}); code != 429 || errCode(out) != "rate_limited" {
		t.Fatalf("past the limit: %d %v", code, out)
	}
}

// ---------------------------------------------------------------- TOTP

func testMFATOTP(t *testing.T, open opener) {
	e, clk, admin := mfaEnv(t, open)
	key, codes := enrollTOTP(t, admin, clk)
	if code, _ := admin.do("POST", "/api/v1/me/totp/setup", nil); code != 200 {
		t.Fatalf("re-setup: %d", code)
	}
	if me := admin.must(200, "GET", "/api/v1/me", nil); me["mfa"].(map[string]any)["enabled"] != true {
		t.Fatalf("me %v", me)
	}
	// Password alone yields no session.
	b, code, out := e.loginStep1("admin", "correct-horse-1")
	if code != http.StatusAccepted || out["mfa_required"] != true || b.cookie(SessionCookie) != "" || b.cookie(mfaCookie) == "" {
		t.Fatalf("step 1: %d %v", code, out)
	}
	if code, _ := b.do("GET", "/api/v1/me", nil); code != 401 {
		t.Fatalf("me before second factor: %d", code)
	}
	if got := b.must(200, "GET", "/api/v1/auth/mfa", nil); len(got["methods"].([]any)) != 2 {
		t.Fatalf("methods %v", got)
	}
	// The code used to enable is spent: replay refused.
	if code, out := b.do("POST", "/api/v1/auth/mfa", map[string]any{"method": "totp", "code": hotp(key, totpStep(clk.now()))}); code != 401 || errCode(out) != "mfa_invalid" {
		t.Fatalf("replayed enable code: %d %v", code, out)
	}
	clk.add(30 * time.Second)
	cur := hotp(key, totpStep(clk.now()))
	b.must(200, "POST", "/api/v1/auth/mfa", map[string]any{"method": "totp", "code": cur})
	if b.cookie(SessionCookie) == "" || b.cookie(mfaCookie) != "" {
		t.Fatal("no session after second factor")
	}
	b.must(200, "GET", "/api/v1/me", nil)

	// Same code, new sign-in: one use per step.
	b2, _, _ := e.loginStep1("admin", "correct-horse-1")
	if code, _ := b2.do("POST", "/api/v1/auth/mfa", map[string]any{"method": "totp", "code": cur}); code != 401 {
		t.Fatalf("code reused across sign-ins: %d", code)
	}
	// Recovery codes work once.
	b2.must(200, "POST", "/api/v1/auth/mfa", map[string]any{"method": "recovery", "code": strings.ToUpper(codes[0])})
	b3, _, _ := e.loginStep1("admin", "correct-horse-1")
	if code, _ := b3.do("POST", "/api/v1/auth/mfa", map[string]any{"method": "recovery", "code": codes[0]}); code != 401 {
		t.Fatalf("recovery code reused: %d", code)
	}
	// Five tries per pending sign-in.
	for range mfaMaxTries - 1 {
		b3.do("POST", "/api/v1/auth/mfa", map[string]any{"method": "totp", "code": wrongTOTP(key, clk.now())})
	}
	if code, out := b3.do("POST", "/api/v1/auth/mfa", map[string]any{"method": "recovery", "code": codes[1]}); code != 401 || errCode(out) != "mfa_expired" {
		t.Fatalf("after max tries: %d %v", code, out)
	}
	// Wrong codes feed the login limiter (per address and per account).
	clk.add(loginWindow + time.Second)
	for range loginMaxFails {
		bx, _, _ := e.loginStep1("admin", "correct-horse-1")
		bx.do("POST", "/api/v1/auth/mfa", map[string]any{"method": "totp", "code": "000001"})
	}
	if _, code, _ := e.loginStep1("admin", "correct-horse-1"); code != 429 {
		t.Fatalf("after %d wrong codes, login: %d", loginMaxFails, code)
	}
	clk.add(loginWindow + time.Second)

	// Disabling needs a recent re-authentication.
	admin.dropReauth()
	if code, out := admin.do("DELETE", "/api/v1/me/totp", nil); code != 403 || errCode(out) != "reauth_required" {
		t.Fatalf("disable without reauth: %d %v", code, out)
	}
	if code, out := admin.do("POST", "/api/v1/me/reauth", map[string]any{"method": "password", "password": "nope"}); code != 401 || errCode(out) != "reauth_failed" {
		t.Fatalf("bad reauth: %d %v", code, out)
	}
	clk.add(30 * time.Second)
	admin.must(200, "POST", "/api/v1/me/reauth", map[string]any{"method": "totp", "code": hotp(key, totpStep(clk.now()))})
	fresh := admin.must(200, "POST", "/api/v1/me/recovery-codes", nil)
	if len(fresh["recovery_codes"].([]any)) != recoveryCount {
		t.Fatalf("regenerate %v", fresh)
	}
	admin.must(204, "DELETE", "/api/v1/me/totp", nil)
	if _, code, _ := e.loginStep1("admin", "correct-horse-1"); code != 200 {
		t.Fatalf("login after disable: %d", code)
	}

	// Operator tokens have no account to protect.
	bearer := &client{e: e, http: http.DefaultClient, auth: adminToken}
	if code, _ := bearer.do("POST", "/api/v1/me/totp/setup", nil); code != 409 {
		t.Fatalf("bearer setup: %d", code)
	}
	// The secret is sealed at rest.
	m, _ := store.MFAOf(e.st)
	enrollTOTP(t, admin, clk)
	users, _ := m.MFAUsers(context.Background())
	if len(users) != 1 {
		t.Fatalf("mfa users %v", users)
	}
}

// ---------------------------------------------------------------- passkeys

// cborEnc encodes the subset softKey needs; maps are [k, v, k, v…].
type cborMap []any

func cborEnc(v any) []byte {
	head := func(major byte, n uint64) []byte {
		switch {
		case n < 24:
			return []byte{major<<5 | byte(n)}
		case n < 256:
			return []byte{major<<5 | 24, byte(n)}
		case n < 65536:
			return []byte{major<<5 | 25, byte(n >> 8), byte(n)}
		}
		b := []byte{major<<5 | 26, 0, 0, 0, 0}
		binary.BigEndian.PutUint32(b[1:], uint32(n))
		return b
	}
	switch x := v.(type) {
	case int:
		if x < 0 {
			return head(1, uint64(-1-x))
		}
		return head(0, uint64(x))
	case []byte:
		return append(head(2, uint64(len(x))), x...)
	case string:
		return append(head(3, uint64(len(x))), x...)
	case cborMap:
		out := head(5, uint64(len(x)/2))
		for _, e := range x {
			out = append(out, cborEnc(e)...)
		}
		return out
	}
	panic("cborEnc")
}

// softKey is a software authenticator.
type softKey struct {
	alg    int
	ec     *ecdsa.PrivateKey
	ed     ed25519.PrivateKey
	rsa    *rsa.PrivateKey
	credID []byte
	count  uint32
}

func newSoftKey(t *testing.T, alg int) *softKey {
	k := &softKey{alg: alg, credID: make([]byte, 16)}
	_, _ = rand.Read(k.credID)
	var err error
	switch alg {
	case coseES256:
		k.ec, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	case coseEdDSA:
		_, k.ed, err = ed25519.GenerateKey(rand.Reader)
	case coseRS256:
		k.rsa, err = rsa.GenerateKey(rand.Reader, 2048)
	}
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func (k *softKey) cose() []byte {
	switch k.alg {
	case coseES256:
		b, _ := k.ec.PublicKey.Bytes()
		return cborEnc(cborMap{1, 2, 3, coseES256, -1, 1, -2, b[1:33], -3, b[33:]})
	case coseEdDSA:
		return cborEnc(cborMap{1, 1, 3, coseEdDSA, -1, 6, -2, []byte(k.ed.Public().(ed25519.PublicKey))})
	}
	return cborEnc(cborMap{1, 3, 3, coseRS256, -1, k.rsa.N.Bytes(), -2, []byte{1, 0, 1}})
}

func (k *softKey) sign(msg []byte) []byte {
	h := sha256.Sum256(msg)
	switch k.alg {
	case coseES256:
		s, _ := ecdsa.SignASN1(rand.Reader, k.ec, h[:])
		return s
	case coseEdDSA:
		return ed25519.Sign(k.ed, msg)
	}
	s, _ := rsa.SignPKCS1v15(rand.Reader, k.rsa, crypto.SHA256, h[:])
	return s
}

func authDataFor(rpID string, flags byte, count uint32, attested []byte) []byte {
	h := sha256.Sum256([]byte(rpID))
	b := append(h[:], flags, 0, 0, 0, 0)
	binary.BigEndian.PutUint32(b[33:], count)
	return append(b, attested...)
}

func clientJSON(typ, challenge, origin string) []byte {
	b, _ := json.Marshal(map[string]any{"type": typ, "challenge": challenge, "origin": origin})
	return b
}

func (k *softKey) register(rpID, origin, challenge string) map[string]any {
	att := append(make([]byte, 16), byte(len(k.credID)>>8), byte(len(k.credID)))
	att = append(append(att, k.credID...), k.cose()...)
	ad := authDataFor(rpID, flagUP|flagUV|flagAT, k.count, att)
	obj := cborEnc(cborMap{"fmt", "none", "attStmt", cborMap{}, "authData", ad})
	return map[string]any{"id": enc64(k.credID), "rawId": enc64(k.credID), "type": "public-key",
		"response": map[string]any{"clientDataJSON": enc64(clientJSON("webauthn.create", challenge, origin)),
			"attestationObject": enc64(obj), "transports": []string{"internal"}}}
}

type assertOpts struct {
	rpID, origin, typ string
	flags             byte
	count             *uint32
	userHandle        string
	badSig            bool
}

func (k *softKey) assert(challenge string, o assertOpts) map[string]any {
	if o.typ == "" {
		o.typ = "webauthn.get"
	}
	if o.count == nil {
		k.count++
		o.count = &k.count
	}
	ad := authDataFor(o.rpID, o.flags, *o.count, nil)
	cdj := clientJSON(o.typ, challenge, o.origin)
	h := sha256.Sum256(cdj)
	sig := k.sign(append(append([]byte{}, ad...), h[:]...))
	if o.badSig {
		sig[len(sig)-1] ^= 1
	}
	resp := map[string]any{"clientDataJSON": enc64(cdj), "authenticatorData": enc64(ad), "signature": enc64(sig)}
	if o.userHandle != "" {
		resp["userHandle"] = o.userHandle
	}
	return map[string]any{"id": enc64(k.credID), "rawId": enc64(k.credID), "type": "public-key", "response": resp}
}

func (e *env) rp() (string, string) {
	u, _ := url.Parse(e.srv.URL)
	return u.Hostname(), e.srv.URL
}

func registerKey(t *testing.T, e *env, c *client, k *softKey, name string) map[string]any {
	t.Helper()
	rpID, origin := e.rp()
	opts := c.must(200, "POST", "/api/v1/me/passkeys/options", nil)
	if opts["rp"].(map[string]any)["id"] != rpID || opts["attestation"] != "none" {
		t.Fatalf("options %v", opts)
	}
	return c.must(201, "POST", "/api/v1/me/passkeys", map[string]any{"name": name, "credential": k.register(rpID, origin, opts["challenge"].(string))})
}

func testMFAPasskeys(t *testing.T, open opener) {
	e, _, admin := mfaEnv(t, open)
	rpID, origin := e.rp()
	es := newSoftKey(t, coseES256)
	first := registerKey(t, e, admin, es, "laptop")
	if len(first["recovery_codes"].([]any)) != recoveryCount {
		t.Fatalf("first passkey returns recovery codes: %v", first)
	}
	if second := registerKey(t, e, admin, newSoftKey(t, coseEdDSA), "phone"); second["recovery_codes"] != nil {
		t.Fatal("second passkey replaced recovery codes")
	}
	rs := newSoftKey(t, coseRS256)
	registerKey(t, e, admin, rs, "")

	// Registration checks: bad origin, other RP, reused challenge, duplicate.
	opts := admin.must(200, "POST", "/api/v1/me/passkeys/options", nil)
	ch := opts["challenge"].(string)
	if code, out := admin.do("POST", "/api/v1/me/passkeys", map[string]any{"credential": newSoftKey(t, coseES256).register(rpID, "https://evil.example", ch)}); code != 422 {
		t.Fatalf("bad origin: %d %v", code, out)
	}
	if code, _ := admin.do("POST", "/api/v1/me/passkeys", map[string]any{"credential": newSoftKey(t, coseES256).register("evil.example", origin, ch)}); code != 422 {
		t.Fatalf("other RP: %d", code)
	}
	opts = admin.must(200, "POST", "/api/v1/me/passkeys/options", nil)
	if code, _ := admin.do("POST", "/api/v1/me/passkeys", map[string]any{"credential": newSoftKey(t, coseES256).register("evil.example", origin, opts["challenge"].(string))}); code != 422 {
		t.Fatalf("other RP: %d", code)
	}
	// The challenge was spent by the failed attempt.
	if code, _ := admin.do("POST", "/api/v1/me/passkeys", map[string]any{"credential": newSoftKey(t, coseES256).register(rpID, origin, opts["challenge"].(string))}); code != 422 {
		t.Fatalf("challenge reused: %d", code)
	}
	opts = admin.must(200, "POST", "/api/v1/me/passkeys/options", nil)
	if code, _ := admin.do("POST", "/api/v1/me/passkeys", map[string]any{"credential": es.register(rpID, origin, opts["challenge"].(string))}); code != 409 {
		t.Fatalf("duplicate credential: %d", code)
	}

	sec := admin.must(200, "GET", "/api/v1/me/security", nil)
	pks := sec["passkeys"].([]any)
	if len(pks) != 3 || pks[0].(map[string]any)["name"] != "laptop" || pks[2].(map[string]any)["name"] != "Passkey 3" {
		t.Fatalf("security %v", sec)
	}
	esID := pks[0].(map[string]any)["id"].(string)

	login := func(b *client, k *softKey, o assertOpts) (int, map[string]any) {
		opts := b.must(200, "POST", "/api/v1/auth/passkey/options", map[string]any{"purpose": "login"})
		if opts["userVerification"] != "required" {
			t.Fatalf("login options %v", opts)
		}
		if o.rpID == "" {
			o.rpID = rpID
		}
		if o.origin == "" {
			o.origin = origin
		}
		return b.do("POST", "/api/v1/auth/passkey", map[string]any{"credential": k.assert(opts["challenge"].(string), o)})
	}
	uv := byte(flagUP | flagUV)
	// Passwordless sign-in with each algorithm.
	for _, k := range []*softKey{es, rs} {
		b := e.browser()
		if code, out := login(b, k, assertOpts{flags: uv}); code != 200 {
			t.Fatalf("passkey login alg %d: %d %v", k.alg, code, out)
		}
		b.must(200, "GET", "/api/v1/me", nil)
	}
	for name, o := range map[string]assertOpts{
		"bad origin":     {flags: uv, origin: "https://evil.example"},
		"other RP":       {flags: uv, rpID: "evil.example"},
		"no UV":          {flags: flagUP},
		"no UP":          {flags: flagUV},
		"wrong type":     {flags: uv, typ: "webauthn.create"},
		"bad signature":  {flags: uv, badSig: true},
		"rollback":       {flags: uv, count: new(uint32)},
		"foreign handle": {flags: uv, userHandle: enc64([]byte("someone-else"))},
	} {
		b := e.browser()
		if code, out := login(b, es, o); code != 401 || errCode(out) != "passkey_invalid" || b.cookie(SessionCookie) != "" {
			t.Fatalf("%s: %d %v", name, code, out)
		}
		e.h.logins.reset("127.0.0.1")
	}
	// A challenge works once.
	b := e.browser()
	o := b.must(200, "POST", "/api/v1/auth/passkey/options", map[string]any{"purpose": "login"})
	cred := es.assert(o["challenge"].(string), assertOpts{rpID: rpID, origin: origin, flags: uv})
	b.must(200, "POST", "/api/v1/auth/passkey", map[string]any{"credential": cred})
	es.count++
	cred2 := es.assert(o["challenge"].(string), assertOpts{rpID: rpID, origin: origin, flags: uv})
	if code, _ := e.browser().do("POST", "/api/v1/auth/passkey", map[string]any{"credential": cred2}); code != 401 {
		t.Fatalf("challenge reuse: %d", code)
	}

	// As a second factor after the password; UV not required.
	b, code, out := e.loginStep1("admin", "correct-horse-1")
	if code != 202 {
		t.Fatalf("step 1 with passkeys: %d %v", code, out)
	}
	if code, _ := b.do("POST", "/api/v1/auth/mfa", map[string]any{"method": "passkey", "credential": es.assert("made-up", assertOpts{rpID: rpID, origin: origin, flags: flagUP})}); code != 401 {
		t.Fatalf("unissued challenge: %d", code)
	}
	o = b.must(200, "POST", "/api/v1/auth/passkey/options", map[string]any{"purpose": "mfa"})
	if len(o["allowCredentials"].([]any)) != 3 || o["userVerification"] != "preferred" {
		t.Fatalf("mfa options %v", o)
	}
	// A challenge issued for another pending sign-in is refused.
	other, _, _ := e.loginStep1("admin", "correct-horse-1")
	if code, _ := other.do("POST", "/api/v1/auth/mfa", map[string]any{"method": "passkey", "credential": es.assert(o["challenge"].(string), assertOpts{rpID: rpID, origin: origin, flags: flagUP})}); code != 401 {
		t.Fatalf("challenge of another sign-in: %d", code)
	}
	o = b.must(200, "POST", "/api/v1/auth/passkey/options", map[string]any{"purpose": "mfa"})
	b.must(200, "POST", "/api/v1/auth/mfa", map[string]any{"method": "passkey", "credential": es.assert(o["challenge"].(string), assertOpts{rpID: rpID, origin: origin, flags: flagUP})})

	// Rename, then delete behind re-authentication by passkey.
	admin.must(204, "PATCH", "/api/v1/me/passkeys/"+esID, map[string]any{"name": "work laptop"})
	admin.dropReauth()
	if code, _ := admin.do("DELETE", "/api/v1/me/passkeys/"+esID, nil); code != 403 {
		t.Fatalf("delete without reauth: %d", code)
	}
	o = admin.must(200, "POST", "/api/v1/auth/passkey/options", map[string]any{"purpose": "reauth"})
	admin.must(200, "POST", "/api/v1/me/reauth", map[string]any{"method": "passkey", "credential": es.assert(o["challenge"].(string), assertOpts{rpID: rpID, origin: origin, flags: flagUP})})
	admin.must(204, "DELETE", "/api/v1/me/passkeys/"+esID, nil)
	if len(admin.must(200, "GET", "/api/v1/me/security", nil)["passkeys"].([]any)) != 2 {
		t.Fatal("passkey not deleted")
	}
	// A deleted passkey signs nobody in.
	if code, _ := login(e.browser(), es, assertOpts{flags: uv}); code != 401 {
		t.Fatalf("deleted passkey: %d", code)
	}

	// Reachable from other machines without a public URL: no RP to bind to.
	e.setSite(SettingPublicURL, nil)
	e.listenPublic()
	if code, out := admin.do("POST", "/api/v1/me/passkeys/options", nil); code != 409 || errCode(out) != "public_url_missing" {
		t.Fatalf("no public url: %d %v", code, out)
	}
	if code, _ := e.browser().do("POST", "/api/v1/auth/passkey/options", map[string]any{"purpose": "login"}); code != 409 {
		t.Fatalf("no public url login: %d", code)
	}
}

// ---------------------------------------------------------------- policy

func testMFAPolicy(t *testing.T, open opener) {
	e, clk, admin := mfaEnv(t, open)
	admin.must(200, "PATCH", "/api/v1/settings", map[string]any{"values": map[string]any{
		"security.require_2fa_admins": true, "security.require_2fa_grace_days": 2}})
	me := admin.must(200, "GET", "/api/v1/me", nil)["mfa"].(map[string]any)
	if me["required"] != true || me["enroll_by"] == nil {
		t.Fatalf("me mfa %v", me)
	}
	admin.must(200, "GET", "/api/v1/users", nil) // inside the grace period
	carol := admin.must(201, "POST", "/api/v1/users", map[string]any{"username": "carol", "role": "member", "password": "carols-password-1"})
	carolKey := admin.must(201, "POST", "/api/v1/keys", map[string]any{"name": "c", "user_id": carol["id"]})
	clk.add(49 * time.Hour)
	if code, out := admin.do("GET", "/api/v1/users", nil); code != 403 || errCode(out) != "mfa_enrollment_required" {
		t.Fatalf("past grace: %d %v", code, out)
	}
	admin.must(200, "GET", "/api/v1/keys", nil) // own pages stay open
	// Admin powers on pages every user has are gone too: no keys for
	// others, no access to their keys, no permissions reported.
	if code, out := admin.do("POST", "/api/v1/keys", map[string]any{"name": "x", "user_id": carol["id"]}); code != 422 {
		t.Fatalf("past grace, key for another user: %d %v", code, out)
	}
	if code, _ := admin.do("DELETE", "/api/v1/keys/"+carolKey["key"].(map[string]any)["id"].(string), nil); code != 404 && code != 403 {
		t.Fatalf("past grace, delete another user's key: %d", code)
	}
	if perms := admin.must(200, "GET", "/api/v1/me", nil)["permissions"].([]any); len(perms) != 0 {
		t.Fatalf("past grace permissions %v", perms)
	}
	enrollTOTP(t, admin, clk)
	admin.must(200, "GET", "/api/v1/users", nil)

	// Admin reset of another user's factors, audited.
	bob := admin.must(201, "POST", "/api/v1/users", map[string]any{"username": "bob", "role": "member", "password": "bobs-password-1"})
	m, _ := store.MFAOf(e.st)
	if err := m.EnableTOTP(context.Background(), bob["id"].(string), []byte("k"), 1, nil); err != nil {
		t.Fatal(err)
	}
	if u := admin.must(200, "GET", "/api/v1/users/"+bob["id"].(string), nil); u["mfa_enabled"] != true {
		t.Fatalf("user view %v", u)
	}
	if _, code, _ := e.loginStep1("bob", "bobs-password-1"); code != 202 {
		t.Fatalf("bob step 1: %d", code)
	}
	admin.must(204, "DELETE", "/api/v1/users/"+bob["id"].(string)+"/mfa", nil)
	if _, code, _ := e.loginStep1("bob", "bobs-password-1"); code != 200 {
		t.Fatalf("bob after reset: %d", code)
	}
	audit, _ := e.st.ListAudit(context.Background(), store.AuditQuery{Before: clk.now().Add(time.Hour), Target: "user:" + bob["id"].(string)})
	found := false
	for _, a := range audit {
		found = found || (a.Action == "user.mfa_reset" && a.Actor != "")
	}
	if !found {
		t.Fatal("reset not audited")
	}
	if code, _ := e.browser().do("DELETE", "/api/v1/users/"+bob["id"].(string)+"/mfa", nil); code != 401 {
		t.Fatalf("anonymous reset: %d", code)
	}
}

// ---------------------------------------------------------------- SSO

func testMFASSO(t *testing.T, open opener) {
	e, clk, admin := mfaEnv(t, open)
	idp := newFakeIdP(t)
	admin.must(200, "PATCH", "/api/v1/features/sso", map[string]any{"enabled": true})
	admin.must(201, "POST", "/api/v1/sso/providers", idp.provider(nil))
	b, _ := e.ssoTry(idp, "corp", person("sam-1", "sam@example.com", true))
	key, _ := enrollTOTP(t, b, clk)

	// Off by default: the IdP's own MFA is trusted.
	if _, final := e.ssoTry(idp, "corp", person("sam-1", "sam@example.com", true)); final != "/" {
		t.Fatalf("sso without second step: %q", final)
	}
	admin.must(200, "PATCH", "/api/v1/settings", map[string]any{"values": map[string]any{"security.require_2fa_sso": true}})
	b2, final := e.ssoTry(idp, "corp", person("sam-1", "sam@example.com", true))
	if final != "/login?mfa=1" || b2.cookie(SessionCookie) != "" {
		t.Fatalf("sso with second step: %q", final)
	}
	clk.add(30 * time.Second)
	b2.http.CheckRedirect = nil
	if got := b2.must(200, "GET", "/api/v1/auth/mfa", nil); got["return_to"] != "/" {
		t.Fatalf("pending %v", got)
	}
	b2.must(200, "POST", "/api/v1/auth/mfa", map[string]any{"method": "totp", "code": hotp(key, totpStep(clk.now()))})
	b2.must(200, "GET", "/api/v1/me", nil)
}

// ---------------------------------------------------------------- captcha

func testLoginCaptcha(t *testing.T, open opener) {
	e, _, admin := mfaEnv(t, open)
	var mu sync.Mutex
	status := 200
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		_ = r.ParseForm()
		if status != 200 {
			w.WriteHeader(status)
			return
		}
		ok := r.Form.Get("secret") == "captcha-secret" && r.Form.Get("response") == "good-token"
		writeJSON(w, 200, map[string]any{"success": ok})
	}))
	t.Cleanup(fake.Close)
	e.h.captchaEndpoint = fake.URL
	admin.must(200, "PATCH", "/api/v1/settings", map[string]any{"values": map[string]any{
		"security.captcha_provider": "turnstile", "security.captcha_site_key": "site-key",
		"security.captcha_secret": "captcha-secret", "security.login_captcha_after": 2}})
	raw, _, _ := e.st.KV(siteKV).Get(context.Background(), siteKey)
	if strings.Contains(string(raw), "captcha-secret") {
		t.Fatal("captcha secret stored in plaintext")
	}
	pc := e.browser().must(200, "GET", "/api/v1/auth/providers", nil)["captcha"].(map[string]any)
	if pc["provider"] != "turnstile" || pc["site_key"] != "site-key" || pc["after_failures"] != float64(2) {
		t.Fatalf("providers captcha %v", pc)
	}
	try := func(token string) (int, string) {
		code, out := e.browser().do("POST", "/api/v1/auth/login", map[string]any{"username": "admin", "password": "correct-horse-1", "captcha_token": token})
		return code, errCode(out)
	}
	e.h.logins.reset("127.0.0.1")
	// Under the threshold no captcha is asked.
	if _, code, _ := e.loginStep1("admin", "wrong-password-x"); code != 401 {
		t.Fatalf("first failure: %d", code)
	}
	if _, code, _ := e.loginStep1("admin", "correct-horse-1"); code != 200 {
		t.Fatalf("after one failure: %d", code)
	}
	e.h.logins.reset("127.0.0.1")
	e.loginStep1("admin", "wrong-password-x")
	e.loginStep1("admin", "wrong-password-x")
	if code, ec := try(""); code != 403 || ec != "captcha_required" {
		t.Fatalf("missing token: %d %s", code, ec)
	}
	if code, ec := try("bad-token"); code != 403 || ec != "captcha_failed" {
		t.Fatalf("bad token: %d %s", code, ec)
	}
	mu.Lock()
	status = 500
	mu.Unlock()
	if code, ec := try("good-token"); code != 503 || ec != "captcha_unavailable" {
		t.Fatalf("provider down: %d %s", code, ec)
	}
	mu.Lock()
	status = 200
	mu.Unlock()
	if code, ec := try("good-token"); code != 200 {
		t.Fatalf("good token: %d %s", code, ec)
	}

	// A captcha that is on but cannot run refuses instead of vanishing:
	// saving one without its secret is refused, and a secret that no
	// longer opens (another master key) blocks logins that need it.
	if code, out := admin.do("PATCH", "/api/v1/settings", map[string]any{"values": map[string]any{"security.captcha_secret": nil}}); code != 422 ||
		!strings.Contains(fmt.Sprint(out), "security.captcha_provider") {
		t.Fatalf("save without secret: %d %v", code, out)
	}
	if got := e.h.CaptchaProviders(context.Background()); !slices.Equal(got, []string{"turnstile"}) {
		t.Fatalf("CaptchaProviders %v", got)
	}
	s, _ := LoadSite(context.Background(), e.st)
	s[settingCaptchaSecret], _ = json.Marshal("v1.not-a-sealed-value")
	if err := saveSite(context.Background(), e.st, s); err != nil {
		t.Fatal(err)
	}
	e.h.logins.reset("127.0.0.1")
	e.loginStep1("admin", "wrong-password-x")
	e.loginStep1("admin", "wrong-password-x")
	if code, ec := try("good-token"); code != 503 || ec != "captcha_unavailable" {
		t.Fatalf("unreadable secret: %d %s", code, ec)
	}
}

// testReenableAdminAndEmail: re-enabling a disabled admin hands out a working
// admin account, so it needs a recent re-authentication like creating one
// (re-enabling a member does not); and the email, which is the login name,
// is refused when it differs instead of being dropped silently.
func testReenableAdminAndEmail(t *testing.T, open opener) {
	_, _, admin := mfaEnv(t, open)
	ann := admin.must(201, "POST", "/api/v1/users", map[string]any{"username": "ann@x.com", "role": "admin", "password": "anns-password-1"})["id"].(string)
	mem := admin.must(201, "POST", "/api/v1/users", map[string]any{"username": "max@x.com", "role": "member"})["id"].(string)
	admin.must(200, "PATCH", "/api/v1/users/"+ann, map[string]any{"status": "disabled"})
	admin.must(200, "PATCH", "/api/v1/users/"+mem, map[string]any{"status": "disabled"})
	admin.dropReauth()
	if code, out := admin.do("PATCH", "/api/v1/users/"+ann, map[string]any{"status": "active"}); code != 403 || errCode(out) != "reauth_required" {
		t.Fatalf("re-enabling an admin without reauth: %d %v", code, out)
	}
	admin.must(200, "PATCH", "/api/v1/users/"+mem, map[string]any{"status": "active"})

	if code, out := admin.do("POST", "/api/v1/users", map[string]any{"username": "eve@x.com", "email": "other@x.com", "role": "member"}); code != 422 {
		t.Fatalf("create with a different email: %d %v", code, out)
	}
	admin.must(201, "POST", "/api/v1/users", map[string]any{"username": "eve@x.com", "email": "EVE@x.com", "role": "member"})
	if code, out := admin.do("PATCH", "/api/v1/users/"+mem, map[string]any{"email": "max2@x.com"}); code != 422 {
		t.Fatalf("changing the email: %d %v", code, out)
	}
	admin.must(200, "PATCH", "/api/v1/users/"+mem, map[string]any{"email": "max@x.com", "display_name": "Max"})
}
