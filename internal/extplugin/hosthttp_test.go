package extplugin

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/initxy/plugrelay/pkg/plugin"
)

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func reply(via string) *http.Client {
	return &http.Client{Transport: roundTrip(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(via)), Header: http.Header{}}, nil
	})}
}

// egressHost answers host.http with the client it was asked for.
type egressHost struct {
	testHost
	asked []string
	fail  bool
}

func (h *egressHost) HTTPClient() *http.Client { return reply("direct") }
func (h *egressHost) ClientFor(u *plugin.Upstream, c *plugin.Credential) (*http.Client, error) {
	id := u.ID + "/"
	if c != nil {
		id += c.ID
	}
	h.asked = append(h.asked, id)
	if h.fail {
		return nil, errors.New("egress: proxy down")
	}
	return reply("credential"), nil
}

// host.http with an upstream credential takes that credential's egress;
// without one it goes direct.
func TestHostHTTPCredentialEgress(t *testing.T) {
	h := &egressHost{}
	in := &instance{host: h}
	call := func(params string) (string, error) {
		res, err := in.hostHTTP(context.Background(), json.RawMessage(params))
		if err != nil {
			return "", err
		}
		return *res.(wireHTTPResponse).Body, nil
	}
	if got, err := call(`{"url":"https://api.example.com/x"}`); err != nil || got != "direct" {
		t.Fatalf("plain: %q %v", got, err)
	}
	if got, err := call(`{"url":"https://api.example.com/x","upstream_id":"u","credential_id":"c"}`); err != nil || got != "credential" || h.asked[0] != "u/c" {
		t.Fatalf("credential: %q %v %v", got, err, h.asked)
	}
	if got, err := call(`{"url":"https://api.example.com/x","upstream_id":"u"}`); err != nil || got != "credential" || h.asked[1] != "u/" {
		t.Fatalf("upstream: %q %v %v", got, err, h.asked)
	}
	if _, err := call(`{"url":"https://api.example.com/x","credential_id":"c"}`); err == nil {
		t.Fatal("credential without upstream accepted")
	}
	h.fail = true
	if _, err := call(`{"url":"https://api.example.com/x","upstream_id":"u","credential_id":"c"}`); err == nil {
		t.Fatal("failed egress fell back to direct")
	}
}
