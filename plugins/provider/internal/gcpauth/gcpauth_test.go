package gcpauth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/initxy/plugrelay/plugins/provider/internal/cloudtest"
)

func TestExchangeAgainstFakeTokenEndpoint(t *testing.T) {
	ts := cloudtest.NewTokenServer(t)
	sa, err := ParseServiceAccount([]byte(ts.ServiceAccount))
	if err != nil {
		t.Fatal(err)
	}
	if sa.ProjectID != "conformance-project" || sa.TokenURI != ts.URL {
		t.Errorf("parsed %+v", sa)
	}
	now := time.Now()
	tok, err := sa.Exchange(context.Background(), http.DefaultClient, ScopeCloudPlatform, now)
	if err != nil {
		t.Fatal(err)
	}
	if tok.AccessToken != ts.Issued()[0] || !tok.Expiry.Equal(now.Add(time.Hour)) {
		t.Errorf("token %+v", tok)
	}
	c := ts.Claims()[0]
	if int64(c["iat"].(float64)) != now.Unix() || int64(c["exp"].(float64)) != now.Add(time.Hour).Unix() {
		t.Errorf("claims %v", c)
	}

	ts.Fail("invalid_grant")
	_, err = sa.Exchange(context.Background(), http.DefaultClient, ScopeCloudPlatform, now)
	var ee *ExchangeError
	if !errors.As(err, &ee) || ee.Status != 400 || !strings.Contains(ee.Message, "invalid_grant") {
		t.Errorf("refusal: %v", err)
	}
	if strings.Contains(err.Error(), "PRIVATE KEY") {
		t.Error("error leaks the key")
	}
}

func TestParseServiceAccountRejects(t *testing.T) {
	ts := cloudtest.NewTokenServer(t)
	var good map[string]string
	_ = json.Unmarshal([]byte(ts.ServiceAccount), &good)
	edit := func(k, v string) []byte {
		m := map[string]string{}
		for kk, vv := range good {
			m[kk] = vv
		}
		if v == "" {
			delete(m, k)
		} else {
			m[k] = v
		}
		b, _ := json.Marshal(m)
		return b
	}
	for name, in := range map[string][]byte{
		"not json":       []byte("ya29.token"),
		"user creds":     edit("type", "authorized_user"),
		"no email":       edit("client_email", ""),
		"bad pem":        edit("private_key", "not a key"),
		"plain http uri": edit("token_uri", "http://oauth2.example.com/token"),
	} {
		if _, err := ParseServiceAccount(in); err == nil {
			t.Errorf("%s: accepted", name)
		} else if strings.Contains(err.Error(), "BEGIN") {
			t.Errorf("%s: error leaks the key: %v", name, err)
		}
	}
	sa, err := ParseServiceAccount(edit("token_uri", ""))
	if err != nil || sa.TokenURI != DefaultTokenURI {
		t.Errorf("default token_uri: %v %+v", err, sa)
	}
}

func TestCache(t *testing.T) {
	var c Cache
	c.Margin, c.FailFor = 5*time.Minute, 30*time.Second
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	var calls atomic.Int64
	fail := false
	fetch := func(context.Context) (Token, error) {
		n := calls.Add(1)
		if fail {
			return Token{}, errors.New("refused")
		}
		return Token{AccessToken: "t" + string(rune('0'+n)), Expiry: now.Add(time.Hour)}, nil
	}
	ctx := context.Background()
	get := func() (string, error) {
		tok, err := c.Get(ctx, "k", clock, fetch)
		return tok.AccessToken, err
	}

	if tok, _ := get(); tok != "t1" {
		t.Fatalf("first = %s", tok)
	}
	now = now.Add(54 * time.Minute)
	if tok, _ := get(); tok != "t1" || calls.Load() != 1 {
		t.Errorf("cached token not reused: %s, %d calls", tok, calls.Load())
	}
	now = now.Add(2 * time.Minute) // inside the 5 minute margin
	if tok, _ := get(); tok != "t2" {
		t.Errorf("not refreshed before expiry: %s", tok)
	}

	c.Invalidate("k")
	fail = true
	if _, err := get(); err == nil {
		t.Fatal("failure not reported")
	}
	fail = false
	if _, err := get(); err == nil || calls.Load() != 4-1 {
		t.Errorf("failure not remembered: err=%v calls=%d", err, calls.Load())
	}
	now = now.Add(31 * time.Second)
	if tok, err := get(); err != nil || tok != "t4" {
		t.Errorf("after FailFor: %s %v", tok, err)
	}

	// Concurrent callers share one exchange.
	c.Invalidate("k")
	before := calls.Load()
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() { defer wg.Done(); _, _ = get() }()
	}
	wg.Wait()
	if n := calls.Load() - before; n != 1 {
		t.Errorf("%d exchanges for concurrent callers, want 1", n)
	}
}
