// Package pricesync is the job.pricesync plugin: it reads the public model
// catalog of models.dev, finds our models in it by their upstream model
// names, and hands the catalog's prices and limits to the core as price
// suggestions. The core decides what applies (manual prices always win).
package pricesync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/plugin"
)

// ID is the plugin id.
const ID = "job.pricesync"

// DefaultURL is the models.dev catalog: provider id → {models: model id → entry}.
const DefaultURL = "https://models.dev/api.json"

// DefaultInterval applies when the interval setting is unset.
const DefaultInterval = 24 * time.Hour

// maxCatalog bounds the catalog download (about 6 MB in late 2026).
const maxCatalog = 64 << 20

func init() { plugin.Register(ID, func() plugin.Plugin { return New(nil) }) }

// Route is one way a model of ours is served: the upstream's manifest (or
// provider) id and the model name the upstream knows it by.
type Route struct {
	Provider      string
	UpstreamModel string
}

// Model is one model of ours with its routes.
type Model struct {
	Name   string
	Routes []Route
}

// Suggestion is the catalog's view of one model, prices in micro-units of
// Currency per million tokens.
type Suggestion struct {
	Model        string
	Ref          string // "<catalog provider>/<catalog model>"
	Input        int64
	CacheRead    int64
	CacheWrite5m int64
	CacheWrite1h int64
	Output       int64
	Currency     string
	Info         ir.ModelInfo
}

// Store is the core's side of the job.
type Store interface {
	Models(ctx context.Context) ([]Model, error)
	// Currency is the site currency every price is in.
	Currency(ctx context.Context) string
	// Save stores the suggestions; the core applies followed ones.
	Save(ctx context.Context, s []Suggestion, at time.Time) error
}

// StoreHost is implemented by a Host that hands the job its store.
type StoreHost interface {
	PriceSyncStore() Store
}

// Status is the outcome of the last run, kept in the plugin's KV under
// StatusKey.
type Status struct {
	RanAt   time.Time `json:"ran_at"`
	Matched int       `json:"matched"`
	Models  int       `json:"models"`
	// Code says what failed: StatusNoRate, StatusDownload or StatusFailed.
	Code  string `json:"code,omitempty"`
	Error string `json:"error,omitempty"`
}

// Status codes of a failed run.
const (
	StatusNoRate   = "no_rate"
	StatusDownload = "download"
	StatusFailed   = "failed"
)

var (
	errNoRate   = errors.New("no exchange rate")
	errDownload = errors.New("download price catalog")
)

// StatusKey is the KV key of the last run's Status.
const StatusKey = "status"

// Job implements plugin.Job.
type Job struct {
	st    Store
	host  plugin.Host
	log   *slog.Logger
	every time.Duration
}

// New returns the job; a nil store is taken from the Host in Init.
func New(st Store) *Job {
	return &Job{st: st, log: slog.New(slog.DiscardHandler), every: DefaultInterval}
}

// Info implements plugin.Plugin.
func (j *Job) Info() plugin.Info {
	return plugin.Info{
		ID:          ID,
		Kind:        plugin.KindJob,
		Version:     "1.0.0",
		Description: "Reads model prices from models.dev and suggests them; prices entered by hand are never replaced.",
		FailPolicy:  plugin.FailOpen,
		Settings: []plugin.Setting{
			{Key: "interval", Type: plugin.TypeDuration, Default: DefaultInterval.String(), Scopes: plugin.Global,
				Merge: plugin.Override, Restart: true, Label: plugin.T("Check prices every", "多久查一次价格"),
				Help: plugin.T("Takes effect after a restart. The first check runs about a minute after the feature is turned on.",
					"重启后生效。开启后大约一分钟内会先查一次。")},
			{Key: "usd_rate", Type: plugin.TypeFloat, Default: 0, Scopes: plugin.Global, Merge: plugin.Override,
				Label: plugin.T("Value of 1 US dollar in the site currency", "1 美元折合多少站点货币"),
				Help: plugin.T("models.dev lists prices in US dollars. Needed when the site currency is not USD; with USD it is ignored.",
					"models.dev 的价格是美元。站点币种不是美元时必须填；是美元时不用管。")},
			{Key: "url", Type: plugin.TypeString, Default: DefaultURL, Scopes: plugin.Global, Merge: plugin.Override,
				Label: plugin.T("Catalog address", "价格目录地址"),
				Help: plugin.T("A copy of the models.dev catalog in the same format, for relays that cannot reach models.dev.",
					"可以换成格式相同的 models.dev 目录镜像，给连不上 models.dev 的中转用。")},
		},
	}
}

