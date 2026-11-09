// Package app wires a running relay together: store, state, secrets,
// egress, plugins, router, pipeline, admin API and HTTP server. Run is the
// whole lifetime of "plugrelay serve".
//
// Configuration that routing depends on is read from the store into an
// immutable snapshot (routing table, decrypted credentials, prices,
// settings resolver, plugin chain) and swapped atomically when the store's
// version counter moves; in-flight requests keep the snapshot they
// started with.
package app

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/initxy/plugrelay/internal/admin"
	"github.com/initxy/plugrelay/internal/config"
	"github.com/initxy/plugrelay/internal/egress"
	"github.com/initxy/plugrelay/internal/extplugin"
	"github.com/initxy/plugrelay/internal/features"
	"github.com/initxy/plugrelay/internal/httpx"
	"github.com/initxy/plugrelay/internal/mcp"
	"github.com/initxy/plugrelay/internal/pipeline"
	"github.com/initxy/plugrelay/internal/router"
	"github.com/initxy/plugrelay/internal/seal"
	"github.com/initxy/plugrelay/internal/secret"
	"github.com/initxy/plugrelay/internal/server"
	"github.com/initxy/plugrelay/internal/settings"
	"github.com/initxy/plugrelay/internal/state"
	"github.com/initxy/plugrelay/internal/store"
	"github.com/initxy/plugrelay/internal/version"
	"github.com/initxy/plugrelay/pkg/plugin"
	"github.com/initxy/plugrelay/web"

	_ "github.com/initxy/plugrelay/pkg/protocol/anthropic"
	_ "github.com/initxy/plugrelay/pkg/protocol/coherererank"
	_ "github.com/initxy/plugrelay/pkg/protocol/gemini"
	_ "github.com/initxy/plugrelay/pkg/protocol/geminiembeddings"
	_ "github.com/initxy/plugrelay/pkg/protocol/openaiaudio"
	_ "github.com/initxy/plugrelay/pkg/protocol/openaichat"
	_ "github.com/initxy/plugrelay/pkg/protocol/openaicompletions"
	_ "github.com/initxy/plugrelay/pkg/protocol/openaiembeddings"
	_ "github.com/initxy/plugrelay/pkg/protocol/openaiimages"
	_ "github.com/initxy/plugrelay/pkg/protocol/openairesponses"
	_ "github.com/initxy/plugrelay/pkg/protocol/rerank"
	_ "github.com/initxy/plugrelay/pkg/protocol/teirerank"
	_ "github.com/initxy/plugrelay/plugins/builtin"
)

// Environment variables read by the app beyond relay.yaml.
const (
	// EnvAdminToken is an operator Bearer token for the admin API and /metrics.
	EnvAdminToken = "PLUGRELAY_ADMIN_TOKEN"
	// EnvTestMode drops every plugin that reports data off the box.
	EnvTestMode = "PLUGRELAY_TEST_MODE"
)

// PollEvery is how often the store version is checked for changes.
const PollEvery = 2 * time.Second

// App is a running relay.
type App struct {
	cfg    *config.Config
	log    *slog.Logger
	store  store.Store
	box    *secret.Box
	sealer *seal.Sealer
	state  state.State
	egress *egress.Client
	// clientIP resolves the client address behind trusted proxies.
	clientIP func(*http.Request) string
	router   *router.Router
	pipe     *pipeline.Pipeline
	queue    *pipeline.SinkQueue
	// ext is set once in New and never cleared. extMu guards extClosed and
	// extWG, the starts and stops syncExternal runs in the background;
	// extStop cancels them.
	ext       *extplugin.Manager
	extMu     sync.Mutex
	extClosed bool
	extWG     sync.WaitGroup
	extCtx    context.Context
	extStop   context.CancelFunc
	admin     *admin.Handler
	mcp       *mcp.Gateway
	keys      *keyLookup

	plugins *pluginSet
	cur     atomic.Pointer[runtime]
	// resolver is the newest settings, live before the runtime that
	// carries them so plugins initialize against them.
	resolver atomic.Pointer[settings.Resolver]
	started  atomic.Bool
	// rebuildMu serializes snapshot rebuilds; reloadErr is the error of the
	// last one when it failed.
	rebuildMu sync.Mutex
	reloadErr atomic.Pointer[string]

	listenAddr     string
	listenLoopback bool
	testMode       bool

	ctx context.Context // lifetime of background work
	wg  sync.WaitGroup
}

