package admin

import (
	"encoding/json"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/initxy/plugrelay/internal/features"
	"github.com/initxy/plugrelay/pkg/plugin"
)

// publicPagesModule holds the public pages' settings.
const publicPagesModule = "admin.public-pages"

// publicTTL is how long a public page's answer is reused; the pages are
// open to anyone, so the store is asked at most once per TTL.
const publicTTL = time.Minute

type publicCache struct {
	mu      sync.Mutex
	entries map[string]publicEntry
}

type publicEntry struct {
	body []byte
	at   time.Time
}

func (pc *publicCache) get(key string, now time.Time) ([]byte, bool) {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	e, ok := pc.entries[key]
	return e.body, ok && now.Sub(e.at) < publicTTL
}

func (pc *publicCache) put(key string, body []byte, now time.Time) {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	if pc.entries == nil {
		pc.entries = map[string]publicEntry{}
	}
	pc.entries[key] = publicEntry{body, now}
}

type publicSite struct {
	Name    string `json:"name"`
	LogoURL string `json:"logo_url,omitempty"`
	Accent  string `json:"accent,omitempty"`
}

// publicPages reports which public pages are on: none while the feature
// is off.
func (h *Handler) publicPages(c *call) (models, status bool, err error) {
	on, err := h.featureState(c)
	if err != nil || !on.On(features.PublicPages) {
		return false, false, err
	}
	s := h.d.Runtime.SettingsFor(publicPagesModule, plugin.Caller{}).Get("show_status")
	return true, !s.IsSet() || s.Bool(), nil
}

// servePublicPage answers a public page from the cache, or builds it. Calls
// are counted per address; the cache does not exempt them.
func (h *Handler) servePublicPage(c *call, key string, build func() (any, error)) error {
	n := h.d.Runtime.SettingsFor(publicPagesModule, plugin.Caller{}).Get("per_ip_per_minute").Int()
	if n <= 0 {
		n = 60
	}
	now := h.d.Now()
	ip := "public " + loginKey(h.d.ClientIP(c.r))
	h.publicLimit.setLimit(int(n))
	if wait := h.publicLimit.blocked(ip, now); wait > 0 {
		c.w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
		return newError(http.StatusTooManyRequests, eRateLimited)
	}
	h.publicLimit.failed(ip, now)
	body, ok := h.publicCache.get(key, now)
	if !ok {
		v, err := build()
		if err != nil {
			return err
		}
		if body, err = json.Marshal(v); err != nil {
			return err
		}
		h.publicCache.put(key, body, now)
	}
	c.w.Header().Set("Content-Type", "application/json")
	c.w.WriteHeader(http.StatusOK)
	_, _ = c.w.Write(body)
	return nil
}

func (h *Handler) publicSite(c *call) (publicSite, Site, error) {
	s, err := h.site(c.ctx)
	if err != nil {
		return publicSite{}, nil, err
	}
	return publicSite{Name: s.name(), LogoURL: s.String(settingLogo), Accent: s.String(settingAccent)}, s, nil
}

// publicGroup is the public pages' price group: the models it allows and
// its price multiplier; every model at 1 without one. A group that no
// longer exists is an error, never every model.
func (h *Handler) publicGroup(c *call) (func(string) bool, float64, error) {
	gid := h.d.Runtime.SettingsFor(publicPagesModule, plugin.Caller{}).Get("price_group").String()
	if gid == "" {
		return func(string) bool { return true }, 1, nil
	}
	g, err := h.d.Store.GetGroup(c.ctx, gid)
	if err != nil {
		return nil, 0, storeErr(err, objGroup)
	}
	patterns := groupViewOf(g, 0).Models
	return func(name string) bool { return globMatch(name, patterns) }, g.PriceMultiplier, nil
}

func (h *Handler) getPublicCatalog(c *call) error {
	if on, _, err := h.publicPages(c); err != nil {
		return err
	} else if !on {
		return errNotFound(ePublicPagesOff)
	}
	return h.servePublicPage(c, "catalog", func() (any, error) {
		site, s, err := h.publicSite(c)
		if err != nil {
			return nil, err
		}
		allowed, mult, err := h.publicGroup(c)
		if err != nil {
			return nil, err
		}
		items, err := h.catalogItems(c.ctx, allowed, mult)
		if err != nil {
			return nil, err
		}
		if !s.showUpstreamNames() {
			for i := range items {
				items[i].Info.OwnedBy = ""
			}
		}
		return map[string]any{"site": site, "currency": h.currency(c.ctx), "items": items, "updated_at": h.d.Now().UTC()}, nil
	})
}

func (h *Handler) getPublicStatus(c *call) error {
	if _, on, err := h.publicPages(c); err != nil {
		return err
	} else if !on {
		return errNotFound(ePublicPagesOff)
	}
	return h.servePublicPage(c, "status", func() (any, error) {
		site, _, err := h.publicSite(c)
		if err != nil {
			return nil, err
		}
		allowed, _, err := h.publicGroup(c)
		if err != nil {
			return nil, err
		}
		items, err := h.statusItems(c.ctx, allowed)
		if err != nil {
			return nil, err
		}
		// All models down is an outage; any model not fine is degraded.
		overall := "ok"
		if down := countState(items, "down"); down > 0 && down == len(items) {
			overall = "down"
		} else if down > 0 || countState(items, "degraded") > 0 {
			overall = "degraded"
		}
		return map[string]any{"site": site, "overall": overall, "items": items, "updated_at": h.d.Now().UTC()}, nil
	})
}

func countState(items []statusItem, state string) int {
	n := 0
	for _, it := range items {
		if it.State == state {
			n++
		}
	}
	return n
}