// Init implements plugin.Initializer.
func (j *Job) Init(_ context.Context, h plugin.Host) error {
	if sh, ok := h.(StoreHost); ok && j.st == nil {
		j.st = sh.PriceSyncStore()
	}
	if j.st == nil {
		return errors.New("pricesync: no store: construct with New(store) or provide a Host implementing StoreHost")
	}
	j.host, j.log = h, h.Logger()
	if d := h.Settings().Get("interval").Duration(); d > 0 {
		j.every = d
	}
	return nil
}

// Every implements plugin.Job.
func (j *Job) Every() time.Duration { return j.every }

// Run implements plugin.Job.
func (j *Job) Run(ctx context.Context) error {
	st, err := j.run(ctx)
	st.RanAt = time.Now().UTC()
	if err != nil {
		st.Error, st.Code = err.Error(), StatusFailed
		switch {
		case errors.Is(err, errNoRate):
			st.Code = StatusNoRate
		case errors.Is(err, errDownload):
			st.Code = StatusDownload
		}
	}
	if b, e := json.Marshal(st); e == nil {
		_ = j.host.KV().Put(ctx, StatusKey, b, 0)
	}
	return err
}

func (j *Job) run(ctx context.Context) (Status, error) {
	s := j.host.Settings()
	rate := 1.0
	cur := strings.ToUpper(j.st.Currency(ctx))
	if cur != "USD" {
		rate = s.Get("usd_rate").Float()
		if !(rate > 0) {
			return Status{}, fmt.Errorf("%w: the site currency is %s: set how much 1 US dollar is worth in it", errNoRate, cur)
		}
	}
	url := s.Get("url").String()
	if url == "" {
		url = DefaultURL
	}
	cat, err := Fetch(ctx, j.host.HTTPClient(), url)
	if err != nil {
		return Status{}, fmt.Errorf("%w: %w", errDownload, err)
	}
	models, err := j.st.Models(ctx)
	if err != nil {
		return Status{}, err
	}
	out := Match(cat, models, rate, cur)
	st := Status{Matched: len(out), Models: len(models)}
	if len(out) > 0 {
		if err := j.st.Save(ctx, out, time.Now().UTC()); err != nil {
			return st, err
		}
	}
	j.log.Info("prices checked", "models", len(models), "matched", len(out))
	return st, nil
}

// Catalog is the parsed models.dev catalog.
type Catalog map[string]Provider

// Provider is one catalog provider.
type Provider struct {
	ID     string           `json:"id"`
	Models map[string]Entry `json:"models"`
}

// Entry is one catalog model.
type Entry struct {
	ID         string `json:"id"`
	Reasoning  bool   `json:"reasoning"`
	ToolCall   bool   `json:"tool_call"`
	Modalities struct {
		Input []string `json:"input"`
	} `json:"modalities"`
	Limit struct {
		Context int `json:"context"`
		Output  int `json:"output"`
	} `json:"limit"`
	// Cost is US dollars per million tokens.
	Cost *struct {
		Input      float64 `json:"input"`
		Output     float64 `json:"output"`
		CacheRead  float64 `json:"cache_read"`
		CacheWrite float64 `json:"cache_write"`
	} `json:"cost"`
}

// Parse reads a catalog.
func Parse(r io.Reader) (Catalog, error) {
	var c Catalog
	if err := json.NewDecoder(r).Decode(&c); err != nil {
		return nil, fmt.Errorf("read price catalog: %w", err)
	}
	if len(c) == 0 {
		return nil, errors.New("read price catalog: empty")
	}
	return c, nil
}

// Fetch downloads and parses the catalog.
func Fetch(ctx context.Context, hc *http.Client, url string) (Catalog, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	return Parse(io.LimitReader(resp.Body, maxCatalog))
}

