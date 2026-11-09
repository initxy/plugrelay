// Package redeem is the admin.redeem plugin (sell kit): admins generate
// batches of redeem codes (amount, expiry, uses), users redeem them into
// their balance in the console.
//
// Codes are shown once when generated; the core stores only their SHA-256.
// A redeem is one database transaction (check, count the use, credit), so
// a code is never used past its max_uses and an account redeems a code at
// most once. Failed attempts are limited per user (failures_per_hour).
package redeem

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/initxy/plugrelay/pkg/plugin"
	"github.com/initxy/plugrelay/plugins/sellkit/ledger"
)

// ID is the plugin id.
const ID = "admin.redeem"

func init() { plugin.Register(ID, func() plugin.Plugin { return New(nil, nil) }) }

// Plugin implements plugin.Admin.
type Plugin struct {
	l        ledger.Ledger
	limits   plugin.KV
	settings plugin.Settings
	now      func() time.Time
}

var _ plugin.Admin = (*Plugin)(nil)

// New returns the plugin; nil fields are taken from the Host in Init.
func New(l ledger.Ledger, limits plugin.KV) *Plugin {
	return &Plugin{l: l, limits: limits, now: time.Now}
}

// Info implements plugin.Plugin.
func (p *Plugin) Info() plugin.Info {
	return plugin.Info{ID: ID, Kind: plugin.KindAdmin, Version: "1.0.0",
		Description: "Redeem codes: admins generate batches, users redeem them into their balance.",
		Requires:    []string{"meter.balance"},
		Settings: []plugin.Setting{
			{Key: "failures_per_hour", Type: plugin.TypeInt, Default: 10, Scopes: plugin.Global, Merge: plugin.Override,
				Label: plugin.T("Wrong codes allowed per user per hour", "每个用户每小时可输错几次兑换码"),
				Help:  plugin.T("Further attempts wait for the next hour.", "超过后要等到下一个小时才能再试。")},
		}}
}

// Init implements plugin.Initializer.
func (p *Plugin) Init(_ context.Context, h plugin.Host) error {
	if p.l == nil {
		l, err := ledger.From(h)
		if err != nil {
			return err
		}
		p.l = l
	}
	if p.limits == nil {
		p.limits = h.KV()
	}
	p.settings = h.Settings()
	return nil
}

// Routes implements plugin.Admin.
func (p *Plugin) Routes() []plugin.AdminRoute {
	return []plugin.AdminRoute{
		{Method: http.MethodPost, Path: "/batches", Handler: p.postBatch, Role: "admin"},
		{Method: http.MethodGet, Path: "/batches", Handler: p.getBatches, Role: "admin"},
		{Method: http.MethodPost, Path: "/batches/disable", Handler: p.postBatchDisable, Role: "admin"},
		{Method: http.MethodGet, Path: "/codes", Handler: p.getCodes, Role: "admin"},
		{Method: http.MethodPost, Path: "/codes/disable", Handler: p.postDisable, Role: "admin"},
		{Method: http.MethodPost, Path: "/redeem", Handler: p.postRedeem},
	}
}

// Normalize is how a typed code is compared: upper case, no separators.
func Normalize(code string) string {
	r := strings.NewReplacer("-", "", " ", "", "\t", "")
	return strings.ToUpper(r.Replace(strings.TrimSpace(code)))
}

// Hash is the stored form of a code.
func Hash(code string) string {
	sum := sha256.Sum256([]byte("redeem:" + Normalize(code)))
	return hex.EncodeToString(sum[:])
}

// alphabet is Crockford base32 without I, L, O, U: easy to read aloud.
const alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// Generate returns a new code, 20 random symbols (100 bits) in groups of
// four after "RC".
func Generate() (string, error) {
	buf := make([]byte, 20)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString("RC")
	for i, c := range buf {
		if i%4 == 0 {
			b.WriteByte('-')
		}
		b.WriteByte(alphabet[c&31])
	}
	return b.String(), nil
}

type batchInput struct {
	Count       int    `json:"count"`
	AmountMicro int64  `json:"amount_micro"`
	MaxUses     int    `json:"max_uses"`
	ExpiresAt   string `json:"expires_at"`
	Batch       string `json:"batch"`
}

