package pipeline

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/initxy/plugrelay/internal/router"
	"github.com/initxy/plugrelay/pkg/plugin"
)

// refreshingProvider asks for a refresh on 401 and renews to "sk-new".
type refreshingProvider struct {
	plugin.Provider
	mu    sync.Mutex
	calls int
	fail  bool
}

func (p *refreshingProvider) Classify(o *plugin.Outcome) plugin.Verdict {
	if o.Status == 401 {
		return plugin.Verdict{Cause: plugin.CauseAuthExpired, Retry: plugin.RetryNext, Credential: plugin.CredRefresh, Sent: true}
	}
	return p.Provider.Classify(o)
}

func (p *refreshingProvider) Refresh(_ context.Context, _ *plugin.Upstream, c *plugin.Credential) (string, time.Time, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	if p.fail {
		return "", time.Time{}, errors.New("refresh token revoked")
	}
	return "sk-new", time.Unix(2_000_000_000, 0), nil
}

type secretStore struct {
	mu      sync.Mutex
	secrets map[string]string
}

func (s *secretStore) SaveCredentialState(context.Context, router.CredEvent) error { return nil }
func (s *secretStore) LoadCredentialSecret(_ context.Context, id string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.secrets[id], nil
}

func (s *secretStore) SaveCredentialSecret(_ context.Context, id, secret string, _ time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.secrets[id] = secret
	return nil
}

// A refresh verdict renews the credential under a lock, persists the new
// secret and resends the attempt once with it; later requests use it
// directly. Without a lock, or when the refresh fails, the credential just
// sits out.
func TestRefreshVerdictRenewsAndRetries(t *testing.T) {
	upstream := func() *fakeUpstream {
		return newFake(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
			if r.Header.Get("Authorization") != "Bearer sk-new" {
				w.WriteHeader(401)
				_, _ = io.WriteString(w, `{"error":{"message":"token expired","type":"authentication_error"}}`)
				return
			}
			jsonReply(200, chatResp)(w, r, nil)
		})
	}
	setup := func(withLock, fail bool) (*env, *fakeUpstream, *refreshingProvider, *secretStore) {
		up := upstream()
		st := &secretStore{secrets: map[string]string{}}
		mem := newMemState()
		e := newEnv(t, "gpt-x", []upSpec{{id: "o", protocol: "openai-chat", issuer: "i", fake: up}}, func(d *Deps) {
			d.Credentials = st
			if withLock {
				d.Lock = mem.Lock
			}
		})
		rp := &refreshingProvider{Provider: e.snap.Upstreams["o"].Provider, fail: fail}
		e.snap.Upstreams["o"].Provider = rp
		return e, up, rp, st
	}

	e, up, rp, st := setup(true, false)
	if resp, body := e.call("openai-chat", "/v1/chat/completions", chatReq); resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	if up.calls() != 2 || rp.calls != 1 || st.secrets["c1"] != "sk-new" {
		t.Fatalf("upstream calls %d, refreshes %d, stored %q", up.calls(), rp.calls, st.secrets["c1"])
	}
	if resp, _ := e.call("openai-chat", "/v1/chat/completions", chatReq); resp.StatusCode != 200 || up.calls() != 3 || rp.calls != 1 {
		t.Fatalf("next request: %d, upstream calls %d, refreshes %d", resp.StatusCode, up.calls(), rp.calls)
	}
	if rec := e.records.last(t); len(rec.Attempts) != 1 {
		t.Errorf("next request attempts %d", len(rec.Attempts))
	}
	// A rebuilt snapshot carrying the new secret drops the override.
	e.snap.Upstreams["o"].Credentials["c1"].Secret = "sk-new"
	if resp, _ := e.call("openai-chat", "/v1/chat/completions", chatReq); resp.StatusCode != 200 {
		t.Fatal("after rebuild")
	}

	for name, c := range map[string]struct{ lock, fail bool }{"no lock": {false, false}, "refresh fails": {true, true}} {
		t.Run(name, func(t *testing.T) {
			e, up, rp, st := setup(c.lock, c.fail)
			if resp, _ := e.call("openai-chat", "/v1/chat/completions", chatReq); resp.StatusCode == 200 {
				t.Fatal("served without a refreshed secret")
			}
			if up.calls() != 1 || len(st.secrets) != 0 || (!c.lock && rp.calls != 0) {
				t.Fatalf("upstream calls %d, stored %v, refreshes %d", up.calls(), st.secrets, rp.calls)
			}
		})
	}
}

// flakyStore refuses secrets until failing is cleared.
type flakyStore struct {
	secretStore
	failing bool
	tries   int
}

func (s *flakyStore) SaveCredentialSecret(ctx context.Context, id, secret string, exp time.Time) error {
	s.mu.Lock()
	s.tries++
	failing := s.failing
	s.mu.Unlock()
	if failing {
		return errors.New("database is locked")
	}
	return s.secretStore.SaveCredentialSecret(ctx, id, secret, exp)
}