// catalogProviders maps our manifest ids to catalog provider ids where
// they differ.
var catalogProviders = map[string]string{
	"azure-openai": "azure", "bedrock": "amazon-bedrock", "gemini": "google", "vertex-gemini": "google-vertex",
	"vertex-anthropic": "google-vertex-anthropic", "moonshot-kimi": "moonshotai-cn", "moonshot-kimi-intl": "moonshotai",
	"zhipu-glm": "zhipuai", "zhipu-glm-coding": "zhipuai-coding-plan", "qwen-dashscope": "alibaba-cn",
	"qwen-dashscope-intl": "alibaba", "minimax": "minimax-cn", "minimax-intl": "minimax", "siliconflow": "siliconflow-cn",
	"together": "togetherai", "fireworks": "fireworks-ai", "novita": "novita-ai", "xiaomi-mimo": "xiaomi",
	"volcengine-ark": "volcengine", "volcengine-ark-coding": "volcengine-coding-plan", "nvidia-nim": "nvidia",
}

// firstParty are the model makers' own catalog entries, tried in order
// when no route's provider lists the model.
var firstParty = []string{"anthropic", "openai", "google", "deepseek", "xai", "mistral", "moonshotai", "zhipuai", "zai",
	"alibaba", "minimax", "xiaomi", "stepfun", "cohere", "perplexity", "meta"}

// Match finds each model in the catalog: first under a route's own
// provider by the upstream model name, then the model makers' entries by
// the upstream names and our name, then the same with a vendor or region
// prefix ("us.anthropic.", "openai/") taken off. Entries without prices
// are skipped. rate converts dollars into currency.
func Match(c Catalog, models []Model, rate float64, currency string) []Suggestion {
	var out []Suggestion
	for _, m := range models {
		if s, ok := matchOne(c, m); ok {
			out = append(out, suggestion(m.Name, s.ref, s.e, rate, currency))
		}
	}
	return out
}

type hit struct {
	ref string
	e   Entry
}

func priced(e Entry) bool {
	return e.Cost != nil && (e.Cost.Input > 0 || e.Cost.Output > 0)
}

func (c Catalog) lookup(provider, model string) (hit, bool) {
	if e, ok := c[provider].Models[model]; ok && priced(e) {
		return hit{provider + "/" + model, e}, true
	}
	return hit{}, false
}

func matchOne(c Catalog, m Model) (hit, bool) {
	for _, r := range m.Routes {
		p := r.Provider
		if a, ok := catalogProviders[p]; ok {
			p = a
		}
		if h, ok := c.lookup(p, r.UpstreamModel); ok {
			return h, true
		}
	}
	var names []string
	for _, r := range m.Routes {
		names = append(names, r.UpstreamModel)
	}
	names = append(names, m.Name)
	for _, strip := range []bool{false, true} {
		for _, n := range names {
			if strip {
				n = bare(n)
			}
			for _, p := range firstParty {
				if h, ok := c.lookup(p, n); ok {
					return h, true
				}
			}
		}
	}
	return hit{}, false
}

// bare drops a vendor path ("openai/gpt-5.5") and a cloud's region and
// vendor prefix ("us.anthropic.claude-…").
func bare(name string) string {
	if i := strings.LastIndexByte(name, '/'); i >= 0 {
		name = name[i+1:]
	}
	parts := strings.Split(name, ".")
	for i, p := range parts {
		if i < len(parts)-1 && slices.Contains(firstParty, p) {
			return strings.Join(parts[i+1:], ".")
		}
	}
	return name
}

func suggestion(model, ref string, e Entry, rate float64, currency string) Suggestion {
	micro := func(usd float64) int64 { return int64(math.Round(usd * rate * 1e6)) }
	s := Suggestion{Model: model, Ref: ref, Currency: currency, Input: micro(e.Cost.Input), Output: micro(e.Cost.Output),
		CacheRead: micro(e.Cost.CacheRead), CacheWrite5m: micro(e.Cost.CacheWrite)}
	s.CacheWrite1h = s.CacheWrite5m
	// The catalog has one cache-write price. Where it is the 1.25× input
	// of 5-minute writes, the 1-hour price is 2× input.
	if e.Cost.CacheWrite > 0 && math.Abs(e.Cost.CacheWrite-1.25*e.Cost.Input) < 1e-9+e.Cost.Input*1e-6 {
		s.CacheWrite1h = micro(2 * e.Cost.Input)
	}
	s.Info = ir.ModelInfo{ID: model, ContextWindow: e.Limit.Context, MaxOutput: e.Limit.Output, Reasoning: e.Reasoning,
		Tools: e.ToolCall, Vision: slices.Contains(e.Modalities.Input, "image")}
	return s
}
