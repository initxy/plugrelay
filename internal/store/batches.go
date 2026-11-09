package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/initxy/plugrelay/internal/sqldialect"
)

// Batch object kinds.
const (
	BatchKindFile         = "file"          // OpenAI file (batch input or output)
	BatchKindBatch        = "batch"         // OpenAI batch
	BatchKindMessageBatch = "message_batch" // Anthropic message batch
)

// BatchObject records where a stateful upstream object a client created
// lives (upstream, endpoint, credential) and who owns it. IDs are the
// upstream's.
type BatchObject struct {
	Kind          string `json:"kind"`
	ID            string `json:"id"`
	UpstreamID    string `json:"upstream_id"`
	EndpointID    string `json:"endpoint_id"`
	CredentialID  string `json:"credential_id"`
	Protocol      string `json:"protocol"` // protocol of the requests inside
	Model         string `json:"model,omitempty"`
	UpstreamModel string `json:"upstream_model,omitempty"`
	// ServedModel is the member of a requested preset (Model) the object
	// was placed on; "" when a model was requested.
	ServedModel string `json:"served_model,omitempty"`
	UserID      string `json:"user_id,omitempty"`
	KeyID       string `json:"key_id,omitempty"`
	// GroupID is the creator's group when the object was made: the payer
	// and price multiplier its usage is billed with.
	GroupID string `json:"group_id,omitempty"`
	// HoldID is the id a batch's money is held and charged under.
	HoldID string `json:"hold_id,omitempty"`
	// InputTokens and OutputTokens estimate an input file's requests: the
	// prompt and the output limit of every line.
	InputTokens  int64 `json:"input_tokens,omitempty"`
	OutputTokens int64 `json:"output_tokens,omitempty"`
	// Role of a file: "batch" (input), "batch_output", "batch_error".
	Role string `json:"role,omitempty"`
	// BatchID is the batch an output or error file belongs to.
	BatchID string `json:"batch_id,omitempty"`
	// Object is the upstream's last answer about the object, verbatim.
	Object    json.RawMessage `json:"object,omitempty"`
	CreatedAt time.Time       `json:"created_at,omitzero"`
	// BilledAt is set once the object's usage was charged.
	BilledAt time.Time `json:"billed_at,omitzero"`
}

// BatchQuery pages through one owner's objects of one kind, newest first.
type BatchQuery struct {
	Kind   string
	UserID string // owner: the user, or the key when UserID is empty
	KeyID  string
	// After is the id of the last object of the previous page; Before the
	// first object of the next (newer) page.
	After, Before string
	Limit         int
}

// Batches stores batch objects. The SQL store implements it; get it with
// BatchesOf.
type Batches interface {
	// PutBatchObject inserts or replaces an object.
	PutBatchObject(ctx context.Context, o *BatchObject) error
	GetBatchObject(ctx context.Context, kind, id string) (*BatchObject, error)
	// ListBatchObjects returns up to q.Limit objects plus whether more
	// follow.
	ListBatchObjects(ctx context.Context, q BatchQuery) ([]BatchObject, bool, error)
	// SetBatchObject replaces the stored upstream answer.
	SetBatchObject(ctx context.Context, kind, id string, object json.RawMessage) error
	// ClaimBatchBilling marks an object billed; only the first call for an
	// object reports true.
	ClaimBatchBilling(ctx context.Context, kind, id string, at time.Time) (bool, error)
	// UnbilledBatches returns up to limit batches and message batches not
	// billed yet and not checked since checkedBefore, least recently
	// checked first, and marks them checked at now.
	UnbilledBatches(ctx context.Context, checkedBefore, now time.Time, limit int) ([]BatchObject, error)
	DeleteBatchObject(ctx context.Context, kind, id string) error
}

// BatchesOf returns the store's batch objects.
func BatchesOf(s Store) (Batches, bool) {
	b, ok := s.(Batches)
	return b, ok
}

var _ Batches = (*db)(nil)

var errNoPage = errors.New("no page")

const batchCols = `kind, id, upstream_id, endpoint_id, credential_id, protocol, model, upstream_model, user_id, key_id, role, batch_id, object, created_at, billed_at,
  group_id, hold_id, input_tokens, output_tokens, served_model`

func scanBatchObject(sc interface{ Scan(...any) error }) (*BatchObject, error) {
	var o BatchObject
	var obj string
	var created, billed int64
	if err := sc.Scan(&o.Kind, &o.ID, &o.UpstreamID, &o.EndpointID, &o.CredentialID, &o.Protocol, &o.Model, &o.UpstreamModel,
		&o.UserID, &o.KeyID, &o.Role, &o.BatchID, &obj, &created, &billed, &o.GroupID, &o.HoldID, &o.InputTokens, &o.OutputTokens, &o.ServedModel); err != nil {
		return nil, err
	}
	if obj != "" {
		o.Object = json.RawMessage(obj)
	}
	o.CreatedAt, o.BilledAt = fromMs(created), fromMs(billed)
	return &o, nil
}

