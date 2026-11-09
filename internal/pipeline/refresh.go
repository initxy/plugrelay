package pipeline

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"time"

	"github.com/initxy/plugrelay/pkg/plugin"
)

// refreshTimeout bounds one credential refresh, lock included.
const refreshTimeout = 30 * time.Second

// secretSaveBackoff is the wait before each retry of saving a refreshed
// secret while the request waits; afterwards PersistPending keeps trying.
var secretSaveBackoff = []time.Duration{200 * time.Millisecond, time.Second}

// attentionUnsaved is the operator-facing reason on a credential whose
// refreshed secret lives only in this node's memory.
const attentionUnsaved = "refreshed secret could not be saved; it is lost if this node restarts (save is retried every minute)"

type refreshedSecret struct {
	old, new string // the snapshot's secret when refreshed, and its replacement
}

// unsavedSecret is a refreshed secret the store did not take yet.
type unsavedSecret struct {
	upstreamID, credID string
	old, new           string
	expires            time.Time
}

// CurrentSecret returns c with a secret refreshed since the snapshot was
// built. Once the snapshot holds another secret the override is dropped.
func (p *Pipeline) CurrentSecret(upstreamID string, c *plugin.Credential) *plugin.Credential {
	key := upstreamID + "|" + c.ID
	v, ok := p.refreshed.Load(key)
	if !ok {
		return c
	}
	r := v.(refreshedSecret)
	switch c.Secret {
	case r.old: // a snapshot from before the refresh
	case r.new: // the snapshot caught up; older snapshots still need it
		return c
	default: // replaced since (an admin edit): the override is stale
		p.refreshed.CompareAndDelete(key, v)
		return c
	}
	cp := *c
	cp.Secret = r.new
	return &cp
}

// refresh renews the credential of a refresh verdict through the provider's
// Refresher, under a cluster-wide lock, persists the new secret and returns
// the credential to rotation. It reports whether the attempt may be resent
// with it; a credential is refreshed at most once per request. Without a
// Refresher, a lock or on failure, the credential sits out as the verdict
// asked.
func (x *exchange) refresh(a *attempt) bool {
	rf, ok := a.up.Provider.(plugin.Refresher)
	if !ok || !supports(a.up.Provider, "provider.refresh") || x.p.d.Lock == nil {
		return false
	}
	upID, credID := a.cand.UpstreamID, a.cand.CredentialID
	key := upID + "|" + credID
	if x.refreshedCreds[key] {
		return false
	}
	if x.refreshedCreds == nil {
		x.refreshedCreds = map[string]bool{}
	}
	x.refreshedCreds[key] = true
	stored := x.snap.Upstreams[upID].Credentials[credID]
	if stored == nil {
		return false
	}
	log := x.p.d.Logger.With("upstream", upID, "credential", credID)
	ctx, cancel := context.WithTimeout(context.WithoutCancel(x.ctx), refreshTimeout)
	defer cancel()
	unlock, err := x.waitLock(ctx, "credential-refresh:"+key)
	if err != nil {
		log.Warn("credential refresh lock unavailable; it stays out of rotation", "err", err)
		return false
	}
	defer unlock()
	// Two requests can fail with the same old secret; the one that waited
	// for the lock must not refresh again (a rotating refresh token is
	// already spent, and some servers revoke the whole family on reuse).
	if cur := x.p.CurrentSecret(upID, stored); cur.Secret != a.cred.Secret {
		log.Debug("credential already refreshed by another request; retrying with it")
		return true
	}
	// Another node may have refreshed it: its in-memory override and this
	// node's snapshot don't show that, the shared mark does.
	oldFP := secretFingerprint(a.cred.Secret)
	if x.p.d.Router.Refreshed(ctx, upID, credID, oldFP) {
		// The old refresh token is known to be spent: never use it again,
		// even when the new secret cannot be read here (the other node may
		// not have saved it yet). The credential then just sits out.
		if x.p.d.Credentials == nil {
			return false
		}
		cur, err := x.p.d.Credentials.LoadCredentialSecret(ctx, credID)
		if err != nil || cur == "" || cur == a.cred.Secret {
			log.Warn("credential refreshed on another node but its new secret is not readable yet; it stays out of rotation", "err", err)
			return false
		}
		x.p.refreshed.Store(key, refreshedSecret{old: stored.Secret, new: cur})
		log.Debug("credential refreshed on another node; retrying with the stored secret")
		return true
	}
	secret, expires, err := rf.Refresh(ctx, a.up.Config, a.cred)
	if err != nil || secret == "" {
		log.Warn("credential refresh failed; it stays out of rotation", "err", err)
		return false
	}
	// The new secret is valid whether or not it is saved, and a rotating
	// refresh token already spent the old one: use it, and if the store
	// keeps refusing it, say so loudly and keep retrying in the background.
	x.p.refreshed.Store(key, refreshedSecret{old: stored.Secret, new: secret})
	x.p.saveSecret(ctx, unsavedSecret{upstreamID: upID, credID: credID, old: stored.Secret, new: secret, expires: expires})
	if err := x.p.d.Router.MarkRefreshed(ctx, upID, credID, oldFP, refreshedMarkTTL); err != nil {
		log.Warn("record refresh for other nodes", "err", err)
	}
	if err := x.p.d.Router.RefreshDone(ctx, upID, credID); err != nil {
		log.Warn("clear refreshing state", "err", err)
	}
	log.Info("credential refreshed", "expires", expires)
	return true
}