// When the store refuses a refreshed secret the request is still served
// with it (it is valid, and a rotating refresh token already spent the old
// one), the save is retried a few times, then the credential is flagged for
// an operator and counted; a later PersistPending saves it and clears the
// flag.
func TestRefreshedSecretSaveFailure(t *testing.T) {
	defer func(b []time.Duration) { secretSaveBackoff = b }(secretSaveBackoff)
	secretSaveBackoff = []time.Duration{time.Millisecond, time.Millisecond}
	up := newFake(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		if r.Header.Get("Authorization") != "Bearer sk-new" {
			w.WriteHeader(401)
			_, _ = io.WriteString(w, `{"error":{"message":"token expired","type":"authentication_error"}}`)
			return
		}
		jsonReply(200, chatResp)(w, r, nil)
	})
	st := &flakyStore{secretStore: secretStore{secrets: map[string]string{}}, failing: true}
	mem := newMemState()
	e := newEnv(t, "gpt-x", []upSpec{{id: "o", protocol: "openai-chat", issuer: "i", fake: up}}, func(d *Deps) {
		d.Credentials = st
		d.Lock = mem.Lock
	})
	e.snap.Upstreams["o"].Provider = &refreshingProvider{Provider: e.snap.Upstreams["o"].Provider}
	attention := func() string {
		for _, ul := range e.rt.Live(context.Background()).Upstreams {
			for _, cl := range ul.Credentials {
				if ul.ID == "o" && cl.ID == "c1" {
					return cl.Attention
				}
			}
		}
		return "?"
	}

	if resp, body := e.call("openai-chat", "/v1/chat/completions", chatReq); resp.StatusCode != 200 {
		t.Fatalf("not served with the refreshed secret: %d %s", resp.StatusCode, body)
	}
	if st.tries != 1+len(secretSaveBackoff) || len(st.secrets) != 0 {
		t.Fatalf("save tries %d, stored %v", st.tries, st.secrets)
	}
	if e.p.SecretSaveFailures() != 1 || attention() != attentionUnsaved {
		t.Fatalf("failures %d, attention %q", e.p.SecretSaveFailures(), attention())
	}
	// Still in rotation, still using the in-memory secret.
	if resp, _ := e.call("openai-chat", "/v1/chat/completions", chatReq); resp.StatusCode != 200 || up.calls() != 3 {
		t.Fatalf("next request: %d, upstream calls %d", resp.StatusCode, up.calls())
	}

	ctx := context.Background()
	if left := e.p.PersistPending(ctx); left != 1 || attention() == "" {
		t.Fatalf("store still failing: left %d, attention %q", left, attention())
	}
	st.mu.Lock()
	st.failing = false
	st.mu.Unlock()
	if left := e.p.PersistPending(ctx); left != 0 || st.secrets["c1"] != "sk-new" || attention() != "" {
		t.Fatalf("after recovery: left %d, stored %v, attention %q", left, st.secrets, attention())
	}
	if left := e.p.PersistPending(ctx); left != 0 || st.tries != 5 {
		t.Fatalf("saved secret retried again: tries %d", st.tries)
	}
}

// A pending secret the operator replaced in the meantime is dropped, not
// written over the new one.
func TestPendingSecretDroppedWhenReplaced(t *testing.T) {
	defer func(b []time.Duration) { secretSaveBackoff = b }(secretSaveBackoff)
	secretSaveBackoff = nil
	up := newFake(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		if r.Header.Get("Authorization") != "Bearer sk-new" {
			w.WriteHeader(401)
			return
		}
		jsonReply(200, chatResp)(w, r, nil)
	})
	st := &flakyStore{secretStore: secretStore{secrets: map[string]string{}}, failing: true}
	mem := newMemState()
	e := newEnv(t, "gpt-x", []upSpec{{id: "o", protocol: "openai-chat", issuer: "i", fake: up}}, func(d *Deps) {
		d.Credentials = st
		d.Lock = mem.Lock
	})
	e.snap.Upstreams["o"].Provider = &refreshingProvider{Provider: e.snap.Upstreams["o"].Provider}
	if resp, _ := e.call("openai-chat", "/v1/chat/completions", chatReq); resp.StatusCode != 200 {
		t.Fatal(resp.StatusCode)
	}
	e.snap.Upstreams["o"].Credentials["c1"].Secret = "sk-operator"
	st.failing = false
	if left := e.p.PersistPending(context.Background()); left != 0 || len(st.secrets) != 0 {
		t.Fatalf("left %d, stored %v", left, st.secrets)
	}
	for _, ul := range e.rt.Live(context.Background()).Upstreams {
		for _, cl := range ul.Credentials {
			if cl.Attention != "" {
				t.Fatalf("attention kept after the operator replaced the secret: %q", cl.Attention)
			}
		}
	}
}

