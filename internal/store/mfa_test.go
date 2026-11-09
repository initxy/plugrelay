package store_test

import (
	"context"
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/initxy/plugrelay/internal/store"
)

func TestMFASQLite(t *testing.T) { mfaConformance(t, sqliteOpener) }
func TestMFAMySQL(t *testing.T)  { mfaConformance(t, mysqlFromEnv(t)) }

func TestMFAPostgres(t *testing.T) {
	dsn := os.Getenv("PLUGRELAY_TEST_PG")
	if dsn == "" {
		t.Skip("PLUGRELAY_TEST_PG not set")
	}
	mfaConformance(t, pgOpener(dsn))
}

func mfaConformance(t *testing.T, open opener) {
	ctx := context.Background()
	s := open(t, newBox(t))
	m, ok := store.MFAOf(s)
	if !ok {
		t.Fatal("store has no MFA")
	}
	u := &store.User{Name: "Ann", Email: "ann@example.com", Role: store.RoleAdmin}
	must(t, s.CreateUser(ctx, u, "correct-horse-1"))
	if !u.HasPassword() {
		t.Fatal("HasPassword false")
	}
	got, _ := s.GetUser(ctx, u.ID)
	if !got.CheckPassword("correct-horse-1") || got.CheckPassword("wrong") {
		t.Fatal("CheckPassword")
	}

	if _, err := m.TOTPSecret(ctx, u.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("no totp yet: %v", err)
	}
	must(t, m.EnableTOTP(ctx, u.ID, []byte("12345678901234567890"), 100, []string{"h1", "h2"}))
	sec, err := m.TOTPSecret(ctx, u.ID)
	must(t, err)
	if string(sec) != "12345678901234567890" {
		t.Fatalf("secret %q", sec)
	}
	// The step used to enable, and older ones, are spent.
	for _, step := range []int64{99, 100} {
		if ok, err := m.UseTOTPStep(ctx, u.ID, step); err != nil || ok {
			t.Fatalf("step %d accepted: %v %v", step, ok, err)
		}
	}
	// Concurrent use of one step: exactly one wins.
	var wins atomic.Int32
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if ok, err := m.UseTOTPStep(ctx, u.ID, 101); err == nil && ok {
				wins.Add(1)
			}
		})
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatalf("step 101 accepted %d times", wins.Load())
	}

	if n, _ := m.CountRecoveryCodes(ctx, u.ID); n != 2 {
		t.Fatalf("codes %d", n)
	}
	if ok, _ := m.UseRecoveryCode(ctx, u.ID, "h1"); !ok {
		t.Fatal("code h1 refused")
	}
	if ok, _ := m.UseRecoveryCode(ctx, u.ID, "h1"); ok {
		t.Fatal("code h1 used twice")
	}
	must(t, m.ReplaceRecoveryCodes(ctx, u.ID, []string{"h3"}))
	if ok, _ := m.UseRecoveryCode(ctx, u.ID, "h2"); ok {
		t.Fatal("replaced code still works")
	}

	pk := &store.Passkey{UserID: u.ID, CredentialID: "cred-1", PublicKey: "pk", Alg: -7, SignCount: 5, Name: "laptop", Transports: []string{"internal"}}
	must(t, m.AddPasskey(ctx, pk))
	if err := m.AddPasskey(ctx, &store.Passkey{UserID: "other", CredentialID: "cred-1", PublicKey: "pk", Alg: -7}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("duplicate credential: %v", err)
	}
	p, err := m.PasskeyByCredential(ctx, "cred-1")
	must(t, err)
	if p.UserID != u.ID || p.SignCount != 5 || p.Transports[0] != "internal" {
		t.Fatalf("%+v", p)
	}
	at := time.Now().UTC().Truncate(time.Millisecond)
	if ok, _ := m.UsePasskey(ctx, p.ID, 5, at); ok {
		t.Fatal("equal sign count accepted")
	}
	if ok, _ := m.UsePasskey(ctx, p.ID, 6, at); !ok {
		t.Fatal("higher sign count refused")
	}
	if ok, _ := m.UsePasskey(ctx, p.ID, 0, at); ok {
		t.Fatal("counter drop to zero accepted")
	}
	zero := &store.Passkey{UserID: u.ID, CredentialID: "cred-0", PublicKey: "pk", Alg: -8}
	must(t, m.AddPasskey(ctx, zero))
	for range 2 {
		if ok, _ := m.UsePasskey(ctx, zero.ID, 0, at); !ok {
			t.Fatal("zero-counter authenticator refused")
		}
	}
	must(t, m.RenamePasskey(ctx, u.ID, p.ID, "phone"))
	if err := m.RenamePasskey(ctx, "other", p.ID, "x"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("rename someone else's passkey: %v", err)
	}
	list, _ := m.ListPasskeys(ctx, u.ID)
	if len(list) != 2 || list[0].Name != "phone" || !list[0].LastUsedAt.Equal(at) {
		t.Fatalf("list %+v", list)
	}
	users, _ := m.MFAUsers(ctx)
	if !users[u.ID] || len(users) != 1 {
		t.Fatalf("mfa users %v", users)
	}

	// Removing TOTP keeps recovery codes while passkeys remain; removing
	// the last factor drops them.
	must(t, m.DeleteTOTP(ctx, u.ID))
	if n, _ := m.CountRecoveryCodes(ctx, u.ID); n != 1 {
		t.Fatalf("codes after totp off: %d", n)
	}
	must(t, m.DeletePasskey(ctx, u.ID, p.ID))
	must(t, m.DeletePasskey(ctx, u.ID, zero.ID))
	if n, _ := m.CountRecoveryCodes(ctx, u.ID); n != 0 {
		t.Fatalf("codes after last factor: %d", n)
	}

	must(t, m.EnableTOTP(ctx, u.ID, []byte("x"), 1, []string{"a"}))
	must(t, m.AddPasskey(ctx, &store.Passkey{UserID: u.ID, CredentialID: "cred-2", PublicKey: "pk", Alg: -7}))
	must(t, m.ResetMFA(ctx, u.ID))
	if users, _ := m.MFAUsers(ctx); len(users) != 0 {
		t.Fatalf("after reset %v", users)
	}
	must(t, m.EnableTOTP(ctx, u.ID, []byte("x"), 1, []string{"a"}))
	must(t, s.DeleteUser(ctx, u.ID))
	if users, _ := m.MFAUsers(ctx); len(users) != 0 {
		t.Fatalf("after user delete %v", users)
	}
}