// refreshedMarkTTL is how long other nodes are told a secret was replaced;
// it outlasts the snapshot reload that brings them the new secret.
const refreshedMarkTTL = 10 * time.Minute

// secretFingerprint identifies a secret in shared state without storing it.
func secretFingerprint(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:12])
}

// refreshLockPoll is how often a request waits on a refresh another request
// or node is running; the lock has no blocking acquire across backends.
const refreshLockPoll = 50 * time.Millisecond

// waitLock takes the cluster lock for key, waiting while another holder
// refreshes, until ctx ends.
func (x *exchange) waitLock(ctx context.Context, key string) (func(), error) {
	for {
		unlock, ok, err := x.p.d.Lock(ctx, key, refreshTimeout)
		if err != nil {
			return nil, err
		}
		if ok {
			return unlock, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(refreshLockPoll):
		}
	}
}

// saveSecret stores a refreshed secret, retrying a few times. On failure
// the credential is flagged for an operator (shared state, every node's
// console shows it) and the secret queued for PersistPending.
func (p *Pipeline) saveSecret(ctx context.Context, u unsavedSecret) {
	if p.d.Credentials == nil {
		return
	}
	key := u.upstreamID + "|" + u.credID
	var err error
	for i := 0; ; i++ {
		if err = p.d.Credentials.SaveCredentialSecret(ctx, u.credID, u.new, u.expires); err == nil {
			p.unsaved.Delete(key)
			return
		}
		if i == len(secretSaveBackoff) || ctx.Err() != nil {
			break
		}
		select {
		case <-ctx.Done():
		case <-time.After(secretSaveBackoff[i]):
		}
	}
	p.secretSaveFailures.Add(1)
	p.unsaved.Store(key, u)
	p.d.Logger.Error("refreshed credential secret not saved; serving with it from memory, lost on restart",
		"upstream", u.upstreamID, "credential", u.credID, "err", err)
	if merr := p.d.Router.MarkAttention(ctx, u.upstreamID, u.credID, attentionUnsaved); merr != nil {
		p.d.Logger.Error("flag credential for attention", "upstream", u.upstreamID, "credential", u.credID, "err", merr)
	}
}

// PersistPending retries saving refreshed secrets the store refused and
// clears the credential's attention flag once one is saved. A secret the
// snapshot no longer starts from (an operator replaced it, or a newer
// refresh superseded it) is dropped. It returns how many are still
// unsaved. Call it periodically.
func (p *Pipeline) PersistPending(ctx context.Context) int {
	if p.d.Credentials == nil {
		return 0
	}
	snap := p.snapshot()
	left := 0
	p.unsaved.Range(func(k, v any) bool {
		u := v.(unsavedSecret)
		cur, _ := p.refreshed.Load(k)
		var stored *plugin.Credential
		if snap != nil && snap.Upstreams[u.upstreamID] != nil {
			stored = snap.Upstreams[u.upstreamID].Credentials[u.credID]
		}
		if stored == nil || stored.Secret != u.old || cur != (refreshedSecret{old: u.old, new: u.new}) {
			p.unsaved.CompareAndDelete(k, v)
			if stored != nil && stored.Secret != u.old {
				// The store holds another secret now (an operator's, or this
				// one saved by another path): nothing is at risk any more.
				_ = p.d.Router.ClearAttention(ctx, u.upstreamID, u.credID)
			}
			return true
		}
		if err := p.d.Credentials.SaveCredentialSecret(ctx, u.credID, u.new, u.expires); err != nil {
			left++
			p.d.Logger.Error("refreshed credential secret still not saved", "upstream", u.upstreamID, "credential", u.credID, "err", err)
			return true
		}
		p.unsaved.CompareAndDelete(k, v)
		if err := p.d.Router.ClearAttention(ctx, u.upstreamID, u.credID); err != nil {
			p.d.Logger.Warn("clear credential attention", "upstream", u.upstreamID, "credential", u.credID, "err", err)
		}
		p.d.Logger.Info("refreshed credential secret saved on retry", "upstream", u.upstreamID, "credential", u.credID)
		return true
	})
	return left
}

// SecretSaveFailures counts refreshed secrets the store refused after the
// in-request retries, since start.
func (p *Pipeline) SecretSaveFailures() int64 { return p.secretSaveFailures.Load() }