// Two requests fail with the same old secret at once: one refreshes, the
// other waits for it and retries with the new secret instead of spending
// the (possibly rotating) refresh token a second time.
func TestConcurrentRefreshRunsOnce(t *testing.T) {
	var mu sync.Mutex
	stale := 0
	both := make(chan struct{})
	up := newFake(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		if r.Header.Get("Authorization") != "Bearer sk-new" {
			mu.Lock()
			stale++
			if stale == 2 {
				close(both)
			}
			mu.Unlock()
			select { // hold both stale requests until each has failed
			case <-both:
			case <-time.After(5 * time.Second):
			}
			w.WriteHeader(401)
			_, _ = io.WriteString(w, `{"error":{"message":"token expired","type":"authentication_error"}}`)
			return
		}
		jsonReply(200, chatResp)(w, r, nil)
	})
	st := &secretStore{secrets: map[string]string{}}
	mem := newMemState()
	e := newEnv(t, "gpt-x", []upSpec{{id: "o", protocol: "openai-chat", issuer: "i", fake: up}}, func(d *Deps) {
		d.Credentials, d.Lock = st, mem.Lock
	})
	rp := &refreshingProvider{Provider: e.snap.Upstreams["o"].Provider}
	e.snap.Upstreams["o"].Provider = rp
	var wg sync.WaitGroup
	codes := make([]int, 2)
	for i := range 2 {
		wg.Go(func() {
			resp, _ := e.call("openai-chat", "/v1/chat/completions", chatReq)
			codes[i] = resp.StatusCode
		})
	}
	wg.Wait()
	rp.mu.Lock()
	defer rp.mu.Unlock()
	if codes[0] != 200 || codes[1] != 200 || rp.calls != 1 {
		t.Fatalf("statuses %v, refreshes %d (want both 200, one refresh)", codes, rp.calls)
	}
}

// Another node refreshed the credential and saved it; this node's snapshot
// still has the old secret. It must reload the stored secret instead of
// spending the (possibly rotating) refresh token again — and when the new
// secret is not readable yet, it must still not refresh.
func TestRefreshOnOtherNodeReloadsSecret(t *testing.T) {
	t.Run("not saved yet", func(t *testing.T) {
		up := newFake(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
			w.WriteHeader(401)
			_, _ = io.WriteString(w, `{"error":{"message":"token expired","type":"authentication_error"}}`)
		})
		st := &secretStore{secrets: map[string]string{}}
		mem := newMemState()
		e := newEnv(t, "gpt-x", []upSpec{{id: "o", protocol: "openai-chat", issuer: "i", fake: up}}, func(d *Deps) {
			d.Credentials, d.Lock = st, mem.Lock
		})
		rp := &refreshingProvider{Provider: e.snap.Upstreams["o"].Provider}
		e.snap.Upstreams["o"].Provider = rp
		old := e.snap.Upstreams["o"].Credentials["c1"].Secret
		_ = e.rt.MarkRefreshed(context.Background(), "o", "c1", secretFingerprint(old), time.Minute)
		if resp, _ := e.call("openai-chat", "/v1/chat/completions", chatReq); resp.StatusCode == 200 {
			t.Fatal("served with a spent secret")
		}
		rp.mu.Lock()
		defer rp.mu.Unlock()
		if rp.calls != 0 {
			t.Fatalf("refreshed %d times with a token another node already spent", rp.calls)
		}
	})
	up := newFake(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		if r.Header.Get("Authorization") != "Bearer sk-new" {
			w.WriteHeader(401)
			_, _ = io.WriteString(w, `{"error":{"message":"token expired","type":"authentication_error"}}`)
			return
		}
		jsonReply(200, chatResp)(w, r, nil)
	})
	st := &secretStore{secrets: map[string]string{"c1": "sk-new"}} // saved by the other node
	mem := newMemState()
	e := newEnv(t, "gpt-x", []upSpec{{id: "o", protocol: "openai-chat", issuer: "i", fake: up}}, func(d *Deps) {
		d.Credentials, d.Lock = st, mem.Lock
	})
	rp := &refreshingProvider{Provider: e.snap.Upstreams["o"].Provider}
	e.snap.Upstreams["o"].Provider = rp
	old := e.snap.Upstreams["o"].Credentials["c1"].Secret
	if err := e.rt.MarkRefreshed(context.Background(), "o", "c1", secretFingerprint(old), time.Minute); err != nil {
		t.Fatal(err)
	}
	if resp, body := e.call("openai-chat", "/v1/chat/completions", chatReq); resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	rp.mu.Lock()
	defer rp.mu.Unlock()
	if rp.calls != 0 || up.calls() != 2 {
		t.Fatalf("refreshes %d (want 0), upstream calls %d (want 2)", rp.calls, up.calls())
	}
}
