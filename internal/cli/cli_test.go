package cli

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/initxy/plugrelay/internal/store"
)

var testNow = time.Date(2026, 11, 5, 10, 30, 0, 0, time.UTC)

type testEnv struct {
	*Env
	vars   map[string]string
	stdin  *bytes.Buffer
	stdout *bytes.Buffer
	stderr *bytes.Buffer
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	te := &testEnv{vars: map[string]string{}, stdin: &bytes.Buffer{}, stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}}
	te.Env = &Env{
		Stdin:  te.stdin,
		Stdout: te.stdout,
		Stderr: te.stderr,
		LookupEnv: func(k string) (string, bool) {
			v, ok := te.vars[k]
			return v, ok
		},
		Now:         func() time.Time { return testNow },
		Interactive: func() bool { return false },
		HomeDir: func() (string, error) {
			t.Error("test reached the real home directory lookup")
			return "", errors.New("no home in tests")
		},
	}
	return te
}

// run executes args and returns the exit code; output stays in the
// buffers, reset before each run.
func (te *testEnv) run(args ...string) int {
	te.stdout.Reset()
	te.stderr.Reset()
	return Run(context.Background(), args, te.Env)
}

func TestExitCodes(t *testing.T) {
	cases := []struct {
		args   []string
		code   int
		stdout string
		stderr string
	}{
		{nil, ExitUsage, "", "Commands:"},
		{[]string{"--help"}, ExitOK, "Commands:", ""},
		{[]string{"bogus"}, ExitUsage, "", `unknown command "bogus"`},
		{[]string{"--nope"}, ExitUsage, "", "flag provided but not defined"},
		{[]string{"key"}, ExitUsage, "", "missing subcommand"},
		{[]string{"key", "bogus"}, ExitUsage, "", `key: unknown command "bogus"`},
		{[]string{"key", "help"}, ExitOK, "create", ""},
		{[]string{"key", "create"}, ExitUsage, "", "--name is required"},
		{[]string{"key", "create", "--name", "x", "--expires", "soon"}, ExitUsage, "", "--expires"},
		{[]string{"key", "create", "--bogus"}, ExitUsage, "", "plugrelay key create --help"},
		{[]string{"key", "create", "--help"}, ExitOK, "Usage: plugrelay key create", ""},
		{[]string{"help", "key", "create"}, ExitOK, "Usage: plugrelay key create", ""},
		{[]string{"model", "add", "--name", "m"}, ExitUsage, "", "--route"},
		{[]string{"model", "add", "--name", "m", "--route", ":x"}, ExitUsage, "", "want upstream"},
		{[]string{"upstream", "add"}, ExitUsage, "", "--manifest is required"},
		{[]string{"upstream", "add", "--manifest", "x", "--var", "novalue"}, ExitUsage, "", "want key=value"},
		{[]string{"apply"}, ExitUsage, "", "-f is required"},
		{[]string{"manifest", "validate"}, ExitUsage, "", "at least one"},
		{[]string{"setup", "codex", "--url", "http://x"}, ExitUsage, "", "--key is required"},
		{[]string{"setup", "codex", "--key", "k", "--url", "ftp://x"}, ExitUsage, "", "http://"},
		{[]string{"serve"}, ExitError, "", "serve not wired"},
		{[]string{"version"}, ExitOK, "plugrelay dev", ""},
	}
	for _, tc := range cases {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			te := newTestEnv(t)
			te.vars["PLUGRELAY_CONFIG"] = t.TempDir() + "/relay.yaml"
			if got := te.run(tc.args...); got != tc.code {
				t.Fatalf("exit %d, want %d\nstdout: %s\nstderr: %s", got, tc.code, te.stdout, te.stderr)
			}
			if !strings.Contains(te.stdout.String(), tc.stdout) {
				t.Errorf("stdout %q lacks %q", te.stdout, tc.stdout)
			}
			if !strings.Contains(te.stderr.String(), tc.stderr) {
				t.Errorf("stderr %q lacks %q", te.stderr, tc.stderr)
			}
		})
	}
}

// Every leaf documents itself: usage line, flags and an example.
func TestEveryCommandHasHelp(t *testing.T) {
	var walk func(path []string, c *command)
	walk = func(path []string, c *command) {
		if c.run == nil {
			for _, s := range c.subs {
				walk(append(append([]string{}, path...), s.name), s)
			}
			return
		}
		t.Run(strings.Join(path, " "), func(t *testing.T) {
			te := newTestEnv(t)
			if code := te.run(append(path, "--help")...); code != ExitOK {
				t.Fatalf("exit %d: %s", code, te.stderr)
			}
			out := te.stdout.String()
			for _, want := range []string{"Usage: plugrelay " + strings.Join(path, " "), "Flags:", "-config", "Example:"} {
				if !strings.Contains(out, want) {
					t.Errorf("help lacks %q:\n%s", want, out)
				}
			}
		})
	}
	walk(nil, rootCommand())
}

