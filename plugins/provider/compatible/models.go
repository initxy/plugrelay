package compatible

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/plugin"
	"github.com/initxy/plugrelay/plugins/provider/manifest"
	"github.com/tidwall/gjson"
)

const (
	maxListPages = 20
	maxBodyBytes = 8 << 20
)

// ListModels implements plugin.ModelLister: the manifest's static list, or
// a GET of models.list through the core egress client. Responses shaped as
// {"data": [...]}, {"models": [...]} or a bare array are understood, with
// cursor pagination (has_more/last_id, nextPageToken).
func (p *Provider) ListModels(ctx context.Context, u *plugin.Upstream, c *plugin.Credential) ([]ir.ModelInfo, error) {
	m, err := p.Manifest(u)
	if err != nil {
		return nil, err
	}
	if len(m.Models.Static) > 0 {
		out := make([]ir.ModelInfo, len(m.Models.Static))
		for i, mi := range m.Models.Static {
			out[i] = mi.IR()
		}
		return out, nil
	}
	if m.Models.List == "" {
		return nil, fmt.Errorf("manifest %s declares no model list", m.ID)
	}
	var out []ir.ModelInfo
	query := url.Values{}
	for page := 0; page < maxListPages; page++ {
		body, err := p.get(ctx, u, c, m.Models.Endpoint, http.MethodGet, m.Models.List, query)
		if err != nil {
			return nil, err
		}
		items := gjson.ParseBytes(body)
		if !items.IsArray() {
			items = gjson.GetBytes(body, "data")
			if !items.Exists() {
				items = gjson.GetBytes(body, "models")
			}
		}
		for _, it := range items.Array() {
			if mi, ok := parseModel(it); ok {
				out = append(out, mi)
			}
		}
		next := url.Values{}
		if gjson.GetBytes(body, "has_more").Bool() {
			if last := gjson.GetBytes(body, "last_id").String(); last != "" {
				next.Set("after_id", last)
			}
		} else if tok := gjson.GetBytes(body, "nextPageToken").String(); tok != "" {
			next.Set("pageToken", tok)
		}
		if len(next) == 0 {
			return out, nil
		}
		query = next
	}
	return out, nil
}

func parseModel(it gjson.Result) (ir.ModelInfo, bool) {
	id := it.Get("id").String()
	if id == "" {
		id = strings.TrimPrefix(it.Get("name").String(), "models/")
	}
	if id == "" {
		return ir.ModelInfo{}, false
	}
	mi := ir.ModelInfo{
		ID:          id,
		DisplayName: firstString(it, "display_name", "displayName"),
		OwnedBy:     it.Get("owned_by").String(),
	}
	mi.ContextWindow = int(firstInt(it, "context_window", "context_length", "max_input_tokens", "inputTokenLimit"))
	mi.MaxOutput = int(firstInt(it, "max_output", "max_tokens", "outputTokenLimit", "top_provider.max_completion_tokens"))
	if c := it.Get("created"); c.Type == gjson.Number {
		mi.Created = c.Int()
	} else if t, err := time.Parse(time.RFC3339, it.Get("created_at").String()); err == nil {
		mi.Created = t.Unix()
	}
	return mi, true
}

func firstString(it gjson.Result, paths ...string) string {
	for _, p := range paths {
		if s := it.Get(p).String(); s != "" {
			return s
		}
	}
	return ""
}

func firstInt(it gjson.Result, paths ...string) int64 {
	for _, p := range paths {
		if r := it.Get(p); r.Type == gjson.Number {
			return r.Int()
		}
	}
	return 0
}

// Quota implements plugin.QuotaProber when the manifest declares a probe;
// otherwise it returns plugin.ErrNoQuota.
func (p *Provider) Quota(ctx context.Context, u *plugin.Upstream, c *plugin.Credential) (plugin.Quota, error) {
	m, err := p.Manifest(u)
	if err != nil {
		return plugin.Quota{}, err
	}
	q := m.Quota
	if q == nil {
		return plugin.Quota{Remaining: -1}, plugin.ErrNoQuota
	}
	method := q.Method
	if method == "" {
		method = http.MethodGet
	}
	body, err := p.get(ctx, u, c, q.Endpoint, method, q.Path, nil)
	if err != nil {
		return plugin.Quota{Remaining: -1}, err
	}
	return readQuota(q, body, time.Now()), nil
}

// readQuota applies a probe's paths to its response. A null limit (an
// unlimited key) leaves the fraction unknown.
func readQuota(q *manifest.QuotaProbe, body []byte, now time.Time) plugin.Quota {
	out := plugin.Quota{Remaining: -1}
	if q.RemainingFraction != "" {
		if r := gjson.GetBytes(body, q.RemainingFraction); r.Type == gjson.Number {
			out.Remaining = clamp01(r.Float())
		}
	} else {
		rem, lim := gjson.GetBytes(body, q.Remaining), gjson.GetBytes(body, q.Limit)
		if rem.Type == gjson.Number && lim.Type == gjson.Number && lim.Float() > 0 {
			out.Remaining = clamp01(rem.Float() / lim.Float())
			out.Left, out.Limit = max(0, rem.Float()), lim.Float()
		}
	}
	if q.Reset != "" {
		r := gjson.GetBytes(body, q.Reset)
		if r.Type == gjson.Number {
			out.ResetAt = time.Unix(r.Int(), 0)
		} else if t, err := time.Parse(time.RFC3339, r.String()); err == nil {
			out.ResetAt = t
		}
	}
	if out.ResetAt.IsZero() && q.ResetPeriod != "" {
		out.ResetAt = nextPeriod(gjson.GetBytes(body, q.ResetPeriod).String(), now)
	}
	return out
}

// nextPeriod is the next 00:00 UTC starting a day, week (Monday) or month;
// zero for anything else.
func nextPeriod(period string, now time.Time) time.Time {
	now = now.UTC()
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	switch period {
	case "daily":
		return day.AddDate(0, 0, 1)
	case "weekly":
		return day.AddDate(0, 0, 7-(int(now.Weekday())+6)%7)
	case "monthly":
		return time.Date(now.Year(), now.Month()+1, 1, 0, 0, 0, 0, time.UTC)
	}
	return time.Time{}
}

func clamp01(f float64) float64 { return max(0, min(1, f)) }

// get calls a path on an endpoint's base URL with the credential applied
// the same way Prepare applies it.
func (p *Provider) get(ctx context.Context, u *plugin.Upstream, c *plugin.Credential, endpointID, method, path string, query url.Values) ([]byte, error) {
	client, err := p.ClientFor(u, c)
	if err != nil {
		return nil, err
	}
	eps, err := p.Endpoints(u)
	if err != nil {
		return nil, err
	}
	ep := &eps[0]
	if endpointID != "" {
		for i := range eps {
			if eps[i].ID == endpointID {
				ep = &eps[i]
			}
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, ep.BaseURL+path, nil)
	if err != nil {
		return nil, err
	}
	q := req.URL.Query()
	for k, vs := range query {
		q[k] = vs
	}
	req.URL.RawQuery = q.Encode()
	out := &plugin.Outgoing{Upstream: u, Endpoint: ep, Credential: c, Req: req, Client: client}
	if err := p.Prepare(ctx, out); err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		// The query may carry the credential (auth.query_param).
		return nil, plugin.RedactURLError(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("%s %s: status %s: %s", method, path, strconv.Itoa(resp.StatusCode), truncate(strings.TrimSpace(string(body)), 256))
	}
	return body, nil
}
