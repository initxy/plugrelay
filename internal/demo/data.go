package demo

// The demo's business data. Upstreams use real manifests with every
// endpoint pointed at the fake upstream; prices are the manifests' list
// prices (USD per million tokens).

type endpointSpec struct {
	id, protocol string
	// path is appended to the fake upstream's URL; the manifests differ
	// in where their base URL ends.
	path string
}

type credSpec struct {
	key, label, kind string
	owner            string // user key of a personal credential
	maxConcurrency   int
}

type upstreamSpec struct {
	key, name, manifest string
	tags                []string
	vars                map[string]any
	endpoints           []endpointSpec
	creds               []credSpec
}

var upstreamSpecs = []upstreamSpec{
	{key: "anthropic", name: "Anthropic", manifest: "anthropic", tags: []string{"primary"},
		endpoints: []endpointSpec{{"messages", "anthropic", "/anthropic"}},
		creds: []credSpec{{key: "primary", label: "Team plan key", kind: "shared", maxConcurrency: 16},
			{key: "backup", label: "Backup key", kind: "shared", maxConcurrency: 8},
			{key: "legacy", label: "Old org key", kind: "shared"}}},
	{key: "openai", name: "OpenAI", manifest: "openai", tags: []string{"primary"},
		endpoints: []endpointSpec{{"responses", "openai-responses", "/openai/v1"}, {"chat", "openai-chat", "/openai/v1"},
			{"completions", "openai-completions", "/openai/v1"}, {"embeddings", "openai-embeddings", "/openai/v1"},
			{"images", "openai-images", "/openai/v1"}, {"audio", "openai-audio", "/openai/v1"}},
		creds: []credSpec{{key: "team-a", label: "Project A", kind: "shared"}, {key: "team-b", label: "Project B", kind: "shared"},
			{key: "bob", label: "Bob's own key", kind: "personal", owner: "bob"}}},
	{key: "deepseek", name: "DeepSeek", manifest: "deepseek", tags: []string{"cheap"},
		endpoints: []endpointSpec{{"chat", "openai-chat", "/deepseek"}, {"anthropic", "anthropic", "/deepseek/anthropic"},
			{"completions", "openai-completions", "/deepseek/beta"}},
		creds: []credSpec{{key: "main", label: "Main account", kind: "shared"}}},
	{key: "kimi", name: "Kimi", manifest: "moonshot-kimi", tags: []string{"cheap"}, vars: map[string]any{"host": "api.moonshot.cn"},
		endpoints: []endpointSpec{{"chat", "openai-chat", "/kimi/v1"}, {"responses", "openai-responses", "/kimi/v1"},
			{"anthropic", "anthropic", "/kimi/anthropic"}},
		creds: []credSpec{{key: "main", label: "Moonshot platform", kind: "shared"}}},
	{key: "bedrock", name: "Bedrock us-east-1", manifest: "bedrock", tags: []string{"fallback"}, vars: map[string]any{"region": "us-east-1"},
		endpoints: []endpointSpec{{"messages", "anthropic", "/bedrock"}},
		creds:     []credSpec{{key: "prod", label: "AWS prod account", kind: "shared"}}},
}

// price is USD per million tokens.
type price struct{ in, cacheRead, cacheWrite5m, cacheWrite1h, out float64 }

type routeSpec struct {
	up, upstreamModel string
	priority, weight  int
	groups            []string // group keys
}

type modelSpec struct {
	name, display, owner string
	context, maxOutput   int
	reasoning, vision    bool
	cost                 price
	routes               []routeSpec
}

// markup is the list price over the upstream cost.
const markup = 1.25