func (p *Plugin) postBatch(w http.ResponseWriter, r *http.Request) {
	var in batchInput
	if !ledger.ReadJSON(w, r, &in) {
		return
	}
	if in.Count < 1 || in.Count > 1000 || in.AmountMicro <= 0 || in.MaxUses < 0 || in.MaxUses > 1_000_000 {
		ledger.WriteError(w, http.StatusBadRequest, "invalid_request", "count 1–1000, a positive amount_micro and max_uses ≥ 0 are required")
		return
	}
	var exp time.Time
	if in.ExpiresAt != "" {
		t, err := time.Parse(time.RFC3339, in.ExpiresAt)
		if err != nil || !t.After(p.now()) {
			ledger.WriteError(w, http.StatusBadRequest, "invalid_request", "expires_at must be a future RFC 3339 time")
			return
		}
		exp = t
	}
	if in.Batch == "" {
		in.Batch = p.now().UTC().Format("20060102-150405")
	}
	actor, _ := ledger.Who(r)
	plain := make([]string, in.Count)
	codes := make([]ledger.Code, in.Count)
	for i := range plain {
		c, err := Generate()
		if err != nil {
			ledger.Fail(w, err)
			return
		}
		plain[i] = c
		codes[i] = ledger.Code{Hash: Hash(c), Last4: c[len(c)-4:], Batch: in.Batch, AmountMicro: in.AmountMicro,
			MaxUses: max(in.MaxUses, 1), ExpiresAt: exp, CreatedBy: actor}
	}
	if err := p.l.CreateCodes(r.Context(), codes); err != nil {
		ledger.Fail(w, err)
		return
	}
	ledger.WriteJSON(w, http.StatusCreated, map[string]any{"batch": in.Batch, "codes": plain,
		"amount": ledger.Money{Micro: in.AmountMicro, Currency: p.l.Currency(r.Context())}})
}

// codeView is a code with the accounts that redeemed it, when asked for.
type codeView struct {
	ledger.Code
	Amount   ledger.Money       `json:"amount"`
	Redeemed []ledger.RedeemUse `json:"redeemed,omitempty"`
}

// getCodes lists codes, of one batch with ?batch=; ?uses=1 adds who
// redeemed each (needs a batch).
func (p *Plugin) getCodes(w http.ResponseWriter, r *http.Request) {
	ctx, q := r.Context(), r.URL.Query()
	codes, err := p.l.Codes(ctx, q.Get("batch"))
	if err != nil {
		ledger.Fail(w, err)
		return
	}
	withUses := q.Get("uses") == "1" && q.Get("batch") != ""
	cur := p.l.Currency(ctx)
	items := make([]codeView, 0, len(codes))
	for _, c := range codes {
		v := codeView{Code: c, Amount: ledger.Money{Micro: c.AmountMicro, Currency: cur}}
		if withUses && c.Uses > 0 {
			if v.Redeemed, err = p.l.RedeemUses(ctx, c.ID); err != nil {
				ledger.Fail(w, err)
				return
			}
		}
		items = append(items, v)
	}
	ledger.WriteJSON(w, http.StatusOK, map[string]any{"items": items, "currency": cur})
}

// batchView sums one batch of codes.
type batchView struct {
	Batch     string       `json:"batch"`
	Codes     int          `json:"codes"`
	Amount    ledger.Money `json:"amount"`
	MaxUses   int          `json:"max_uses"`
	Uses      int          `json:"uses"`
	Disabled  int          `json:"disabled"`
	Expired   int          `json:"expired"`
	ExpiresAt time.Time    `json:"expires_at,omitzero"`
	CreatedAt time.Time    `json:"created_at"`
	CreatedBy string       `json:"created_by,omitempty"`
	// Redeemed is the money redeemed from the batch so far.
	Redeemed ledger.Money `json:"redeemed"`
}

