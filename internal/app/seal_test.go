package app

import (
	"context"
	"encoding/json"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/initxy/plugrelay/internal/config"
	"github.com/initxy/plugrelay/internal/secret"
	"github.com/initxy/plugrelay/pkg/ir"
)

// After a master key rotation, private state sealed under the old key
// still opens while the old key is listed in previous_master_keys; new
// seals use the new key.
func TestMasterKeyRotationOpensOldSeals(t *testing.T) {
	oldKey, _ := secret.GenerateMasterKey()
	newKey, _ := secret.GenerateMasterKey()
	dir := t.TempDir()
	relay := func(master string, previous ...string) *App {
		t.Helper()
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		cfg := config.Defaults()
		cfg.Store.DSN = filepath.Join(dir, "relay.db")
		cfg.MasterKey, cfg.PreviousMasterKeys = master, previous
		a, err := New(ctx, &cfg, slog.New(slog.DiscardHandler))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(a.closeAll)
		return a
	}
	st := ir.OpaqueState{Issuer: "anthropic", Kind: "signature", Data: json.RawMessage(`"sig-1"`)}
	sealed, err := relay(oldKey).sealer.Seal(st)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := relay(newKey).sealer.Open(sealed); err == nil {
		t.Fatal("a seal from a replaced key opened without previous_master_keys")
	}
	rotated := relay(newKey, oldKey)
	got, err := rotated.sealer.Open(sealed)
	if err != nil || got.Issuer != "anthropic" || string(got.Data) != `"sig-1"` {
		t.Fatalf("old seal after rotation: %+v %v", got, err)
	}
	fresh, err := rotated.sealer.Seal(st)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := relay(newKey).sealer.Open(fresh); err != nil {
		t.Fatalf("a new seal is not under the new key: %v", err)
	}
}