var modelSpecs = []modelSpec{
	{name: "claude-sonnet-4-6", display: "Claude Sonnet 4.6", owner: "anthropic", context: 1_000_000, maxOutput: 64_000, reasoning: true, vision: true,
		cost: price{3, 0.3, 3.75, 6, 15},
		routes: []routeSpec{{up: "anthropic", upstreamModel: "claude-sonnet-4-6", priority: 10, weight: 3},
			{up: "bedrock", upstreamModel: "us.anthropic.claude-sonnet-4-6-v1:0", priority: 10, weight: 1}}},
	{name: "claude-opus-4-8", display: "Claude Opus 4.8", owner: "anthropic", context: 1_000_000, maxOutput: 128_000, reasoning: true, vision: true,
		cost:   price{5, 0.5, 6.25, 10, 25},
		routes: []routeSpec{{up: "anthropic", upstreamModel: "claude-opus-4-8", priority: 0, weight: 1, groups: []string{"vip"}}}},
	{name: "claude-haiku-4-5", display: "Claude Haiku 4.5", owner: "anthropic", context: 200_000, maxOutput: 64_000, vision: true,
		cost: price{1, 0.1, 1.25, 2, 5},
		routes: []routeSpec{{up: "anthropic", upstreamModel: "claude-haiku-4-5", priority: 10, weight: 1},
			{up: "bedrock", upstreamModel: "us.anthropic.claude-haiku-4-5-20251001-v1:0", priority: 5, weight: 1}}},
	{name: "gpt-5.4", display: "GPT-5.4", owner: "openai", context: 400_000, maxOutput: 128_000, reasoning: true, vision: true,
		cost:   price{2.5, 0.25, 0, 0, 15},
		routes: []routeSpec{{up: "openai", upstreamModel: "gpt-5.4", weight: 1}}},
	{name: "gpt-5.3-codex", display: "GPT-5.3 Codex", owner: "openai", context: 400_000, maxOutput: 128_000, reasoning: true,
		cost:   price{1.75, 0.175, 0, 0, 14},
		routes: []routeSpec{{up: "openai", upstreamModel: "gpt-5.3-codex", weight: 1}}},
	{name: "gpt-5.4-mini", display: "GPT-5.4 mini", owner: "openai", context: 400_000, maxOutput: 128_000, vision: true,
		cost:   price{0.75, 0.075, 0, 0, 4.5},
		routes: []routeSpec{{up: "openai", upstreamModel: "gpt-5.4-mini", weight: 1}}},
	{name: "deepseek-v4-pro", display: "DeepSeek V4 Pro", owner: "deepseek", context: 128_000, maxOutput: 64_000, reasoning: true,
		cost:   price{1.32, 0.044, 0, 0, 3.96},
		routes: []routeSpec{{up: "deepseek", upstreamModel: "deepseek-v4-pro", weight: 1}}},
	{name: "deepseek-v4-flash", display: "DeepSeek V4 Flash", owner: "deepseek", context: 128_000, maxOutput: 32_000,
		cost:   price{0.3, 0.006, 0, 0, 1.2},
		routes: []routeSpec{{up: "deepseek", upstreamModel: "deepseek-v4-flash", weight: 1}}},
	{name: "kimi-k2.5", display: "Kimi K2.5", owner: "moonshot", context: 256_000, maxOutput: 32_000, reasoning: true,
		cost:   price{0.6, 0.15, 0, 0, 2.5},
		routes: []routeSpec{{up: "kimi", upstreamModel: "kimi-k2.5", weight: 1}}},
}

type presetSpec struct {
	name, strategy string
	models         []string
}

var presetSpecs = []presetSpec{
	{name: "best", strategy: "in_order", models: []string{"claude-opus-4-8", "claude-sonnet-4-6", "gpt-5.4"}},
	{name: "budget", strategy: "cheapest", models: []string{"deepseek-v4-flash", "gpt-5.4-mini", "claude-haiku-4-5"}},
}

type groupSpec struct {
	key, name, description string
	multiplier             float64
}

var groupSpecs = []groupSpec{
	{key: "default", name: "default", description: "Everyone starts here", multiplier: 1},
	{key: "vip", name: "vip", description: "Opus access and 20% off", multiplier: 0.8},
}

type userSpec struct {
	key, email, display, group string
	ip                         string
	balance                    float64 // USD
}

// adminUser is the first user; the others are members.
var userSpecs = []userSpec{
	{key: "admin", email: "admin@demo.local", display: "Demo Admin", ip: "198.51.100.10", balance: 200},
	{key: "alice", email: "alice@demo.local", display: "Alice Chen", group: "vip", ip: "203.0.113.21", balance: 120},
	{key: "bob", email: "bob@demo.local", display: "Bob Martin", group: "default", ip: "203.0.113.34", balance: 45},
	{key: "carol", email: "carol@demo.local", display: "Carol Wu", group: "default", ip: "198.51.100.57", balance: 3.2},
	{key: "dave", email: "dave@demo.local", display: "Dave Kim", group: "vip", ip: "192.0.2.80", balance: 60},
}

// The fixed demo password of every user.
const password = "plugrelay-demo"

