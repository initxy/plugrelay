package extplugin

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/initxy/plugrelay/pkg/plugin"
)

// State is a plugin process's lifecycle state.
type State string

const (
	StateStopped  State = "stopped"
	StateStarting State = "starting"
	StateRunning  State = "running"
	// StateBackoff: the process exited or failed its handshake and waits
	// before the next restart. Calls fail with ErrUnavailable.
	StateBackoff State = "backoff"
)

// stableAfter is how long a process must stay up for its restart backoff
// to reset.
const stableAfter = 30 * time.Second

var errCallTimeout = errors.New("call timeout")

// instance supervises one plugin process.
type instance struct {
	man  *Manifest
	opts *Options
	host plugin.Host
	log  *slog.Logger
	sem  chan struct{}

	mu       sync.Mutex
	state    State
	conn     *conn
	pid      int
	restarts int
	attempts int
	lastErr  error
	since    time.Time
	changed  chan struct{} // closed and replaced on every state change
	stop     chan struct{}
	done     chan struct{}
}

func newInstance(m *Manifest, opts *Options) *instance {
	in := &instance{
		man: m, opts: opts,
		sem:     make(chan struct{}, m.MaxConcurrency),
		state:   StateStopped,
		changed: make(chan struct{}),
		log:     opts.Logger.With("plugin", m.ID),
	}
	if opts.Host != nil {
		in.host = opts.Host(m.PluginID(m.Kinds[0]))
	}
	return in
}

func (in *instance) setStateLocked(s State) {
	in.state = s
	in.since = time.Now()
	close(in.changed)
	in.changed = make(chan struct{})
}

// start launches the supervisor; it is a no-op while one is running.
func (in *instance) start() {
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.stop != nil {
		return
	}
	in.stop = make(chan struct{})
	in.done = make(chan struct{})
	in.attempts = 0
	in.setStateLocked(StateStarting)
	go in.supervise(in.stop, in.done)
}