// Run starts the relay and blocks until ctx is cancelled, then shuts down
// gracefully: stop accepting requests and let in-flight ones finish, stop
// jobs, drain the sink queue, stop external plugins, close plugins, state
// and store.
func Run(ctx context.Context, cfg *config.Config) error {
	a, err := New(ctx, cfg, newLogger(cfg.Log))
	if err != nil {
		return err
	}
	return a.Serve(ctx)
}

// New opens every dependency and builds the first snapshot without
// listening yet.
func New(ctx context.Context, cfg *config.Config, log *slog.Logger) (*App, error) {
	return newApp(ctx, cfg, log, true)
}

// newApp is New; without serve no external plugin is loaded or started, as
// for a one-shot command.
func newApp(ctx context.Context, cfg *config.Config, log *slog.Logger, serve bool) (*App, error) {
	a := &App{cfg: cfg, log: log, ctx: ctx, testMode: os.Getenv(EnvTestMode) != ""}
	a.extCtx, a.extStop = context.WithCancel(context.WithoutCancel(ctx))
	ok := false
	defer func() {
		if !ok {
			a.closeAll()
		}
	}()
	master, err := cfg.LoadMasterKey()
	if err != nil {
		log.Warn("no usable master key: credentials cannot be stored and sealed state does not survive a restart", "err", err)
		master = make([]byte, secret.MasterKeySize)
		_, _ = rand.Read(master)
	} else if a.box, err = secret.NewBox(master); err != nil {
		return nil, fmt.Errorf("master key: %w", err)
	}
	previous, err := cfg.LoadPreviousMasterKeys()
	if err != nil {
		return nil, err
	}
	if a.sealer, err = seal.New(master, previous...); err != nil {
		return nil, fmt.Errorf("sealer: %w", err)
	}
	if a.store, err = store.Open(ctx, store.Config{Driver: cfg.Store.Driver, DSN: cfg.Store.DSN, MaxConns: int(cfg.Store.MaxConns), Secrets: a.box}); err != nil {
		return nil, err
	}
	if cfg.State.Driver == "sql" {
		a.state, err = a.store.State(ctx)
	} else {
		a.state, err = state.Open(ctx, state.Config{Driver: cfg.State.Driver, URL: cfg.State.URL})
	}
	if err != nil {
		return nil, fmt.Errorf("state: %w", err)
	}
	var allow []netip.Prefix
	for _, c := range cfg.Egress.AllowPrivateCIDRs {
		p, err := netip.ParsePrefix(c)
		if err != nil {
			return nil, fmt.Errorf("egress.allow_private_cidrs: %w", err)
		}
		allow = append(allow, p)
	}
	a.egress = egress.New(egress.Options{AllowCIDRs: allow})
	a.clientIP = httpx.IPResolver{Trusted: cfg.TrustedProxyPrefixes()}.ClientIP
	a.keys = newKeyLookup(a.store)
	turn := sha256.Sum256(append(append([]byte{}, master...), "plugrelay turn token"...))
	a.router = router.New(a.state, router.Options{Secret: turn[:], OnCredential: a.onCredential})
	a.plugins = newPluginSet(a)
	if serve && cfg.ExternalPlugins.Enabled {
		a.ext = extplugin.NewManager(extplugin.Options{HostVersion: version.Version, Logger: log.With("component", "extplugin"),
			Host: func(id string) plugin.Host { return a.plugins.host(id) }, OnCrash: a.onExtensionCrash})
		if err := a.ext.Load(cfg.ExternalPlugins.Dir); err != nil {
			log.Warn("external plugins", "dir", cfg.ExternalPlugins.Dir, "err", err)
		}
	}
	a.queue = pipeline.NewSinkQueue(func() []plugin.Sink { return a.runtime().sinks },
		pipeline.SinkQueueOptions{Size: 4096, Timeout: 10 * time.Second, CloseTimeout: minQueueFlush,
			Logger: log.With("component", "sinks")})
	a.pipe = pipeline.New(pipeline.Deps{
		Snapshot:    func() *pipeline.Snapshot { return a.runtime().snap },
		Router:      a.router,
		Egress:      a.egress,
		Sealer:      a.sealer,
		Plugins:     func() *pipeline.Chain { return a.runtime().chain },
		Settings:    a.settingFor,
		Records:     a.queue,
		Credentials: credentialStore{a.store, a.box},
		Lock:        a.state.Lock,
		Logger:      log.With("component", "pipeline"),
		ClientIP:    a.clientIP,
		Timeouts:    a.defaultTimeouts(),
		Native:      pipeline.Native{Authorize: a.authorizeNative, MaxBody: cfg.Native.MaxBodyBytes},
		Batches:     batchStoreOf(a.store),
		PublicURL:   func(ctx context.Context) string { return admin.PublicURL(ctx, a.store) },
		WebSocket: pipeline.WebSocket{MaxSession: cfg.WebSocket.MaxSession.D(), Idle: cfg.WebSocket.IdleTimeout.D(),
			ReadyTimeout: cfg.WebSocket.SessionReadyTimeout.D(), MaxMessage: cfg.WebSocket.MaxMessageBytes, ResponseReserve: cfg.WebSocket.ResponseReserveTokens,
			Authorize: a.authorizeWebSocket},
	})
	if err := a.rebuild(ctx, true); err != nil {
		return nil, err
	}
	a.mcp = a.newMCP(master)
	a.admin = admin.New(admin.Deps{MCP: a.mcp, Store: a.store, Box: a.box, Config: cfg, Runtime: runtimeAPI{a},
		Version: version.Version, AdminToken: os.Getenv(EnvAdminToken), Logger: log.With("component", "admin"),
		Listen:   func() (string, bool) { return a.listenAddr, a.listenLoopback },
		ClientIP: a.clientIP, State: a.state,
		HTTPClient: func() *http.Client { return (&host{a: a}).HTTPClient() }})
	ok = true
	return a, nil
}