func TestParseRoute(t *testing.T) {
	cases := []struct {
		in      string
		want    store.Route
		wantErr bool
	}{
		{in: "ds:deepseek-chat", want: store.Route{UpstreamID: "ds", UpstreamModel: "deepseek-chat"}},
		{in: "ds:deepseek-chat:10", want: store.Route{UpstreamID: "ds", UpstreamModel: "deepseek-chat", Priority: 10}},
		{in: "ds:deepseek-chat:10:3", want: store.Route{UpstreamID: "ds", UpstreamModel: "deepseek-chat", Priority: 10, Weight: 3}},
		{in: "ds:m:-1", want: store.Route{UpstreamID: "ds", UpstreamModel: "m", Priority: -1}},
		{in: "ollama:qwen3:8b", want: store.Route{UpstreamID: "ollama", UpstreamModel: "qwen3:8b"}},
		{in: "ollama:qwen3:8b:5:2", want: store.Route{UpstreamID: "ollama", UpstreamModel: "qwen3:8b", Priority: 5, Weight: 2}},
		{in: "ds", want: store.Route{UpstreamID: "ds"}},
		{in: "ds:7", want: store.Route{UpstreamID: "ds", UpstreamModel: "7"}},
		{in: "ds::5", want: store.Route{UpstreamID: "ds", Priority: 5}},
		{in: ":m", wantErr: true},
		{in: "ds:m:1:-2", wantErr: true},
	}
	for _, tc := range cases {
		got, err := parseRoute(tc.in)
		if (err != nil) != tc.wantErr {
			t.Errorf("parseRoute(%q) error = %v, wantErr %v", tc.in, err, tc.wantErr)
			continue
		}
		if !tc.wantErr && !reflect.DeepEqual(got, tc.want) {
			t.Errorf("parseRoute(%q) = %+v, want %+v", tc.in, got, tc.want)
		}
	}
}

func TestParseExpires(t *testing.T) {
	cases := []struct {
		in      string
		want    time.Time
		wantErr bool
	}{
		{in: "", want: time.Time{}},
		{in: "30d", want: testNow.Add(30 * 24 * time.Hour)},
		{in: "2w", want: testNow.Add(14 * 24 * time.Hour)},
		{in: "12h", want: testNow.Add(12 * time.Hour)},
		{in: "2027-01-31", want: time.Date(2027, 1, 31, 0, 0, 0, 0, time.UTC)},
		{in: "2027-01-31T08:00:00+08:00", want: time.Date(2027, 1, 31, 0, 0, 0, 0, time.UTC)},
		{in: "0d", wantErr: true},
		{in: "-1h", wantErr: true},
		{in: "xd", wantErr: true},
		{in: "soon", wantErr: true},
	}
	for _, tc := range cases {
		got, err := parseExpires(tc.in, testNow)
		if (err != nil) != tc.wantErr {
			t.Errorf("parseExpires(%q) error = %v, wantErr %v", tc.in, err, tc.wantErr)
			continue
		}
		if !got.Equal(tc.want) {
			t.Errorf("parseExpires(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestParseVars(t *testing.T) {
	cases := []struct {
		in      []string
		want    map[string]string
		wantErr bool
	}{
		{in: nil, want: map[string]string{}},
		{in: []string{"resource=acme", "api_version=2025-04-01"}, want: map[string]string{"resource": "acme", "api_version": "2025-04-01"}},
		{in: []string{"url=http://h/?a=b"}, want: map[string]string{"url": "http://h/?a=b"}},
		{in: []string{"empty="}, want: map[string]string{"empty": ""}},
		{in: []string{"novalue"}, wantErr: true},
		{in: []string{"=v"}, wantErr: true},
		{in: []string{"a=1", "a=2"}, wantErr: true},
	}
	for _, tc := range cases {
		got, err := parseVars(tc.in)
		if (err != nil) != tc.wantErr {
			t.Errorf("parseVars(%q) error = %v, wantErr %v", tc.in, err, tc.wantErr)
			continue
		}
		if tc.wantErr {
			continue
		}
		if len(got) != len(tc.want) {
			t.Errorf("parseVars(%q) = %v, want %v", tc.in, got, tc.want)
		}
		for k, v := range tc.want {
			if got[k] != v {
				t.Errorf("parseVars(%q)[%s] = %q, want %q", tc.in, k, got[k], v)
			}
		}
	}
}

func TestIsLoopback(t *testing.T) {
	for addr, want := range map[string]bool{
		"127.0.0.1:8787": true, "localhost:1": true, "[::1]:80": true,
		"0.0.0.0:8787": false, ":8787": false, "[::]:80": false, "10.0.0.1:80": false, "bad": false,
	} {
		if got := isLoopback(addr); got != want {
			t.Errorf("isLoopback(%q) = %v, want %v", addr, got, want)
		}
	}
}