// waitReady blocks until the first start attempt finished.
func (in *instance) waitReady(ctx context.Context) error {
	for {
		in.mu.Lock()
		st, attempts, lastErr, ch := in.state, in.attempts, in.lastErr, in.changed
		in.mu.Unlock()
		switch {
		case st == StateRunning:
			return nil
		case st == StateStopped:
			return fmt.Errorf("plugin %s: stopped", in.man.ID)
		case st == StateBackoff && attempts > 0:
			return fmt.Errorf("plugin %s: %w", in.man.ID, lastErr)
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// halt stops the supervisor and the process, gracefully if possible.
func (in *instance) halt(ctx context.Context) error {
	in.mu.Lock()
	stop, done := in.stop, in.done
	in.stop, in.done = nil, nil
	in.mu.Unlock()
	if stop == nil {
		return nil
	}
	close(stop)
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (in *instance) supervise(stop, done chan struct{}) {
	defer close(done)
	failures := 0
	for {
		began := time.Now()
		err := in.runOnce(stop)
		select {
		case <-stop:
			in.mu.Lock()
			in.setStateLocked(StateStopped)
			in.mu.Unlock()
			return
		default:
		}
		if time.Since(began) >= stableAfter {
			failures = 0
		}
		failures++
		delay := in.opts.BackoffMin << min(failures-1, 20)
		delay = min(delay, in.opts.BackoffMax)
		in.mu.Lock()
		in.restarts++
		in.attempts++
		in.lastErr = err
		in.setStateLocked(StateBackoff)
		in.mu.Unlock()
		in.log.Warn("plugin process ended; restarting", "err", err, "backoff", delay)
		if in.opts.OnCrash != nil {
			in.opts.OnCrash(in.man.ID, err)
		}
		select {
		case <-time.After(delay):
			in.mu.Lock()
			in.setStateLocked(StateStarting)
			in.mu.Unlock()
		case <-stop:
			in.mu.Lock()
			in.setStateLocked(StateStopped)
			in.mu.Unlock()
			return
		}
	}
}

// runOnce starts the process, handshakes and waits until it ends or stop
// is closed. The returned error explains why it ended.
func (in *instance) runOnce(stop chan struct{}) error {
	cmd, c, logsDone, err := in.spawn(stop)
	if err != nil {
		return err
	}
	reap := func() {
		in.reap(cmd, logsDone)
	}
	if err := in.handshake(c, stop); err != nil {
		_ = c.close()
		_ = cmd.Process.Kill()
		reap()
		return fmt.Errorf("handshake: %w", err)
	}
	in.mu.Lock()
	in.conn = c
	in.pid = cmd.Process.Pid
	in.attempts++
	in.lastErr = nil
	in.setStateLocked(StateRunning)
	in.mu.Unlock()
	in.log.Info("plugin started", "pid", cmd.Process.Pid)

	var why error
	select {
	case <-c.done:
		why = c.err
	case <-stop:
		in.detach()
		in.shutdown(c)
		why = errors.New("stopped")
	}
	in.detach()
	_ = c.close()
	reap()
	if cmd.ProcessState != nil && !cmd.ProcessState.Success() {
		why = fmt.Errorf("%s (%v)", cmd.ProcessState, why)
	}
	return why
}

func (in *instance) detach() {
	in.mu.Lock()
	in.conn = nil
	in.pid = 0
	in.mu.Unlock()
}

func (in *instance) shutdown(c *conn) {
	ctx, cancel := context.WithTimeout(context.Background(), in.man.Timeouts.Shutdown.D())
	defer cancel()
	if err := c.call(ctx, "shutdown", struct{}{}, nil); err != nil {
		in.log.Warn("plugin shutdown request failed", "err", err)
	}
	_ = c.notify("exit", struct{}{})
}

// reap waits for the process to exit, killing it after the shutdown grace.
func (in *instance) reap(cmd *exec.Cmd, logsDone <-chan struct{}) {
	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(exited)
	}()
	select {
	case <-exited:
	case <-time.After(in.man.Timeouts.Shutdown.D()):
		_ = cmd.Process.Kill()
		<-exited
	}
	select {
	case <-logsDone:
	case <-time.After(time.Second):
	}
}

func (in *instance) spawn(stop chan struct{}) (*exec.Cmd, *conn, <-chan struct{}, error) {
	path, err := in.resolveCommand()
	if err != nil {
		return nil, nil, nil, err
	}
	cmd := exec.Command(path, in.man.Args...)
	cmd.Dir = in.man.Dir
	env := in.env()

	logR, logW, err := os.Pipe()
	if err != nil {
		return nil, nil, nil, err
	}
	cmd.Stderr = logW
	var childEnds []io.Closer
	childEnds = append(childEnds, logW)
	closeAll := func(cs []io.Closer) {
		for _, c := range cs {
			_ = c.Close()
		}
	}

	var (
		inW, outR *os.File
		ln        net.Listener
		sockPath  string
	)
	switch in.man.Transport {
	case TransportUnix:
		dir := in.opts.SocketDir
		if dir == "" {
			dir = os.TempDir()
		}
		sockPath = filepath.Join(dir, "plugrelay-"+in.man.ID+"-"+uuid.NewString()[:8]+".sock")
		ln, err = net.Listen("unix", sockPath)
		if err != nil {
			closeAll([]io.Closer{logR, logW})
			return nil, nil, nil, err
		}
		_ = os.Chmod(sockPath, 0o600)
		env = append(env, "PLUGRELAY_SOCKET="+sockPath)
		cmd.Stdout = logW
	default:
		inR, w, err := os.Pipe()
		if err != nil {
			closeAll([]io.Closer{logR, logW})
			return nil, nil, nil, err
		}
		r, outW, err := os.Pipe()
		if err != nil {
			closeAll([]io.Closer{logR, logW, inR, w})
			return nil, nil, nil, err
		}
		inW, outR = w, r
		cmd.Stdin, cmd.Stdout = inR, outW
		childEnds = append(childEnds, inR, outW)
	}
	cmd.Env = env

	if err := cmd.Start(); err != nil {
		closeAll(childEnds)
		closeAll([]io.Closer{logR})
		if inW != nil {
			closeAll([]io.Closer{inW, outR})
		}
		if ln != nil {
			_ = ln.Close()
		}
		return nil, nil, nil, fmt.Errorf("start %s: %w", path, err)
	}
	closeAll(childEnds)

	logsDone := make(chan struct{})
	go func() {
		defer close(logsDone)
		defer logR.Close()
		sc := bufio.NewScanner(logR)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			in.log.Info(sc.Text(), "source", "stderr")
		}
	}()

	var c *conn
	if ln != nil {
		nc, err := acceptTimeout(ln, in.man.Timeouts.Startup.D(), stop)
		_ = ln.Close()
		_ = os.Remove(sockPath)
		if err != nil {
			_ = cmd.Process.Kill()
			in.reap(cmd, logsDone)
			return nil, nil, nil, fmt.Errorf("accept plugin connection: %w", err)
		}
		c = newConn(nc, nc, nc, in.handleHost)
	} else {
		c = newConn(outR, inW, closers{inW, outR}, in.handleHost)
	}
	go c.run()
	return cmd, c, logsDone, nil
}

type closers []io.Closer

func (cs closers) Close() error {
	var errs []error
	for _, c := range cs {
		errs = append(errs, c.Close())
	}
	return errors.Join(errs...)
}

func acceptTimeout(ln net.Listener, d time.Duration, stop chan struct{}) (net.Conn, error) {
	type res struct {
		c   net.Conn
		err error
	}
	ch := make(chan res, 1)
	go func() {
		c, err := ln.Accept()
		ch <- res{c, err}
	}()
	select {
	case r := <-ch:
		return r.c, r.err
	case <-time.After(d):
		return nil, errors.New("plugin did not connect before the startup timeout")
	case <-stop:
		return nil, errors.New("stopped")
	}
}

func (in *instance) resolveCommand() (string, error) {
	cmd := in.man.Command
	if filepath.IsAbs(cmd) {
		return cmd, nil
	}
	if filepath.Base(cmd) != cmd {
		return filepath.Join(in.man.Dir, cmd), nil
	}
	return exec.LookPath(cmd)
}

// env is a minimal environment: the host's secrets in its own environment
// must not leak into third-party processes.
func (in *instance) env() []string {
	var env []string
	for _, k := range []string{"PATH", "HOME", "LANG", "LC_ALL", "TMPDIR", "TZ", "SYSTEMROOT"} {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	for k, v := range in.man.Env {
		env = append(env, k+"="+v)
	}
	env = append(env,
		"PLUGRELAY_PLUGIN_ID="+in.man.ID,
		"PLUGRELAY_PROTOCOL_VERSION="+strconv.Itoa(ProtocolVersion),
	)
	return env
}

func (in *instance) handshake(c *conn, stop chan struct{}) error {
	ctx, cancel := context.WithTimeout(context.Background(), in.man.Timeouts.Startup.D())
	defer cancel()
	go func() {
		select {
		case <-stop:
			cancel()
		case <-ctx.Done():
		}
	}()
	p := initializeParams{
		ProtocolVersion: ProtocolVersion,
		HostVersion:     in.opts.HostVersion,
		PluginID:        in.man.ID,
		Settings:        in.globalSettings(),
	}
	var res initializeResult
	if err := c.call(ctx, "initialize", p, &res); err != nil {
		return err
	}
	if res.ProtocolVersion != ProtocolVersion {
		return fmt.Errorf("plugin speaks protocol %d, host speaks %d", res.ProtocolVersion, ProtocolVersion)
	}
	if res.Manifest.ID != in.man.ID {
		return fmt.Errorf("plugin reports id %q, manifest says %q", res.Manifest.ID, in.man.ID)
	}
	if pts := res.Capabilities.Points; len(pts) > 0 {
		for _, p := range in.man.Points {
			if !slices.Contains(pts, p) {
				return fmt.Errorf("manifest lists %q but the plugin does not implement it", p)
			}
		}
	}
	return nil
}

func (in *instance) globalSettings() map[string]json.RawMessage {
	if in.host == nil || len(in.man.Settings) == 0 {
		return nil
	}
	s := in.host.Settings()
	if s == nil {
		return nil
	}
	out := map[string]json.RawMessage{}
	for _, d := range in.man.Settings {
		if v := s.Get(d.Key); v.IsSet() {
			out[d.Key] = v.Raw()
		}
	}
	return out
}

// acquire returns the live connection and holds a concurrency slot until
// release is called. It fails fast when the process is down.
func (in *instance) acquire(ctx context.Context) (*conn, func(), error) {
	if c := in.current(); c == nil {
		return nil, nil, in.unavailable()
	}
	select {
	case in.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	}
	release := func() { <-in.sem }
	c := in.current()
	if c == nil {
		release()
		return nil, nil, in.unavailable()
	}
	return c, release, nil
}

func (in *instance) current() *conn {
	in.mu.Lock()
	defer in.mu.Unlock()
	return in.conn
}

func (in *instance) unavailable() error {
	in.mu.Lock()
	st, last := in.state, in.lastErr
	in.mu.Unlock()
	if st == StateStopped {
		return errStoppedConn
	}
	if last != nil {
		return fmt.Errorf("%w (%s): %v", errConnClosed, st, last)
	}
	return fmt.Errorf("%w (%s)", errConnClosed, st)
}

// call performs one request with the method's timeout and the plugin's
// concurrency limit.
func (in *instance) call(ctx context.Context, method string, params, out any) error {
	ctx, cancel := context.WithTimeoutCause(ctx, in.man.CallTimeout(method), errCallTimeout)
	defer cancel()
	c, release, err := in.acquire(ctx)
	if err != nil {
		return in.wrap(ctx, method, err)
	}
	defer release()
	return in.wrap(ctx, method, c.call(ctx, method, params, out))
}

// wrap turns transport and context errors into adapter errors.
func (in *instance) wrap(ctx context.Context, method string, err error) error {
	if err == nil {
		return nil
	}
	var re *rpcError
	switch {
	case errors.As(err, &re):
		return fromRPC(in.man.ID, method, re)
	case errors.Is(err, context.DeadlineExceeded):
		return &CallError{Plugin: in.man.ID, Method: method, Message: in.man.CallTimeout(method).String(), Err: ErrTimeout}
	case errors.Is(err, context.Canceled):
		if errors.Is(context.Cause(ctx), errCallTimeout) {
			return &CallError{Plugin: in.man.ID, Method: method, Err: ErrTimeout}
		}
		return &CallError{Plugin: in.man.ID, Method: method, Err: context.Canceled}
	case errors.Is(err, errStoppedConn):
		return &CallError{Plugin: in.man.ID, Method: method, Err: ErrStopped}
	case errors.Is(err, errConnClosed):
		return &CallError{Plugin: in.man.ID, Method: method, Message: err.Error(), Err: ErrUnavailable}
	}
	return &CallError{Plugin: in.man.ID, Method: method, Message: err.Error(), Err: ErrPlugin}
}

func (in *instance) status() Status {
	in.mu.Lock()
	defer in.mu.Unlock()
	s := Status{
		ID: in.man.ID, Name: in.man.Name, Version: in.man.Version, Kinds: in.man.Kinds,
		State: in.state, PID: in.pid, Restarts: in.restarts, Since: in.since, Manifest: in.man,
	}
	if in.lastErr != nil {
		s.LastError = in.lastErr.Error()
	}
	return s
}