func (s *db) PutBatchObject(ctx context.Context, o *BatchObject) error {
	if o.CreatedAt.IsZero() {
		o.CreatedAt = time.Now().UTC().Truncate(time.Millisecond)
	}
	_, err := s.r().exec(ctx, `INSERT INTO batch_objects (`+batchCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (kind, id) DO UPDATE SET upstream_id = excluded.upstream_id, endpoint_id = excluded.endpoint_id,
  credential_id = excluded.credential_id, protocol = excluded.protocol, model = excluded.model,
  upstream_model = excluded.upstream_model, user_id = excluded.user_id, key_id = excluded.key_id,
  role = excluded.role, batch_id = excluded.batch_id, object = excluded.object, group_id = excluded.group_id,
  hold_id = excluded.hold_id, input_tokens = excluded.input_tokens, output_tokens = excluded.output_tokens,
  served_model = excluded.served_model`,
		o.Kind, o.ID, o.UpstreamID, o.EndpointID, o.CredentialID, o.Protocol, sqldialect.Truncate(o.Model, modelWidth), o.UpstreamModel,
		o.UserID, o.KeyID, o.Role, o.BatchID, string(o.Object), ms(o.CreatedAt), ms(o.BilledAt),
		o.GroupID, o.HoldID, o.InputTokens, o.OutputTokens, sqldialect.Truncate(o.ServedModel, modelWidth))
	return wrap("put batch object", err)
}

func (s *db) GetBatchObject(ctx context.Context, kind, id string) (*BatchObject, error) {
	o, err := scanBatchObject(s.r().row(ctx, `SELECT `+batchCols+` FROM batch_objects WHERE kind = ? AND id = ?`, kind, id))
	return o, wrap("get batch object", err)
}

func (s *db) ListBatchObjects(ctx context.Context, q BatchQuery) ([]BatchObject, bool, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = 20
	}
	where := `kind = ? AND user_id = ? AND (? <> '' OR key_id = ?)`
	args := []any{q.Kind, q.UserID, q.UserID, q.KeyID}
	order := `created_at DESC, id DESC`
	// A cursor the owner cannot see (unknown, deleted, someone else's)
	// pages nothing.
	cursor := func(id, op string) error {
		c, err := s.GetBatchObject(ctx, q.Kind, id)
		if errors.Is(err, ErrNotFound) || err == nil && (c.UserID != q.UserID || q.UserID == "" && c.KeyID != q.KeyID) {
			return errNoPage
		}
		if err != nil {
			return err
		}
		where += ` AND (created_at ` + op + ` ? OR (created_at = ? AND id ` + op + ` ?))`
		args = append(args, ms(c.CreatedAt), ms(c.CreatedAt), c.ID)
		return nil
	}
	var err error
	switch {
	case q.After != "":
		err = cursor(q.After, "<")
	case q.Before != "":
		err = cursor(q.Before, ">")
		order = `created_at ASC, id ASC`
	}
	if errors.Is(err, errNoPage) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	args = append(args, limit+1)
	out, err := list(ctx, s.r(), "list batch objects", `SELECT `+batchCols+` FROM batch_objects WHERE `+where+` ORDER BY `+order+` LIMIT ?`,
		scanBatchObject, args...)
	if err != nil {
		return nil, false, err
	}
	more := len(out) > limit
	if more {
		out = out[:limit]
	}
	if q.Before != "" {
		for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
			out[i], out[j] = out[j], out[i]
		}
	}
	return out, more, nil
}

func (s *db) SetBatchObject(ctx context.Context, kind, id string, object json.RawMessage) error {
	res, err := s.r().exec(ctx, `UPDATE batch_objects SET object = ? WHERE kind = ? AND id = ?`, string(object), kind, id)
	return mustAffect("set batch object", res, err)
}

func (s *db) ClaimBatchBilling(ctx context.Context, kind, id string, at time.Time) (bool, error) {
	res, err := s.r().exec(ctx, `UPDATE batch_objects SET billed_at = ? WHERE kind = ? AND id = ? AND billed_at = 0`, ms(at), kind, id)
	if err != nil {
		return false, wrap("claim batch billing", err)
	}
	n, err := res.RowsAffected()
	return n == 1, wrap("claim batch billing", err)
}

func (s *db) UnbilledBatches(ctx context.Context, checkedBefore, now time.Time, limit int) ([]BatchObject, error) {
	if limit <= 0 {
		limit = 20
	}
	out, err := list(ctx, s.r(), "unbilled batches", `SELECT `+batchCols+` FROM batch_objects
WHERE billed_at = 0 AND checked_at < ? AND kind IN (?, ?) ORDER BY checked_at, created_at, id LIMIT ?`,
		scanBatchObject, ms(checkedBefore), BatchKindBatch, BatchKindMessageBatch, limit)
	if err != nil {
		return nil, err
	}
	for _, o := range out {
		if _, err := s.r().exec(ctx, `UPDATE batch_objects SET checked_at = ? WHERE kind = ? AND id = ?`, ms(now), o.Kind, o.ID); err != nil {
			return nil, wrap("unbilled batches", err)
		}
	}
	return out, nil
}

func (s *db) DeleteBatchObject(ctx context.Context, kind, id string) error {
	res, err := s.r().exec(ctx, `DELETE FROM batch_objects WHERE kind = ? AND id = ?`, kind, id)
	return mustAffect("delete batch object", res, err)
}
