package cli

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/initxy/plugrelay/internal/admin"
	"github.com/initxy/plugrelay/internal/config"
	"github.com/initxy/plugrelay/internal/store"
)

func TestUserResetPassword(t *testing.T) {
	te := newTestEnv(t)
	te.vars["PLUGRELAY_ADMIN_PASSWORD"] = "first password"
	cfgPath := filepath.Join(t.TempDir(), "relay.yaml")
	if code := te.run("--config", cfgPath, "--data-dir", t.TempDir(), "init", "--admin-email", "ops@example.com"); code != ExitOK {
		t.Fatalf("init exit %d: %s", code, te.stderr)
	}
	cfg, err := config.Load(cfgPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	st, err := store.Open(ctx, store.Config{Driver: "sqlite", DSN: cfg.Store.DSN})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	// Password sign-in turned off with a provider enabled.
	off, _ := json.Marshal(map[string]any{"providers": []map[string]any{{"id": "corp", "enabled": true}}})
	if err := st.KV("core.sso").Put(ctx, "config", off, 0); err != nil {
		t.Fatal(err)
	}
	if err := st.PutFeature(ctx, store.FeatureState{ID: "sso", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := admin.PutSiteSetting(ctx, st, "security.password_login", false); err != nil {
		t.Fatal(err)
	}

	if code := te.run("--config", cfgPath, "user", "reset-password"); code != ExitUsage {
		t.Fatalf("missing --user: exit %d", code)
	}
	if code := te.run("--config", cfgPath, "user", "reset-password", "--user", "nobody@example.com"); code != ExitError {
		t.Fatalf("unknown user: exit %d", code)
	}
	te.vars["PLUGRELAY_ADMIN_PASSWORD"] = "second password"
	if code := te.run("--config", cfgPath, "user", "reset-password", "--user", "OPS@example.com"); code != ExitOK {
		t.Fatalf("exit %d: %s", code, te.stderr)
	}
	if !strings.Contains(te.stdout.String(), "on again") {
		t.Errorf("stdout: %s", te.stdout)
	}
	if _, err := st.Authenticate(ctx, "ops@example.com", "second password"); err != nil {
		t.Fatalf("new password: %v", err)
	}
	if site, _ := admin.LoadSite(ctx, st); string(site["security.password_login"]) != "true" {
		t.Fatalf("password sign-in still off: %s", site["security.password_login"])
	}
	audit, err := st.ListAudit(ctx, store.AuditQuery{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, a := range audit {
		found = found || (a.Action == "user.update" && a.Actor == "cli" && strings.Contains(string(a.Diff), "password_login"))
	}
	if !found {
		t.Fatalf("no audit entry for the reset: %+v", audit)
	}
}

func TestUserResetMFA(t *testing.T) {
	te := newTestEnv(t)
	te.vars["PLUGRELAY_ADMIN_PASSWORD"] = "first password"
	cfgPath := filepath.Join(t.TempDir(), "relay.yaml")
	if code := te.run("--config", cfgPath, "--data-dir", t.TempDir(), "init", "--admin-email", "ops@example.com"); code != ExitOK {
		t.Fatalf("init exit %d: %s", code, te.stderr)
	}
	cfg, err := config.Load(cfgPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	st, err := store.Open(ctx, store.Config{Driver: "sqlite", DSN: cfg.Store.DSN})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	u, err := st.UserByEmail(ctx, "ops@example.com")
	if err != nil {
		t.Fatal(err)
	}
	m, _ := store.MFAOf(st)
	if err := m.AddPasskey(ctx, &store.Passkey{UserID: u.ID, CredentialID: "c1", PublicKey: "k", Alg: -7}); err != nil {
		t.Fatal(err)
	}
	if code := te.run("--config", cfgPath, "user", "reset-2fa"); code != ExitUsage {
		t.Fatalf("missing --user: exit %d", code)
	}
	if code := te.run("--config", cfgPath, "user", "reset-2fa", "--user", "nobody@example.com"); code != ExitError {
		t.Fatalf("unknown user: exit %d", code)
	}
	if code := te.run("--config", cfgPath, "user", "reset-2fa", "--user", "ops@example.com"); code != ExitOK {
		t.Fatalf("exit %d: %s", code, te.stderr)
	}
	if users, _ := m.MFAUsers(ctx); len(users) != 0 {
		t.Fatalf("factors left: %v", users)
	}
	audit, _ := st.ListAudit(ctx, store.AuditQuery{Limit: 10})
	if len(audit) == 0 || audit[0].Action != "user.mfa_reset" || audit[0].Actor != "cli" {
		t.Fatalf("audit: %+v", audit)
	}
}
