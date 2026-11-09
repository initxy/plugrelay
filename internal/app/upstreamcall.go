package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/initxy/plugrelay/internal/egress"
	"github.com/initxy/plugrelay/pkg/plugin"
	"github.com/initxy/plugrelay/plugins/middleware/guardrails"
)

var _ guardrails.UpstreamCaller = (*host)(nil)

// maxSideCallBody bounds the answer of a plugin's own upstream call.
const maxSideCallBody = 4 << 20

// CallUpstream sends one request to an upstream for a plugin (a moderation
// check): the first endpoint's base URL plus path, a credential the router
// would pick for that upstream outside any request, the provider's auth
// and the credential's fixed egress. The router's health is not changed.
func (h *host) CallUpstream(ctx context.Context, upstreamID, method, path string, body []byte) (int, []byte, error) {
	a := h.a
	rt := a.runtime()
	up := rt.snap.Upstreams[upstreamID]
	tu, known := rt.snap.Table.Upstreams[upstreamID]
	if up == nil || !known || up.Provider == nil || len(tu.Endpoints) == 0 {
		return 0, nil, fmt.Errorf("upstream %q is not configured", upstreamID)
	}
	if !tu.Enabled {
		return 0, nil, fmt.Errorf("upstream %q is disabled", upstreamID)
	}
	ep := tu.Endpoints[0]
	plan, err := a.router.PlanUpstream(ctx, upstreamID, ep.ID, plugin.Caller{})
	if err != nil {
		return 0, nil, err
	}
	for _, cand := range plan.Candidates {
		cred := up.Credentials[cand.CredentialID]
		if cred == nil {
			continue
		}
		cred = a.pipe.CurrentSecret(upstreamID, cred)
		if cred.Proxy == "" && up.Proxy != "" {
			c := *cred
			c.Proxy = up.Proxy
			cred = &c
		}
		client, err := a.egress.Timed(cred.Proxy, up.AllowPrivate, a.defaultTimeouts().Override(up.Timeouts))
		if err != nil {
			return 0, nil, err
		}
		req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(ep.BaseURL, "/")+path, bytes.NewReader(body))
		if err != nil {
			return 0, nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		egress.SetExtraHeaders(req.Header, up.Headers, up.Provider.OwnedHeaders(up.Config))
		o := &plugin.Outgoing{Upstream: up.Config, Endpoint: &ep, Credential: cred, Req: req, Client: client}
		if err := up.Provider.Prepare(ctx, o); err != nil {
			return 0, nil, fmt.Errorf("prepare: %w", err)
		}
		resp, err := client.Do(o.Req)
		if err != nil {
			return 0, nil, plugin.RedactURLError(err)
		}
		data, err := io.ReadAll(io.LimitReader(resp.Body, maxSideCallBody))
		resp.Body.Close()
		return resp.StatusCode, data, err
	}
	return 0, nil, errors.New("no usable credential for upstream " + upstreamID)
}
