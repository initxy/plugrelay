package apikey

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/plugin"
)

var now = time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)

type lookupFunc func(string) (*plugin.Caller, KeyInfo, error)

func (f lookupFunc) LookupKey(_ context.Context, k string) (*plugin.Caller, KeyInfo, error) {
	return f(k)
}

func TestExtractKey(t *testing.T) {
	tests := []struct {
		name   string
		header map[string]string
		url    string
		want   string
	}{
		{"bearer", map[string]string{"Authorization": "Bearer sk-a"}, "/", "sk-a"},
		{"bearer lowercase", map[string]string{"Authorization": "bearer  sk-a "}, "/", "sk-a"},
		{"x-api-key", map[string]string{"x-api-key": "sk-b"}, "/", "sk-b"},
		{"goog", map[string]string{"x-goog-api-key": "sk-c"}, "/", "sk-c"},
		{"query", nil, "/v1beta/models/m:generateContent?key=sk-d", "sk-d"},
		{"bearer wins", map[string]string{"Authorization": "Bearer sk-a", "x-api-key": "sk-b"}, "/?key=x", "sk-a"},
		{"basic ignored", map[string]string{"Authorization": "Basic Zm9v", "x-api-key": "sk-b"}, "/", "sk-b"},
		{"none", nil, "/", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", tt.url, nil)
			for k, v := range tt.header {
				r.Header.Set(k, v)
			}
			if got := ExtractKey(r); got != tt.want {
				t.Errorf("got %q want %q", got, tt.want)
			}
		})
	}
}

func TestAuthenticate(t *testing.T) {
	keys := map[string]KeyInfo{
		"good":     {},
		"disabled": {Disabled: true},
		"expired":  {Expires: now.Add(-time.Second)},
		"future":   {Expires: now.Add(time.Hour)},
		"ip":       {AllowedIPs: []string{"10.0.0.0/8", "192.168.1.5"}},
		"client":   {AllowClient: func(h http.Header) bool { return h.Get("User-Agent") == "allowed/1" }},
	}
	lookup := lookupFunc(func(k string) (*plugin.Caller, KeyInfo, error) {
		if k == "boom" {
			return nil, KeyInfo{}, errors.New("db down")
		}
		info, ok := keys[k]
		if !ok {
			return nil, KeyInfo{}, nil
		}
		return &plugin.Caller{KeyID: k}, info, nil
	})
	a := New(lookup, WithClock(func() time.Time { return now }))
	tests := []struct {
		key, remote, ua string
		want            ir.ErrorKind // "" = success
		noCaller        bool
	}{
		{key: "good"},
		{key: "", noCaller: true},
		{key: "missing", want: ir.ErrAuthentication},
		{key: "boom", want: ir.ErrInternal},
		{key: "disabled", want: ir.ErrAuthentication},
		{key: "expired", want: ir.ErrAuthentication},
		{key: "future"},
		{key: "ip", remote: "10.2.3.4:5555"},
		{key: "ip", remote: "192.168.1.5:1"},
		{key: "ip", remote: "[::ffff:10.0.0.1]:1"},
		{key: "ip", remote: "192.168.1.6:1", want: ir.ErrPermission},
		{key: "client", ua: "allowed/1"},
		{key: "client", ua: "curl/8", want: ir.ErrPermission},
		{key: "client", want: ir.ErrPermission},
	}
	for _, tt := range tests {
		t.Run(tt.key+"/"+tt.remote+tt.ua, func(t *testing.T) {
			r := httptest.NewRequest("POST", "/v1/messages", nil)
			if tt.key != "" {
				r.Header.Set("x-api-key", tt.key)
			}
			if tt.remote != "" {
				r.RemoteAddr = tt.remote
			}
			r.Header.Set("User-Agent", tt.ua)
			c, err := a.Authenticate(r)
			if tt.want != "" {
				var gw *ir.GwError
				if !errors.As(err, &gw) || gw.Kind != tt.want {
					t.Fatalf("err = %v, want kind %s", err, tt.want)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if tt.noCaller != (c == nil) {
				t.Fatalf("caller = %v", c)
			}
		})
	}
}

type host struct{ plugin.Host }

func (host) KeyLookup() KeyLookup {
	return lookupFunc(func(string) (*plugin.Caller, KeyInfo, error) { return &plugin.Caller{KeyID: "k"}, KeyInfo{}, nil })
}

func TestInitFromHost(t *testing.T) {
	a := New(nil)
	if err := a.Init(context.Background(), struct{ plugin.Host }{}); err == nil {
		t.Fatal("expected error without lookup")
	}
	if err := a.Init(context.Background(), host{}); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", "/?key=x", nil)
	if c, err := a.Authenticate(r); err != nil || c.KeyID != "k" {
		t.Fatal(c, err)
	}
	if f, ok := plugin.Lookup(ID); !ok || f().Info().ID != ID {
		t.Fatal("not registered")
	}
}

// ipHost resolves every client to a fixed address, as the core does behind
// a trusted proxy.
type ipHost struct {
	host
	ip string
}

func (h ipHost) ClientIP(*http.Request) string { return h.ip }

func (ipHost) KeyLookup() KeyLookup {
	return lookupFunc(func(string) (*plugin.Caller, KeyInfo, error) {
		return &plugin.Caller{KeyID: "k"}, KeyInfo{AllowedIPs: []string{"203.0.113.0/24"}}, nil
	})
}

func TestClientIPFromHost(t *testing.T) {
	r := httptest.NewRequest("GET", "/?key=x", nil) // RemoteAddr 192.0.2.1
	a := New(nil)
	if err := a.Init(context.Background(), ipHost{ip: "203.0.113.9"}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Authenticate(r); err != nil {
		t.Fatalf("host-resolved address rejected: %v", err)
	}
}
