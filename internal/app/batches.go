package app

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/initxy/plugrelay/internal/pipeline"
	"github.com/initxy/plugrelay/internal/store"
)

// batchStore adapts the store's batch objects to the pipeline.
type batchStore struct{ b store.Batches }

func batchStoreOf(s store.Store) pipeline.BatchStore {
	b, ok := store.BatchesOf(s)
	if !ok {
		return nil
	}
	return batchStore{b}
}

func toStoreBatch(o *pipeline.BatchObject) *store.BatchObject {
	return &store.BatchObject{Kind: o.Kind, ID: o.ID, UpstreamID: o.UpstreamID, EndpointID: o.EndpointID, CredentialID: o.CredentialID,
		Protocol: o.Protocol, Model: o.Model, UpstreamModel: o.UpstreamModel, UserID: o.UserID, KeyID: o.KeyID, Role: o.Role,
		BatchID: o.BatchID, Object: o.Object, CreatedAt: o.CreatedAt, BilledAt: o.BilledAt, GroupID: o.GroupID, HoldID: o.HoldID,
		InputTokens: o.InputTokens, OutputTokens: o.OutputTokens, ServedModel: o.ServedModel}
}

func fromStoreBatch(o *store.BatchObject) pipeline.BatchObject {
	return pipeline.BatchObject{Kind: o.Kind, ID: o.ID, UpstreamID: o.UpstreamID, EndpointID: o.EndpointID, CredentialID: o.CredentialID,
		Protocol: o.Protocol, Model: o.Model, UpstreamModel: o.UpstreamModel, UserID: o.UserID, KeyID: o.KeyID, Role: o.Role,
		BatchID: o.BatchID, Object: o.Object, CreatedAt: o.CreatedAt, BilledAt: o.BilledAt, GroupID: o.GroupID, HoldID: o.HoldID,
		InputTokens: o.InputTokens, OutputTokens: o.OutputTokens, ServedModel: o.ServedModel}
}

func (s batchStore) PutBatch(ctx context.Context, o *pipeline.BatchObject) error {
	return s.b.PutBatchObject(ctx, toStoreBatch(o))
}

func (s batchStore) GetBatch(ctx context.Context, kind, id string) (*pipeline.BatchObject, error) {
	o, err := s.b.GetBatchObject(ctx, kind, id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	out := fromStoreBatch(o)
	return &out, nil
}

func (s batchStore) ListBatches(ctx context.Context, q pipeline.BatchQuery) ([]pipeline.BatchObject, bool, error) {
	objs, more, err := s.b.ListBatchObjects(ctx, store.BatchQuery{Kind: q.Kind, UserID: q.UserID, KeyID: q.KeyID,
		After: q.After, Before: q.Before, Limit: q.Limit})
	if errors.Is(err, store.ErrNotFound) {
		return nil, false, nil // unknown cursor: an empty page
	}
	out := make([]pipeline.BatchObject, len(objs))
	for i := range objs {
		out[i] = fromStoreBatch(&objs[i])
	}
	return out, more, err
}

func (s batchStore) SetBatchObject(ctx context.Context, kind, id string, object json.RawMessage) error {
	return s.b.SetBatchObject(ctx, kind, id, object)
}

func (s batchStore) ClaimBatchBilling(ctx context.Context, kind, id string, at time.Time) (bool, error) {
	return s.b.ClaimBatchBilling(ctx, kind, id, at)
}

func (s batchStore) UnbilledBatches(ctx context.Context, checkedBefore, now time.Time, limit int) ([]pipeline.BatchObject, error) {
	objs, err := s.b.UnbilledBatches(ctx, checkedBefore, now, limit)
	out := make([]pipeline.BatchObject, len(objs))
	for i := range objs {
		out[i] = fromStoreBatch(&objs[i])
	}
	return out, err
}

func (s batchStore) DeleteBatch(ctx context.Context, kind, id string) error {
	err := s.b.DeleteBatchObject(ctx, kind, id)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	return err
}