// defaultTimeouts are the configured upstream timers.
func (a *App) defaultTimeouts() egress.Timeouts {
	t := a.cfg.Egress.DefaultTimeouts
	return egress.Timeouts{FirstByte: t.FirstByte.D(), Idle: t.Idle.D(), Total: t.Total.D()}
}

// onCredential persists durable credential changes; the pipeline is built
// after the router, so it is looked up at call time.
func (a *App) onCredential(ev router.CredEvent) {
	if a.pipe != nil {
		a.pipe.OnCredential(ev)
	}
}

// Serve listens and serves until ctx is cancelled.
func (a *App) Serve(ctx context.Context) error {
	ln, err := a.listen(ctx)
	if err != nil {
		a.closeAll()
		return err
	}
	return a.ServeListener(ctx, ln)
}

// ServeListener serves on ln until ctx is cancelled.
func (a *App) ServeListener(ctx context.Context, ln net.Listener) error {
	a.listenAddr = ln.Addr().String()
	a.listenLoopback = isLoopback(ln.Addr())
	bg, stopBG := context.WithCancel(context.WithoutCancel(ctx))
	a.ctx = bg
	a.started.Store(true)
	a.startBackground(bg)
	srv := server.New(a.serverOptions())
	a.log.Info("plugrelay listening", "addr", a.listenAddr, "version", version.Version)
	err := srv.Serve(ctx, ln)
	// Requests cut off by the shutdown grace are still finishing; their
	// records must reach the queue before it closes. shutdown.drain bounds
	// that and the queue flush together, but the flush always gets at
	// least minQueueFlush.
	deadline := time.Now().Add(a.cfg.DrainTimeout())
	dctx, cancel := context.WithDeadline(context.Background(), deadline)
	if werr := a.pipe.Wait(dctx); werr != nil {
		a.log.Error("shutdown: requests still finishing; their records may be lost", "err", werr)
	}
	cancel()
	stopBG()
	a.wg.Wait()
	if floor := time.Now().Add(minQueueFlush); deadline.Before(floor) {
		deadline = floor
	}
	fctx, cancel := context.WithDeadline(context.Background(), deadline)
	if ferr := a.queue.Shutdown(fctx); ferr != nil {
		st := a.queue.Stats()
		a.log.Error("shutdown: record queue not drained in time", "ledger_lost", st.LedgerLost, "dropped", st.Dropped)
	}
	cancel()
	a.closeAll()
	return err
}

