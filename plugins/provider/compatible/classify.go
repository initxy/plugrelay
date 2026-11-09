package compatible

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/plugin"
	"github.com/initxy/plugrelay/plugins/provider/manifest"
	"github.com/tidwall/gjson"
)

// Cooldown defaults used when the upstream gives no hint.
const (
	CooldownAuth       = 10 * time.Minute
	CooldownRateLimit  = 30 * time.Second
	CooldownOverloaded = 10 * time.Second
	CooldownQuota      = time.Hour
)

// Classify implements plugin.Provider. The manifest's error rules are tried
// in order first; the first match decides. Otherwise the built-in rules
// below apply, top to bottom, first match wins:
//
//	#   condition                                         cause         retry  credential
//	1   no response, request never sent                  transient     next   keep (Sent=false)
//	2   no response, caller canceled                      transient     none   keep
//	3   no response after send (timeout, reset)           transient     next   keep
//	4   signal content_filtered, or content-policy code   content       none   keep
//	5   400/401/403 saying the account/org is disabled,   banned        none   quarantine
//	    suspended, deactivated, banned or terminated
//	6   400/403 citing usage policy, abuse, flagging      unknown       none   quarantine
//	7   402, signal quota_exhausted, code insufficient_*  quota         next   cooldown until reset (1h)
//	8   401                                               auth_expired  next   refresh if the provider
//	                                                                           refreshes, else cooldown 10m
//	9   403 citing country/region/location                transient     next   keep
//	10  403 otherwise                                     unknown       none   quarantine
//	11  529, or 503 that signals overload                 rate_limit    next   cooldown retry-after or 10s, model only
//	12  429                                               rate_limit    next   cooldown retry-after, reset or 30s, model only
//	13  408, 409, 5xx                                     transient     next   keep
//	14  400/413/422 that signals context too long         request       none   keep (signal context_too_long)
//	15  404, 405 (another endpoint may serve the path)    request       next   keep
//	16  any other status                                  request       none   keep
//
// Rules 4–6 and 10 encode hard rule 16: a suspected ban is quarantined and never
// retried, content rejections are never retried on any credential, and
// when ban and content cannot be told apart the credential is quarantined
// and the request is not retried. Rules 1–3, 9 and 13–16 encode hard rule
// 17: failures that are not the credential's fault leave it untouched.
//
// ClientErr always carries the upstream message, code, body, retry-after
// and the pass-through headers (retry-after, x-should-retry, rate-limit
// headers, request ids). Its status is the upstream's, except for
// credential failures (auth, ban, quota), whose upstream status would
// mislead the client about its own key.
func (p *Provider) Classify(o *plugin.Outcome) plugin.Verdict {
	if o == nil {
		return plugin.Verdict{Cause: plugin.CauseUnknown, Retry: plugin.RetryNone, Credential: plugin.CredKeep}
	}
	now := p.now()
	f := readFacts(o, now)
	var (
		v      plugin.Verdict
		signal ir.ErrorKind
		ok     bool
	)
	if m := p.manifestOf(o); m != nil {
		v, signal, ok = p.ruleVerdict(m, o, f, now)
	}
	if !ok {
		v, signal = p.defaultVerdict(o, f, now)
	}
	v.Sent = o.Sent || o.Status != 0
	v.ClientErr = clientErr(o, f, v, signal)
	return v
}

func (p *Provider) manifestOf(o *plugin.Outcome) *manifest.Manifest {
	if o.Credential != nil {
		if m, ok := p.bound.Load(o.Credential.UpstreamID); ok {
			return m.(*manifest.Manifest)
		}
	}
	if p.defaultManifest != "" {
		if m, ok := p.idx().Get(p.defaultManifest); ok {
			return m
		}
	}
	return nil
}

// facts is what Classify reads from an outcome once.
type facts struct {
	message    string
	codes      []string // upstream code/type values, lower-cased
	text       string   // lower-cased message + body + transport error
	retryAfter time.Duration
	resetAt    time.Time
	noRetry    bool
}

