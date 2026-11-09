package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/initxy/plugrelay/internal/router"
	"github.com/initxy/plugrelay/internal/secret"
	"github.com/initxy/plugrelay/internal/settings"
	"github.com/initxy/plugrelay/internal/store"
	"github.com/initxy/plugrelay/pkg/clientid"
	"github.com/initxy/plugrelay/pkg/plugin"
	"github.com/initxy/plugrelay/plugins/auth/apikey"
)

// keyMeta is what the relay remembers about a key between lookups: the
// key record's own settings (model allowlist, limits) and exemptions.
type keyMeta struct {
	settings map[string]json.RawMessage
	exempt   settings.Exemptions
}

func (m keyMeta) models() []string {
	var ms []string
	_ = json.Unmarshal(m.settings["models"], &ms)
	return ms
}

// keyLookup resolves platform keys for auth.apikey and remembers each
// key's record settings and exemptions for the rest of the request path.
type keyLookup struct {
	st   store.Store
	mu   sync.RWMutex
	meta map[string]keyMeta
}

func newKeyLookup(st store.Store) *keyLookup {
	return &keyLookup{st: st, meta: map[string]keyMeta{}}
}

var _ apikey.KeyLookup = (*keyLookup)(nil)

// LookupKey implements apikey.KeyLookup.
func (k *keyLookup) LookupKey(ctx context.Context, raw string) (*plugin.Caller, apikey.KeyInfo, error) {
	key, err := k.st.KeyByToken(ctx, raw)
	if errors.Is(err, store.ErrNotFound) {
		return nil, apikey.KeyInfo{}, nil
	}
	if err != nil {
		return nil, apikey.KeyInfo{}, err
	}
	info := apikey.KeyInfo{Disabled: key.Disabled, Expires: key.ExpiresAt}
	if key.UserID != "" && !key.Disabled {
		// A disabled user's keys stop working with the account.
		u, err := k.st.GetUser(ctx, key.UserID)
		switch {
		case errors.Is(err, store.ErrNotFound):
			info.Disabled = true
		case err != nil:
			return nil, apikey.KeyInfo{}, err
		default:
			info.Disabled = u.Disabled
		}
	}
	_ = json.Unmarshal(key.Settings["ip_allowlist"], &info.AllowedIPs)
	info.AllowClient = allowClients(key.Settings["clients"])
	k.remember(key)
	if !info.Disabled && key.Usable(time.Now()) {
		k.st.TouchKey(key.ID, time.Now())
	}
	c := key.Caller()
	return &c, info, nil
}

// allowClients is the check of a key's "clients" setting: the client id
// the request log gives the request (clientid.Detect) must be listed. A
// listed id that clientid never detects matches nothing; it never lifts
// the restriction.
func allowClients(raw json.RawMessage) func(http.Header) bool {
	var ids []string
	if err := json.Unmarshal(raw, &ids); err != nil || len(ids) == 0 {
		return nil
	}
	return func(h http.Header) bool { return slices.Contains(ids, clientid.Detect(h).ID) }
}

func (k *keyLookup) remember(key *store.Key) keyMeta {
	m := keyMeta{settings: key.Settings, exempt: key.Exceptions}
	k.mu.Lock()
	k.meta[key.ID] = m
	k.mu.Unlock()
	return m
}

// metaOf is what the key's record says. A key not looked up since start
// (settings resolved outside a request) is read from the store.
func (k *keyLookup) metaOf(keyID string) keyMeta {
	k.mu.RLock()
	m, ok := k.meta[keyID]
	k.mu.RUnlock()
	if ok {
		return m
	}
	key, err := k.st.GetKey(context.Background(), keyID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		key = &store.Key{ID: keyID}
	case err != nil:
		return keyMeta{}
	}
	return k.remember(key)
}

// credentialStore persists router credential state and refreshed secrets;
// box opens sealed secrets for LoadCredentialSecret.
type credentialStore struct {
	st  store.Store
	box *secret.Box
}

func (c credentialStore) SaveCredentialState(ctx context.Context, ev router.CredEvent) error {
	status := store.CredentialStatus(ev.Status)
	switch ev.Status {
	case router.StatusQuarantined, router.StatusManual, router.StatusActive:
	default:
		// Cooldowns and refreshes live in the state backend only.
		return nil
	}
	if ev.Model != "" {
		return nil
	}
	if ev.Status == router.StatusActive {
		cur, err := c.st.GetCredential(ctx, ev.CredentialID)
		if err != nil || cur.Status == store.CredDisabled || cur.Status == store.CredActive {
			return nil
		}
	}
	err := c.st.SetCredentialStatus(ctx, ev.CredentialID, status, ev.Reason, ev.Until)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	return err
}

func (c credentialStore) SaveCredentialSecret(ctx context.Context, credID, secret string, expires time.Time) error {
	return c.st.SetCredentialSecret(ctx, credID, secret, expires)
}

func (c credentialStore) LoadCredentialSecret(ctx context.Context, credID string) (string, error) {
	cur, err := c.st.GetCredential(ctx, credID)
	if err != nil {
		return "", err
	}
	pc, err := cur.Open(c.box)
	return pc.Secret, err
}

// recordWriter feeds sink.requestlog.
type recordWriter struct{ st store.Store }

func (w recordWriter) InsertRecord(ctx context.Context, r *plugin.Record) error {
	return w.st.InsertRecords(ctx, []*plugin.Record{r})
}

func (w recordWriter) Purge(ctx context.Context, before time.Time) error {
	_, err := w.st.PruneRecords(ctx, before)
	return err
}
