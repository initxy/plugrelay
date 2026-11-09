package server

import (
	"bytes"
	"context"
	"io"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/initxy/plugrelay/pkg/captcha"
)

// consoleCSP is the Content-Security-Policy of console pages. The console
// frames nothing but the widgets of the captcha providers in use (login,
// sign-up), and only while one is.
func consoleCSP(providers []string) string {
	var src string
	for _, p := range providers {
		if s := captchaSources[p]; s != "" && !strings.Contains(src, s) {
			src += " " + s
		}
	}
	frame := "'none'"
	if src != "" {
		frame = src[1:]
	}
	return "default-src 'self'; script-src 'self'" + src + "; style-src 'self' 'unsafe-inline'" + src + "; " +
		"img-src 'self' data: https:; font-src 'self' data:; connect-src 'self'" + src + "; frame-src " + frame + "; " +
		"frame-ancestors 'none'; base-uri 'self'; form-action 'self'; object-src 'none'"
}

// captchaSources are the origins each captcha provider's widget loads from.
var captchaSources = map[string]string{
	captcha.Turnstile: "https://challenges.cloudflare.com",
	captcha.HCaptcha:  "https://hcaptcha.com https://*.hcaptcha.com",
}

// securityHeaders sets the headers every console response carries;
// providers are the captcha providers in use.
func securityHeaders(h http.Header, providers []string) {
	h.Set("Content-Security-Policy", consoleCSP(providers))
	h.Set("X-Frame-Options", "DENY")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "same-origin")
	h.Set("Cross-Origin-Opener-Policy", "same-origin")
}

// serveConsole serves the single-page console: real files as they are,
// hashed assets with a year-long cache, and index.html for every other
// path so client-side routes survive a reload.
func serveConsole(w http.ResponseWriter, r *http.Request, fsys fs.FS, captchas func(context.Context) []string) {
	var providers []string
	if captchas != nil {
		providers = captchas(r.Context())
	}
	securityHeaders(w.Header(), providers)
	name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
	if name == "" {
		name = "index.html"
	}
	f, err := fsys.Open(name)
	if err == nil {
		st, serr := f.Stat()
		if serr == nil && !st.IsDir() {
			defer f.Close()
			if strings.HasPrefix(name, "assets/") {
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			} else {
				w.Header().Set("Cache-Control", "no-cache")
			}
			serveFile(w, r, name, st.ModTime(), f)
			return
		}
		f.Close()
	}
	if strings.HasPrefix(name, "assets/") || path.Ext(name) != "" && name != "index.html" {
		http.NotFound(w, r)
		return
	}
	idx, err := fs.ReadFile(fsys, "index.html")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-cache")
	http.ServeContent(w, r, "index.html", time.Time{}, bytes.NewReader(idx))
}

func serveFile(w http.ResponseWriter, r *http.Request, name string, mod time.Time, f fs.File) {
	if rs, ok := f.(io.ReadSeeker); ok {
		http.ServeContent(w, r, name, mod, rs)
		return
	}
	b, err := io.ReadAll(f)
	if err != nil {
		http.Error(w, "read error", http.StatusInternalServerError)
		return
	}
	http.ServeContent(w, r, name, mod, bytes.NewReader(b))
}
