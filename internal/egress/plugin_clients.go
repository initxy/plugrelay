package egress

import (
	"context"
	"io"
	"net/http"
	"net/http/httptrace"

	"github.com/initxy/plugrelay/pkg/plugin"
)

// ForAttempt returns the client a provider uses for calls of its own during
// one attempt (a token exchange in Prepare, a Sender's request): the
// credential's fixed egress, with every request bound to w. Getting a
// connection ends w's connect timer, and a request is canceled when w's
// context is (a timer fired or the attempt ended).
func (c *Client) ForAttempt(cred *plugin.Credential, allowPrivate bool, w *Watch) (*http.Client, error) {
	hc, err := c.For(cred, allowPrivate)
	if err != nil {
		return nil, err
	}
	hc.Transport = watched{rt: hc.Transport, w: w}
	return hc, nil
}

// Timed returns the client of a fixed egress (see HTTPClient) whose every
// request runs under its own timers t, for calls made outside an attempt:
// model listing, quota probes, credential refresh.
func (c *Client) Timed(proxy string, allowPrivate bool, t Timeouts) (*http.Client, error) {
	hc, err := c.HTTPClient(proxy, allowPrivate)
	if err != nil {
		return nil, err
	}
	hc.Transport = timed{rt: hc.Transport, t: t}
	return hc, nil
}

type watched struct {
	rt http.RoundTripper
	w  *Watch
}

func (t watched) RoundTrip(r *http.Request) (*http.Response, error) {
	ctx, cancel := context.WithCancel(r.Context())
	stop := context.AfterFunc(t.w.Context(), cancel)
	release := func() { stop(); cancel() }
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{GotConn: func(httptrace.GotConnInfo) { t.w.Connected() }})
	resp, err := t.rt.RoundTrip(r.WithContext(ctx))
	if err != nil {
		release()
		return nil, t.w.Err(err)
	}
	rb := &releaseBody{rc: resp.Body, w: t.w, release: release}
	resp.Body = rb
	if rw, ok := rb.rc.(io.ReadWriteCloser); ok && resp.StatusCode == http.StatusSwitchingProtocols {
		// An upgraded connection (WebSocket) is written to as well.
		resp.Body = &upgradedBody{releaseBody: rb, w: rw}
	}
	return resp, nil
}

type upgradedBody struct {
	*releaseBody
	w io.Writer
}

func (b *upgradedBody) Write(p []byte) (int, error) { return b.w.Write(p) }

// releaseBody maps read errors to the attempt's fired timer and releases
// the request context on Close; the attempt's watch keeps running.
type releaseBody struct {
	rc      io.ReadCloser
	w       *Watch
	release func()
}

func (b *releaseBody) Read(p []byte) (int, error) {
	n, err := b.rc.Read(p)
	return n, b.w.Err(err)
}

func (b *releaseBody) Close() error {
	defer b.release()
	return b.rc.Close()
}

type timed struct {
	rt http.RoundTripper
	t  Timeouts
}

func (t timed) RoundTrip(r *http.Request) (*http.Response, error) {
	w := Start(r.Context(), t.t)
	ctx := httptrace.WithClientTrace(w.Context(), &httptrace.ClientTrace{GotConn: func(httptrace.GotConnInfo) { w.Connected() }})
	resp, err := t.rt.RoundTrip(r.WithContext(ctx))
	if err != nil {
		err = w.Err(err)
		w.Stop()
		return nil, err
	}
	resp.Body = w.Body(resp.Body)
	return resp, nil
}
