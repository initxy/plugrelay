package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/initxy/plugrelay/internal/store"
)

func TestBatchObjectsSQLite(t *testing.T) { batchObjects(t, sqliteOpener) }
func TestBatchObjectsMySQL(t *testing.T)  { batchObjects(t, mysqlFromEnv(t)) }

func TestBatchObjectsPostgres(t *testing.T) {
	dsn := os.Getenv("PLUGRELAY_TEST_PG")
	if dsn == "" {
		t.Skip("PLUGRELAY_TEST_PG not set")
	}
	batchObjects(t, pgOpener(dsn))
}

func batchObjects(t *testing.T, open opener) {
	ctx := context.Background()
	s := open(t, newBox(t))
	b, ok := store.BatchesOf(s)
	if !ok {
		t.Fatal("store has no batch objects")
	}
	base := time.UnixMilli(1_700_000_000_000).UTC()
	for i, id := range []string{"batch_a", "batch_b", "batch_c", "batch_d"} {
		o := &store.BatchObject{Kind: store.BatchKindBatch, ID: id, UpstreamID: "openai", EndpointID: "chat", CredentialID: "c1",
			Protocol: "openai-chat", Model: "gpt", ServedModel: "gpt-mini", UserID: "u1", KeyID: "k1", CreatedAt: base.Add(time.Duration(i) * time.Second),
			Object: json.RawMessage(`{"id":"` + id + `"}`)}
		must(t, b.PutBatchObject(ctx, o))
	}
	// Another owner's object and a key-owned one never show up in u1's list.
	must(t, b.PutBatchObject(ctx, &store.BatchObject{Kind: store.BatchKindBatch, ID: "batch_x", UpstreamID: "openai", EndpointID: "chat",
		CredentialID: "c1", Protocol: "openai-chat", UserID: "u2", KeyID: "k2", CreatedAt: base}))
	must(t, b.PutBatchObject(ctx, &store.BatchObject{Kind: store.BatchKindBatch, ID: "batch_k", UpstreamID: "openai", EndpointID: "chat",
		CredentialID: "c1", Protocol: "openai-chat", KeyID: "k9", CreatedAt: base}))

	got, err := b.GetBatchObject(ctx, store.BatchKindBatch, "batch_b")
	must(t, err)
	if got.CredentialID != "c1" || got.UserID != "u1" || got.ServedModel != "gpt-mini" || string(got.Object) != `{"id":"batch_b"}` || !got.CreatedAt.Equal(base.Add(time.Second)) {
		t.Fatalf("got %+v", got)
	}
	if _, err := b.GetBatchObject(ctx, store.BatchKindFile, "batch_b"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("other kind: %v", err)
	}

	ids := func(os []store.BatchObject) (out []string) {
		for _, o := range os {
			out = append(out, o.ID)
		}
		return out
	}
	page, more, err := b.ListBatchObjects(ctx, store.BatchQuery{Kind: store.BatchKindBatch, UserID: "u1", KeyID: "k1", Limit: 2})
	must(t, err)
	if got := ids(page); !more || len(got) != 2 || got[0] != "batch_d" || got[1] != "batch_c" {
		t.Fatalf("page 1 %v more %v", got, more)
	}
	page, more, err = b.ListBatchObjects(ctx, store.BatchQuery{Kind: store.BatchKindBatch, UserID: "u1", After: "batch_c", Limit: 2})
	must(t, err)
	if got := ids(page); more || len(got) != 2 || got[0] != "batch_b" || got[1] != "batch_a" {
		t.Fatalf("page 2 %v more %v", got, more)
	}
	page, _, err = b.ListBatchObjects(ctx, store.BatchQuery{Kind: store.BatchKindBatch, UserID: "u1", Before: "batch_a", Limit: 2})
	must(t, err)
	if got := ids(page); len(got) != 2 || got[0] != "batch_c" || got[1] != "batch_b" {
		t.Fatalf("before %v", got)
	}
	// A cursor that is unknown or someone else's pages nothing.
	for _, cur := range []string{"batch_nope", "batch_x"} {
		page, _, err = b.ListBatchObjects(ctx, store.BatchQuery{Kind: store.BatchKindBatch, UserID: "u1", After: cur})
		if err != nil || len(page) != 0 {
			t.Fatalf("cursor %s: %v %v", cur, ids(page), err)
		}
	}
	page, _, err = b.ListBatchObjects(ctx, store.BatchQuery{Kind: store.BatchKindBatch, KeyID: "k9"})
	must(t, err)
	if got := ids(page); len(got) != 1 || got[0] != "batch_k" {
		t.Fatalf("key-owned %v", got)
	}

	// Unbilled batches come least recently checked first, once per check.
	due, err := b.UnbilledBatches(ctx, base.Add(time.Hour), base.Add(time.Hour), 3)
	must(t, err)
	if got := ids(due); len(got) != 3 {
		t.Fatalf("due %v", got)
	}
	rest, err := b.UnbilledBatches(ctx, base.Add(time.Hour), base.Add(time.Hour), 10)
	must(t, err)
	if len(rest) != 3 || rest[0].ID == due[0].ID {
		t.Fatalf("rest %v after %v", ids(rest), ids(due))
	}
	// Billing fields round-trip.
	must(t, b.PutBatchObject(ctx, &store.BatchObject{Kind: store.BatchKindFile, ID: "file-1", UpstreamID: "openai", EndpointID: "chat",
		CredentialID: "c1", Protocol: "openai-chat", UserID: "u1", GroupID: "g1", HoldID: "batch:r1", InputTokens: 7, OutputTokens: 9}))
	if f, _ := b.GetBatchObject(ctx, store.BatchKindFile, "file-1"); f.GroupID != "g1" || f.HoldID != "batch:r1" || f.InputTokens != 7 || f.OutputTokens != 9 {
		t.Fatalf("file %+v", f)
	}

	must(t, b.SetBatchObject(ctx, store.BatchKindBatch, "batch_a", json.RawMessage(`{"status":"completed"}`)))
	if got, _ := b.GetBatchObject(ctx, store.BatchKindBatch, "batch_a"); string(got.Object) != `{"status":"completed"}` {
		t.Errorf("object %s", got.Object)
	}
	first, err := b.ClaimBatchBilling(ctx, store.BatchKindBatch, "batch_a", base)
	must(t, err)
	second, err := b.ClaimBatchBilling(ctx, store.BatchKindBatch, "batch_a", base)
	must(t, err)
	if !first || second {
		t.Errorf("claims %v %v", first, second)
	}
	// Re-putting the object (a later status refresh) keeps the billing mark.
	got, _ = b.GetBatchObject(ctx, store.BatchKindBatch, "batch_a")
	must(t, b.PutBatchObject(ctx, got))
	if again, _ := b.ClaimBatchBilling(ctx, store.BatchKindBatch, "batch_a", base); again {
		t.Error("billing claimed twice after a re-put")
	}
	must(t, b.DeleteBatchObject(ctx, store.BatchKindBatch, "batch_a"))
	if err := b.DeleteBatchObject(ctx, store.BatchKindBatch, "batch_a"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("second delete: %v", err)
	}

	// A batch discount must be in [0, 1).
	for _, d := range []float64{1, -0.1, 1.5} {
		if err := s.PutPrice(ctx, &store.PriceRule{Model: "gpt", Input: 1, BatchDiscount: d}); err == nil {
			t.Errorf("discount %v accepted", d)
		}
	}
	if _, err := s.Apply(ctx, []byte("prices:\n  - {model: gpt, input: 1, batch_discount: 1}\n"), false); err == nil ||
		!strings.Contains(err.Error(), "below 1") {
		t.Errorf("apply with discount 1: %v", err)
	}

	// Batch prices round-trip.
	must(t, s.PutPrice(ctx, &store.PriceRule{Model: "gpt", Input: 1, Output: 2, BatchDiscount: 0.5}))
	ps, err := s.ListPrices(ctx)
	must(t, err)
	if len(ps) != 1 || ps[0].BatchDiscount != 0.5 {
		t.Errorf("prices %+v", ps)
	}
}
