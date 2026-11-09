package pluginsdk

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/plugin"
)

type client struct {
	t   *testing.T
	w   io.Writer
	r   *bufio.Reader
	id  int
	end chan error
}

func serve(t *testing.T, p *Plugin) *client {
	hostR, pluginW := io.Pipe()
	pluginR, hostW := io.Pipe()
	c := &client{t: t, w: hostW, r: bufio.NewReader(hostR), end: make(chan error, 1)}
	go func() { c.end <- p.ServeStreams(pluginR, pluginW); pluginW.Close() }()
	t.Cleanup(func() {
		c.send(map[string]any{"method": "exit"})
		select {
		case <-c.end:
		case <-time.After(2 * time.Second):
			t.Error("Serve did not return after exit")
		}
	})
	return c
}

func (c *client) send(m map[string]any) {
	m["jsonrpc"] = "2.0"
	body, _ := json.Marshal(m)
	fmt.Fprintf(c.w, "Content-Length: %d\r\n\r\n%s", len(body), body)
}

func (c *client) recv() map[string]any {
	body, err := readFrame(c.r)
	if err != nil {
		c.t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		c.t.Fatal(err)
	}
	return m
}

func (c *client) call(method string, params any, onMsg func(map[string]any)) map[string]any {
	c.id++
	id := float64(c.id)
	c.send(map[string]any{"id": c.id, "method": method, "params": params})
	for {
		m := c.recv()
		if m["id"] == id && m["method"] == nil {
			return m
		}
		if onMsg != nil {
			onMsg(m)
		}
	}
}

func code(m map[string]any) int {
	e, _ := m["error"].(map[string]any)
	if e == nil {
		return 0
	}
	return int(e["code"].(float64))
}

func TestLifecycleAndDispatch(t *testing.T) {
	p := New("demo", "1.0.0")
	p.OnRequest(func(ctx context.Context, c *Call) (*Reply, error) {
		if c.Model == "kv" {
			v, _, err := p.Host().KVGet(ctx, "k")
			return Deny(ir.ErrInvalidRequest, v), err
		}
		return nil, c.SetState(map[string]int{"n": 1})
	})
	p.Authenticate(func(ctx context.Context, r *HTTPRequest) (*plugin.Caller, error) {
		return nil, Reject(ir.ErrAuthentication, "no")
	})
	p.RunJob(func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() })
	c := serve(t, p)

	if code(c.call("middleware.onRequest", map[string]any{}, nil)) != CodeNotInitialized {
		t.Fatal("calls before initialize must fail")
	}
	if code(c.call("initialize", map[string]any{"protocol_version": 2}, nil)) != CodeInvalidRequest {
		t.Fatal("version mismatch accepted")
	}
	res := c.call("initialize", map[string]any{"protocol_version": 1}, nil)["result"].(map[string]any)
	if res["manifest"].(map[string]any)["id"] != "demo" || len(res["capabilities"].(map[string]any)["points"].([]any)) != 3 {
		t.Fatalf("initialize %v", res)
	}
	if code(c.call("sink.consume", map[string]any{}, nil)) != CodeMethodNotFound {
		t.Fatal("unknown method")
	}
	r := c.call("middleware.onRequest", map[string]any{"call": map[string]any{"id": "r"}}, nil)["result"].(map[string]any)
	if r["reply"] != nil || r["state"].(map[string]any)["n"] != 1.0 {
		t.Fatalf("onRequest %v", r)
	}
	r = c.call("middleware.onRequest", map[string]any{"call": map[string]any{"model": "kv"}}, func(m map[string]any) {
		if m["method"] != "host.kv.get" {
			t.Fatalf("unexpected %v", m)
		}
		c.send(map[string]any{"id": m["id"], "result": map[string]any{"found": true, "value": "v1"}})
	})["result"].(map[string]any)
	if r["reply"].(map[string]any)["error"].(map[string]any)["message"] != "v1" {
		t.Fatalf("host call %v", r)
	}
	m := c.call("auth.authenticate", map[string]any{"request": map[string]any{}}, nil)
	if code(m) != CodeRejected || m["error"].(map[string]any)["data"].(map[string]any)["kind"] != "authentication" {
		t.Fatalf("reject %v", m)
	}

	c.id++
	c.send(map[string]any{"id": c.id, "method": "job.run", "params": map[string]any{}})
	c.send(map[string]any{"method": "$/cancelRequest", "params": map[string]any{"id": c.id}})
	if m := c.recv(); code(m) != CodeRequestCancelled {
		t.Fatalf("cancel %v", m)
	}
}

func TestReadFrame(t *testing.T) {
	r := bufio.NewReader(bytes.NewBufferString("content-length: 2\r\nContent-Type: x\r\n\r\n{}"))
	if b, err := readFrame(r); err != nil || string(b) != "{}" {
		t.Fatal(b, err)
	}
	if _, err := readFrame(r); err != io.EOF {
		t.Fatal(err)
	}
	if _, err := readFrame(bufio.NewReader(bytes.NewBufferString("\r\n{}"))); err == nil {
		t.Fatal("frame without length accepted")
	}
}