func (p *Plugin) getBatches(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	codes, err := p.l.Codes(ctx, "")
	if err != nil {
		ledger.Fail(w, err)
		return
	}
	cur, now := p.l.Currency(ctx), p.now()
	byName := map[string]*batchView{}
	var order []string
	for _, c := range codes {
		b := byName[c.Batch]
		if b == nil {
			b = &batchView{Batch: c.Batch, Amount: ledger.Money{Micro: c.AmountMicro, Currency: cur}, CreatedAt: c.CreatedAt,
				CreatedBy: c.CreatedBy, ExpiresAt: c.ExpiresAt, Redeemed: ledger.Money{Currency: cur}}
			byName[c.Batch] = b
			order = append(order, c.Batch)
		}
		b.Codes++
		b.MaxUses += c.MaxUses
		b.Uses += c.Uses
		b.Redeemed.Micro += int64(c.Uses) * c.AmountMicro
		if c.Disabled {
			b.Disabled++
		}
		if !c.ExpiresAt.IsZero() && !now.Before(c.ExpiresAt) {
			b.Expired++
		}
		if c.CreatedAt.Before(b.CreatedAt) {
			b.CreatedAt = c.CreatedAt
		}
	}
	items := make([]batchView, 0, len(order))
	for _, n := range order {
		items = append(items, *byName[n])
	}
	ledger.WriteJSON(w, http.StatusOK, map[string]any{"items": items, "currency": cur})
}

func (p *Plugin) postBatchDisable(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Batch    string `json:"batch"`
		Disabled bool   `json:"disabled"`
	}
	if !ledger.ReadJSON(w, r, &in) {
		return
	}
	if err := p.l.SetBatchDisabled(r.Context(), in.Batch, in.Disabled); err != nil {
		ledger.Fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (p *Plugin) postDisable(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID       string `json:"id"`
		Disabled bool   `json:"disabled"`
	}
	if !ledger.ReadJSON(w, r, &in) {
		return
	}
	if err := p.l.SetCodeDisabled(r.Context(), in.ID, in.Disabled); err != nil {
		ledger.Fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (p *Plugin) postRedeem(w http.ResponseWriter, r *http.Request) {
	uid, _ := ledger.Who(r)
	if uid == "" {
		ledger.WriteError(w, http.StatusForbidden, "forbidden", "a console user is required")
		return
	}
	var in struct {
		Code string `json:"code"`
		// KeyID picks the account when keys or groups pay (meter.balance payer).
		KeyID string `json:"key_id"`
	}
	if !ledger.ReadJSON(w, r, &in) {
		return
	}
	ctx := r.Context()
	subject, err := p.l.PayerOf(ctx, uid, in.KeyID)
	if err != nil {
		ledger.Fail(w, err)
		return
	}
	limit := int64(10)
	if p.settings != nil {
		if v := p.settings.Get("failures_per_hour"); v.IsSet() {
			limit = v.Int()
		}
	}
	// Every attempt takes a slot first, so concurrent guesses cannot all
	// pass the check; a successful redeem gives its slot back.
	window := "fail:" + uid + ":" + p.now().UTC().Format("2006010215")
	if limit > 0 {
		n, err := p.counter(ctx, window, 1)
		if err != nil {
			ledger.Fail(w, err)
			return
		}
		if n > limit {
			w.Header().Set("Retry-After", "3600")
			ledger.WriteError(w, http.StatusTooManyRequests, "rate_limited", "too many failed attempts; try again later")
			return
		}
	}
	t, err := p.l.Redeem(ctx, Hash(in.Code), subject, uid, p.now())
	if limit > 0 && !isCodeFailure(err) {
		_, _ = p.counter(context.WithoutCancel(ctx), window, -1)
	}
	if err != nil {
		ledger.Fail(w, err)
		return
	}
	cur := p.l.Currency(ctx)
	ledger.WriteJSON(w, http.StatusOK, map[string]any{"amount": ledger.Money{Micro: t.AmountMicro, Currency: cur},
		"balance": ledger.Money{Micro: t.BalanceAfterMicro, Currency: cur}})
}

// isCodeFailure is a wrong, expired or used-up code: what the limit counts.
func isCodeFailure(err error) bool {
	return errors.Is(err, ledger.ErrCodeInvalid) || errors.Is(err, ledger.ErrCodeExpired) ||
		errors.Is(err, ledger.ErrCodeUsedUp) || errors.Is(err, ledger.ErrCodeAlreadyUsed)
}

func (p *Plugin) counter(ctx context.Context, key string, delta int64) (int64, error) {
	return p.limits.Incr(ctx, key, delta, time.Hour+time.Minute)
}