func readFacts(o *plugin.Outcome, now time.Time) facts {
	var f facts
	if o.Signal != nil {
		f.message = o.Signal.Message
		if o.Signal.Code != "" {
			f.codes = append(f.codes, strings.ToLower(o.Signal.Code))
		}
	}
	body := o.Body
	if gjson.ValidBytes(body) {
		for _, path := range []string{"error.message", "message", "error", "detail", "msg", "0.error.message"} {
			if r := gjson.GetBytes(body, path); r.Type == gjson.String && r.Str != "" {
				if f.message == "" {
					f.message = r.Str
				}
				break
			}
		}
		for _, path := range []string{"error.code", "error.type", "error.status", "code", "type", "0.error.status"} {
			if r := gjson.GetBytes(body, path); r.Exists() && (r.Type == gjson.String || r.Type == gjson.Number) && r.String() != "" {
				f.codes = append(f.codes, strings.ToLower(r.String()))
			}
		}
	} else if len(body) > 0 && f.message == "" {
		f.message = strings.TrimSpace(truncate(string(body), 512))
	}
	parts := []string{f.message, string(body)}
	if o.Err != nil {
		parts = append(parts, o.Err.Error())
	}
	f.text = strings.ToLower(strings.Join(parts, "\n"))
	if o.Header != nil {
		f.retryAfter = retryAfter(o.Header, now)
		f.resetAt = resetAt(o.Header, now)
		f.noRetry = strings.EqualFold(o.Header.Get("X-Should-Retry"), "false")
	}
	return f
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func (f *facts) hasCode(c string) bool { return slices.Contains(f.codes, strings.ToLower(c)) }

func (f *facts) codePrefix(prefix string) bool {
	for _, c := range f.codes {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

func (f *facts) contains(words ...string) bool {
	for _, w := range words {
		if strings.Contains(f.text, w) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------- manifest rules

func matches(w manifest.Match, o *plugin.Outcome, f *facts) bool {
	if w.Status != 0 && o.Status != w.Status {
		return false
	}
	if len(w.StatusIn) > 0 && !slices.Contains(w.StatusIn, o.Status) {
		return false
	}
	if w.MessageContains != "" && !strings.Contains(f.text, strings.ToLower(w.MessageContains)) {
		return false
	}
	if w.Code != "" && !f.hasCode(w.Code) {
		return false
	}
	if w.HeaderPresent != "" && (o.Header == nil || o.Header.Get(w.HeaderPresent) == "") {
		return false
	}
	if w.Signal != "" && (o.Signal == nil || o.Signal.Kind != w.Signal) {
		return false
	}
	return true
}

func (p *Provider) ruleVerdict(m *manifest.Manifest, o *plugin.Outcome, f facts, now time.Time) (plugin.Verdict, ir.ErrorKind, bool) {
	for _, r := range m.Errors {
		if !matches(r.When, o, &f) {
			continue
		}
		rv := r.Verdict
		retry, cred := manifest.RuleDefaults(rv.Cause)
		if rv.Retry != "" {
			retry = rv.Retry
		}
		if rv.Credential != "" {
			cred = rv.Credential
		}
		v := plugin.Verdict{
			Cause:      plugin.Cause(rv.Cause),
			Retry:      plugin.RetryMode(retry),
			ModelScope: rv.ModelScope,
		}
		cooldown := time.Duration(rv.Cooldown)
		switch cred {
		case manifest.CredCooldown:
			v.Credential = plugin.CredCooldown
			v.Until = now.Add(firstPositive(cooldown, f.retryAfter, CooldownRateLimit))
		case manifest.CredCoolUntilReset:
			v.Credential = plugin.CredCooldown
			v.Until = untilReset(f, now, firstPositive(cooldown, CooldownQuota))
		case manifest.CredRefresh:
			v.Credential, v.Until = p.authAction(now, cooldown)
		default:
			v.Credential = plugin.CredAction(cred)
		}
		if len(rv.Downgrade) > 0 {
			v.Downgrade = make(map[string]json.RawMessage, len(rv.Downgrade))
			for path, val := range rv.Downgrade {
				raw, err := json.Marshal(val)
				if err != nil {
					continue
				}
				v.Downgrade[path] = raw
			}
		}
		return v, rv.Signal, true
	}
	return plugin.Verdict{}, "", false
}

func firstPositive(ds ...time.Duration) time.Duration {
	for _, d := range ds {
		if d > 0 {
			return d
		}
	}
	return 0
}

func untilReset(f facts, now time.Time, fallback time.Duration) time.Time {
	if !f.resetAt.IsZero() && f.resetAt.After(now) {
		return f.resetAt
	}
	if f.retryAfter > 0 {
		return now.Add(f.retryAfter)
	}
	return now.Add(fallback)
}

// authAction is refresh when the provider can refresh, else a cooldown.
func (p *Provider) authAction(now time.Time, cooldown time.Duration) (plugin.CredAction, time.Time) {
	if p.refresher != nil {
		return plugin.CredRefresh, time.Time{}
	}
	return plugin.CredCooldown, now.Add(firstPositive(cooldown, CooldownAuth))
}

// ---------------------------------------------------------------- built-in rules

var (
	banSubjects = []string{"account", "organization", "organisation", "workspace", "project", "api key", "user"}
	banWords    = []string{"disabled", "suspended", "deactivated", "banned", "terminated", "blocked"}
	// ambiguousWords may mean either a ban or a content rejection.
	ambiguousWords = []string{"usage policy", "usage policies", "terms of service", "acceptable use", "abuse", "flagged"}
	regionWords    = []string{"country", "region", "territory", "location is not supported", "unsupported_country"}
	contentCodes   = []string{"content_filter", "content_policy_violation", "content_filtered", "safety"}
	contextWords   = []string{"context length", "context window", "context_length", "prompt is too long",
		"too many tokens", "maximum context", "input is too long", "exceeds the maximum number of tokens"}
	overloadWords = []string{"overload", "overloaded"}
)

func (f *facts) suggestsBan() bool {
	for _, w := range banWords {
		if strings.Contains(f.text, w) && f.contains(banSubjects...) {
			return true
		}
	}
	return f.hasCode("account_deactivated") || f.hasCode("organization_disabled")
}

func (p *Provider) defaultVerdict(o *plugin.Outcome, f facts, now time.Time) (plugin.Verdict, ir.ErrorKind) {
	sig := ir.ErrorKind("")
	if o.Signal != nil {
		sig = o.Signal.Kind
	}
	keep := func(c plugin.Cause, r plugin.RetryMode) plugin.Verdict {
		return plugin.Verdict{Cause: c, Retry: r, Credential: plugin.CredKeep}
	}
	quarantine := func(c plugin.Cause) plugin.Verdict {
		return plugin.Verdict{Cause: c, Retry: plugin.RetryNone, Credential: plugin.CredQuarantine}
	}
	cooldown := func(c plugin.Cause, until time.Time, modelScope bool) plugin.Verdict {
		return plugin.Verdict{Cause: c, Retry: plugin.RetryNext, Credential: plugin.CredCooldown, Until: until, ModelScope: modelScope}
	}
	st := o.Status

	if st == 0 {
		switch {
		case !o.Sent:
			return keep(plugin.CauseTransient, plugin.RetryNext), ir.ErrUpstream
		case errors.Is(o.Err, context.Canceled):
			return keep(plugin.CauseTransient, plugin.RetryNone), ir.ErrUpstream
		case isTimeout(o.Err):
			return keep(plugin.CauseTransient, plugin.RetryNext), ir.ErrTimeout
		default:
			return keep(plugin.CauseTransient, plugin.RetryNext), ir.ErrUpstream
		}
	}

	if sig == ir.ErrContentFiltered || slices.ContainsFunc(contentCodes, f.hasCode) {
		return keep(plugin.CauseContent, plugin.RetryNone), ir.ErrContentFiltered
	}
	if (st == 400 || st == 401 || st == 403) && f.suggestsBan() {
		return quarantine(plugin.CauseBanned), ir.ErrPermission
	}
	if (st == 400 || st == 403) && f.contains(ambiguousWords...) {
		return quarantine(plugin.CauseUnknown), ir.ErrPermission
	}
	if st == 402 || sig == ir.ErrQuotaExhausted || f.codePrefix("insufficient_") {
		return cooldown(plugin.CauseQuota, untilReset(f, now, CooldownQuota), false), ir.ErrQuotaExhausted
	}
	if st == 401 {
		cred, until := p.authAction(now, 0)
		return plugin.Verdict{Cause: plugin.CauseAuthExpired, Retry: plugin.RetryNext, Credential: cred, Until: until}, ir.ErrUpstream
	}
	if st == 403 {
		if f.contains(regionWords...) {
			return keep(plugin.CauseTransient, plugin.RetryNext), ir.ErrPermission
		}
		return quarantine(plugin.CauseUnknown), ir.ErrPermission
	}
	if st == 529 || (st == 503 && (sig == ir.ErrOverloaded || f.contains(overloadWords...))) {
		return cooldown(plugin.CauseRateLimit, now.Add(firstPositive(f.retryAfter, CooldownOverloaded)), true), ir.ErrOverloaded
	}
	if st == 429 {
		d := f.retryAfter
		if d <= 0 && f.resetAt.After(now) {
			d = f.resetAt.Sub(now)
		}
		return cooldown(plugin.CauseRateLimit, now.Add(firstPositive(d, CooldownRateLimit)), true), ir.ErrRateLimited
	}
	if st == 408 || st == 409 || st >= 500 {
		k := ir.ErrUpstream
		if st == 408 || st == 504 {
			k = ir.ErrTimeout
		}
		return keep(plugin.CauseTransient, plugin.RetryNext), k
	}
	if (st == 400 || st == 413 || st == 422) && (sig == ir.ErrContextTooLong || f.contains(contextWords...)) {
		return keep(plugin.CauseRequest, plugin.RetryNone), ir.ErrContextTooLong
	}
	if st == 404 {
		return keep(plugin.CauseRequest, plugin.RetryNext), ir.ErrNotFound
	}
	if st == 405 {
		return keep(plugin.CauseRequest, plugin.RetryNext), ir.ErrInvalidRequest
	}
	k := ir.ErrInvalidRequest
	if sig != "" {
		k = sig
	}
	return keep(plugin.CauseRequest, plugin.RetryNone), k
}

func isTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// ---------------------------------------------------------------- client error

// credentialCause reports failures attributable to the relay's credential.
func credentialCause(c plugin.Cause) bool {
	switch c {
	case plugin.CauseAuthExpired, plugin.CauseBanned, plugin.CauseUnknown, plugin.CauseQuota:
		return true
	}
	return false
}

func clientErr(o *plugin.Outcome, f facts, v plugin.Verdict, signal ir.ErrorKind) *ir.GwError {
	e := &ir.GwError{}
	if o.Signal != nil {
		*e = *o.Signal
	}
	switch {
	case signal != "":
		e.Kind = signal
	case e.Kind == "":
		e.Kind = ir.ErrUpstream
	}
	if e.Message == "" {
		e.Message = f.message
	}
	if e.Message == "" {
		switch {
		case o.Err != nil:
			e.Message = "upstream request failed: " + o.Err.Error()
		case o.Status != 0:
			e.Message = fmt.Sprintf("upstream returned %d %s", o.Status, http.StatusText(o.Status))
		default:
			e.Message = "upstream request failed"
		}
	}
	if e.Code == "" && len(f.codes) > 0 {
		e.Code = f.codes[0]
		if o.Signal == nil || o.Signal.Code == "" {
			e.Code = firstRawCode(o.Body, e.Code)
		}
	}
	if f.retryAfter > 0 {
		e.RetryAfter = f.retryAfter
	}
	if v.Cause == plugin.CauseQuota && e.ResetAt.IsZero() && !f.resetAt.IsZero() {
		e.ResetAt = f.resetAt
	}
	if f.noRetry || v.Cause == plugin.CauseContent || v.Cause == plugin.CauseBanned {
		e.NoRetry = true
	}
	e.Status = o.Status
	if credentialCause(v.Cause) {
		e.Status = 0
	}
	e.Header = passthroughHeaders(o.Header)
	e.Upstream = o.Body
	return e
}

// firstRawCode returns the upstream's code with its original case.
func firstRawCode(body []byte, lower string) string {
	for _, path := range []string{"error.code", "error.type", "error.status", "code", "type", "0.error.status"} {
		if r := gjson.GetBytes(body, path); strings.ToLower(r.String()) == lower {
			return r.String()
		}
	}
	return lower
}
