package admin

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/initxy/plugrelay/internal/store"
)

// pluginAPI serves /api/plugins/{id}/{path...} from the plugin's admin
// routes.
func (h *Handler) pluginAPI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	id := r.PathValue("id")
	c := &call{h: h, w: w, r: r, ctx: r.Context()}
	if h.servePublic(w, r, id) {
		return
	}
	if needs, err := h.NeedsSetup(c.ctx); err != nil || needs {
		writeError(w, r, errForbidden(eSetupRequired))
		return
	}
	who, err := h.identify(c)
	if err != nil {
		writeError(w, r, err)
		return
	}
	if who == nil {
		writeError(w, r, errUnauthorized(eNotLoggedIn))
		return
	}
	if who, err = h.pluginCaller(c, who); err != nil {
		writeError(w, r, err)
		return
	}
	c.who = who
	e, ok := h.plugin(id)
	if !ok || !e.Enabled {
		writeError(w, r, errNotFound(eFeatureNotFound.with("object", objPlugin)))
		return
	}
	sub := "/" + r.PathValue("path")
	for _, rt := range e.Routes {
		if !strings.EqualFold(rt.Method, r.Method) || !routeMatch(rt.Path, sub) {
			continue
		}
		if !roleAllows(rt.Role, who.role) {
			role := roleMember // roleAllows refuses only "admin" and "member"
			if rt.Role == "admin" {
				role = roleAdmin
			}
			writeError(w, r, errForbidden(eRoleRequired.with("role", role)))
			return
		}
		if rt.Reauth {
			if err := h.requireAdminReauth(c); err != nil {
				writeError(w, r, err)
				return
			}
		}
		if isUnsafe(r.Method) {
			c.audit("plugin.call", "plugin", id, r.Method+" "+sub, nil)
		}
		r.Header.Del("Cookie")
		r.Header.Set("X-Plugrelay-User", who.userID())
		r.Header.Set("X-Plugrelay-Role", string(who.role))
		rt.Handler(w, r)
		return
	}
	writeError(w, r, errNotFound(eNoSuchEndpoint))
}

// pluginCaller holds a plugin route caller to what authorize holds a
// management API caller to: an admin past the two-step enrollment
// deadline calls as a member.
func (h *Handler) pluginCaller(c *call, who *identity) (*identity, error) {
	if who.role != store.RoleAdmin || who.user == nil || who.auth != authSession {
		return who, nil
	}
	err := h.mfaEnrollmentDue(c.ctx, who.user)
	var ae *apiError
	if err == nil || !errors.As(err, &ae) || ae.Code != codeMFAEnrollment {
		return who, err
	}
	demoted := *who
	demoted.role = store.RoleMember
	return &demoted, nil
}

// servePublic serves a compiled-in plugin's public route (a payment
// webhook, sign-up) without a login, and reports whether it did. The
// request reaches the plugin without cookies or identity headers.
func (h *Handler) servePublic(w http.ResponseWriter, r *http.Request, id string) bool {
	e, ok := h.plugin(id)
	if !ok || !e.Enabled || e.Source != SourceBuiltin {
		return false
	}
	sub := "/" + r.PathValue("path")
	for _, rt := range e.Routes {
		if rt.Public && strings.EqualFold(rt.Method, r.Method) && routeMatch(rt.Path, sub) {
			// Counted per route and address: a sign-up flood does not hold
			// back a provider's payment notices.
			key, now := id+" "+strings.ToUpper(rt.Method)+" "+rt.Path+" "+loginKey(h.d.ClientIP(r)), h.d.Now()
			if wait := h.publicCalls.blocked(key, now); wait > 0 {
				w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
				writeError(w, r, newError(http.StatusTooManyRequests, eRateLimited))
				return true
			}
			h.publicCalls.failed(key, now)
			r.Header.Del("Cookie")
			r.Header.Del("Authorization")
			r.Header.Del("X-Plugrelay-User")
			r.Header.Del("X-Plugrelay-Role")
			rt.Handler(w, r)
			return true
		}
	}
	return false
}

func roleAllows(need string, have store.Role) bool {
	switch need {
	case "admin":
		return have == store.RoleAdmin
	case "member":
		return have == store.RoleAdmin || have == store.RoleMember
	}
	return true
}

// routeMatch matches a route path whose "{x}" segments match any segment.
func routeMatch(pattern, p string) bool {
	ps, ss := strings.Split(strings.Trim(pattern, "/"), "/"), strings.Split(strings.Trim(p, "/"), "/")
	if len(ps) != len(ss) {
		return false
	}
	for i := range ps {
		if strings.HasPrefix(ps[i], "{") && strings.HasSuffix(ps[i], "}") && ss[i] != "" {
			continue
		}
		if ps[i] != ss[i] {
			return false
		}
	}
	return true
}
