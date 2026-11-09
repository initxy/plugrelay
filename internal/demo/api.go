package demo

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"

	"github.com/initxy/plugrelay/internal/admin"
)

// api calls the admin API the way the console does: a session cookie plus
// the double-submit CSRF header.
type api struct {
	base string
	hc   *http.Client
}

func newAPI(base string) *api {
	jar, _ := cookiejar.New(nil)
	return &api{base: strings.TrimRight(base, "/"), hc: &http.Client{Jar: jar}}
}

// do sends in as JSON to path (relative to the server root) and decodes
// the answer into out; any status of 300 or more is an error.
func (c *api) do(method, path string, in, out any) error {
	var rd io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.base+path, rd)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if u, err := url.Parse(c.base); err == nil {
		for _, ck := range c.hc.Jar.Cookies(u) {
			if ck.Name == admin.CSRFCookie {
				req.Header.Set(admin.CSRFHeader, ck.Value)
			}
		}
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s: %d %s", method, path, resp.StatusCode, bytes.TrimSpace(b))
	}
	if out != nil {
		if err := json.Unmarshal(b, out); err != nil {
			return fmt.Errorf("%s %s: %w", method, path, err)
		}
	}
	return nil
}

func (c *api) v1(method, path string, in, out any) error {
	return c.do(method, admin.Prefix+path, in, out)
}

func (c *api) plugin(id, method, path string, in, out any) error {
	return c.do(method, "/api/plugins/"+id+path, in, out)
}

func (c *api) login(user, password string) error {
	return c.v1("POST", "/auth/login", map[string]string{"username": user, "password": password}, nil)
}
