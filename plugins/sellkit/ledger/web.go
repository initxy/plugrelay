package ledger

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// Money is an amount as the console shows it.
type Money struct {
	Micro    int64  `json:"micro"`
	Currency string `json:"currency"`
}

// WriteJSON writes v with status.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if v != nil {
		_ = json.NewEncoder(w).Encode(v)
	}
}

// WriteError writes the management API's error shape.
func WriteError(w http.ResponseWriter, status int, code, msg string) {
	WriteJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": msg}})
}

// ReadJSON decodes a request body of at most 64 KiB, rejecting unknown fields.
func ReadJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(io.LimitReader(r.Body, 64<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		WriteError(w, http.StatusBadRequest, "invalid_request", "invalid JSON body: "+err.Error())
		return false
	}
	return true
}

// Who is the console user the core attached to a plugin API request.
func Who(r *http.Request) (userID, role string) {
	return r.Header.Get("X-Plugrelay-User"), r.Header.Get("X-Plugrelay-Role")
}

// ValidSubject reports whether s is "user:", "group:" or "key:" plus an id.
func ValidSubject(s string) bool {
	kind, id, ok := strings.Cut(s, ":")
	return ok && id != "" && (kind == "user" || kind == "group" || kind == "key")
}

// refusals are the 422 answers, by the code the console words.
var refusals = []struct {
	err  error
	code string
}{
	{ErrCodeInvalid, "redeem_invalid"}, {ErrCodeExpired, "redeem_expired"}, {ErrCodeUsedUp, "redeem_used_up"},
	{ErrCodeAlreadyUsed, "redeem_already_used"},
	{ErrCouponInvalid, "coupon_invalid"}, {ErrCouponNotStarted, "coupon_not_started"}, {ErrCouponExpired, "coupon_expired"},
	{ErrCouponUsedUp, "coupon_used_up"}, {ErrCouponAlreadyUsed, "coupon_already_used"}, {ErrCouponMinAmount, "coupon_min_amount"},
	{ErrSelfReferral, "self_referral"},
}

// Fail maps a ledger error to an API error and writes it.
func Fail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrInsufficient):
		WriteError(w, http.StatusConflict, "insufficient_balance", err.Error())
	case errors.Is(err, ErrSubscribed):
		WriteError(w, http.StatusConflict, "already_subscribed", err.Error())
	case errors.Is(err, ErrAllowanceExhausted):
		WriteError(w, http.StatusConflict, "allowance_exhausted", err.Error())
	case errors.Is(err, ErrKeyRequired):
		WriteError(w, http.StatusBadRequest, "key_required", err.Error())
	case errors.Is(err, ErrNotFound):
		WriteError(w, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, ErrConflict):
		WriteError(w, http.StatusConflict, "conflict", err.Error())
	default:
		for _, c := range refusals {
			if errors.Is(err, c.err) {
				WriteError(w, http.StatusUnprocessableEntity, c.code, err.Error())
				return
			}
		}
		WriteError(w, http.StatusInternalServerError, "internal", "ledger unavailable")
	}
}

// Format renders micro-units as a decimal amount with its currency
// ("1.25 USD"), keeping at least two decimals.
func Format(micro int64, currency string) string {
	sign := ""
	if micro < 0 {
		sign, micro = "-", -micro
	}
	frac := strings.TrimRight(strconv.FormatInt(1_000_000+micro%1_000_000, 10)[1:], "0")
	for len(frac) < 2 {
		frac += "0"
	}
	return sign + strconv.FormatInt(micro/1_000_000, 10) + "." + frac + " " + currency
}