// minQueueFlush is the least time the record queue gets to drain at
// shutdown, after requests finished or shutdown.drain ran out.
const minQueueFlush = 10 * time.Second

func (a *App) serverOptions() server.Options {
	o := server.Options{
		Pipeline: a.pipe,
		Auth:     func() []plugin.Authenticator { return a.runtime().auth },
		Limits:   httpx.Limits{MaxCompressed: a.cfg.Limits.MaxBodyBytes, MaxDecompressed: a.cfg.Limits.MaxDecompressedBytes},
		API:      a.admin.API(),
		Ready:    a.ready,
		Logger:   a.log.With("component", "http"),

		ShutdownGrace: a.cfg.Shutdown.Grace.D(),
		Batch:         a.pipe,
		WebSocket:     a.pipe,
	}
	if a.cfg.ConsoleEnabled() {
		o.Console = web.Dist()
		o.CaptchaProviders = a.captchaProviders
	}
	if a.cfg.Native.Enabled {
		o.Native = a.pipe
	}
	if a.mcp != nil {
		o.MCP = a.mcp
		o.MCPOn = func() bool { return a.featureOn(features.MCP) }
		o.MCPAuthorize = a.authorizeWebSocket
	}
	if m := a.plugins.metrics(); m != nil {
		o.Metrics = m.Handler()
		if os.Getenv(EnvAdminToken) != "" {
			o.MetricsAuth = a.admin.IsAdminToken
		}
	}
	return o
}

// Handler returns the HTTP handler without listening, for tests.
func (a *App) Handler() *server.Server { return server.New(a.serverOptions()) }

func (a *App) ready(ctx context.Context) error {
	if a.cur.Load() == nil {
		return errors.New("no configuration snapshot yet")
	}
	_, err := a.store.Version(ctx)
	return err
}

// Store exposes the business store (tests and wiring).
func (a *App) Store() store.Store { return a.store }

// Router exposes the router, whose live state the demo seeds.
func (a *App) Router() *router.Router { return a.router }

// startBackground starts snapshot polling, jobs and external plugins.
func (a *App) startBackground(ctx context.Context) {
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		a.poll(ctx)
	}()
	a.plugins.startJobs(ctx)
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		a.housekeeping(ctx)
	}()
}

// housekeeping purges expired rows, retries saving refreshed credential
// secrets the store refused and reports sink drops.
func (a *App) housekeeping(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := a.store.PurgeExpired(ctx); err != nil && ctx.Err() == nil {
				a.log.Warn("purge expired", "err", err)
			}
			a.pipe.SettleBatches(ctx)
			unsaved := a.pipe.PersistPending(ctx)
			if m := a.plugins.metrics(); m != nil {
				st := a.queue.Stats()
				m.SetSinkDropped("queue", st.Dropped)
				m.SetSinkLedger(st.LedgerWaited, st.LedgerInline, st.LedgerFailed, st.LedgerLost)
				m.SetCredentialSecrets(a.pipe.SecretSaveFailures(), unsaved)
			}
		}
	}
}

// closeAll releases everything in dependency order; safe on a partly
// built App. The record queue drains before external plugins stop, since
// some of its sinks may be external.
func (a *App) closeAll() {
	if a.mcp != nil {
		a.mcp.Close()
	}
	if a.queue != nil {
		a.queue.Close()
		a.queue = nil
	}
	if a.extStop != nil {
		a.extMu.Lock()
		a.extClosed = true
		a.extMu.Unlock()
		a.extStop()
		a.extWG.Wait()
	}
	if a.ext != nil {
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_ = a.ext.Stop(sctx)
		cancel()
	}
	if a.plugins != nil {
		a.plugins.close()
	}
	if a.state != nil {
		_ = a.state.Close()
		a.state = nil
	}
	if a.store != nil {
		if err := a.store.Close(); err != nil {
			a.log.Warn("close store", "err", err)
		}
		a.store = nil
	}
}