// Client profiles: what a coding client sends and which models it asks for.
type clientProfile struct {
	name, ua, protocol string
	stream             float64 // share of streamed requests
	models             []weighted
	sessions           bool
}

type weighted struct {
	name   string
	weight int
}

var clients = map[string]clientProfile{
	"claude-code": {name: "Claude Code", ua: "claude-cli/2.1.31 (external, cli)", protocol: "anthropic", stream: 0.96, sessions: true,
		models: []weighted{{"claude-sonnet-4-6", 55}, {"claude-haiku-4-5", 25}, {"claude-opus-4-8", 12}, {"deepseek-v4-pro", 8}}},
	"codex": {name: "Codex", ua: "codex_cli_rs/0.81.0 (Mac OS 15.6.1; arm64) iTerm.app/3.6.2", protocol: "openai-responses", stream: 1, sessions: true,
		models: []weighted{{"gpt-5.3-codex", 70}, {"gpt-5.4", 25}, {"kimi-k2.5", 5}}},
	"cursor": {name: "Cursor", ua: "Cursor/1.8.3 (darwin arm64)", protocol: "openai-chat", stream: 1, sessions: true,
		models: []weighted{{"claude-sonnet-4-6", 50}, {"gpt-5.4", 30}, {"kimi-k2.5", 20}}},
	"gemini-cli": {name: "Gemini CLI", ua: "GeminiCLI/0.14.0 (darwin; arm64)", protocol: "gemini", stream: 0.9, sessions: true,
		models: []weighted{{"gpt-5.4-mini", 50}, {"deepseek-v4-flash", 30}, {"claude-haiku-4-5", 20}}},
	"curl": {name: "curl", ua: "curl/8.7.1", protocol: "openai-chat", stream: 0.2,
		models: []weighted{{"deepseek-v4-flash", 60}, {"gpt-5.4-mini", 30}, {"budget", 10}}},
	"cline": {name: "Cline", ua: "Cline/3.38.2", protocol: "openai-chat", stream: 1, sessions: true,
		models: []weighted{{"claude-sonnet-4-6", 60}, {"deepseek-v4-pro", 40}}},
	"sdk-python": {name: "Python SDK", ua: "OpenAI/Python 1.109.1", protocol: "openai-chat", stream: 0.1,
		models: []weighted{{"deepseek-v4-flash", 70}, {"gpt-5.4-mini", 30}}},
}

type keySpec struct {
	name, user string
	clients    []weighted // client profile keys
	weight     int        // share of all traffic
	// settings is the key's own limits as the admin API takes them.
	settings map[string]any
	// expiresInDays sets an expiry relative to now (negative: expired);
	// disabled keys stop receiving traffic a few days ago.
	expiresInDays int
	disabled      bool
}

var keySpecs = []keySpec{
	{name: "admin-laptop", user: "admin", clients: []weighted{{"claude-code", 80}, {"curl", 20}}, weight: 14},
	{name: "alice-claude-code", user: "alice", clients: []weighted{{"claude-code", 1}}, weight: 26,
		settings: map[string]any{"clients": []string{"claude-code"}, "max_concurrency": 6}},
	{name: "alice-cursor", user: "alice", clients: []weighted{{"cursor", 70}, {"cline", 30}}, weight: 10},
	{name: "bob-codex", user: "bob", clients: []weighted{{"codex", 1}}, weight: 22,
		settings: map[string]any{"budget_monthly": map[string]any{"micro": 50_000_000, "currency": "USD"}, "rpm": 60}},
	{name: "carol-gemini", user: "carol", clients: []weighted{{"gemini-cli", 1}}, weight: 9, expiresInDays: 10,
		settings: map[string]any{"rpm": 30, "budget_total": map[string]any{"micro": 20_000_000, "currency": "USD"}}},
	{name: "dave-ci", user: "dave", clients: []weighted{{"curl", 60}, {"sdk-python", 40}}, weight: 12,
		settings: map[string]any{"models": []string{"deepseek-v4-flash", "gpt-5.4-mini", "budget"}, "ip_allowlist": []string{"192.0.2.0/24"}}},
	{name: "dave-claude-code", user: "dave", clients: []weighted{{"claude-code", 1}}, weight: 7},
	{name: "dave-old", user: "dave", clients: []weighted{{"claude-code", 1}}, weight: 4, expiresInDays: -3},
}
