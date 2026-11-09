import type { ApiBody } from "@/test/api";
import type { Credential, Group, Manifest, Model, Preset, PriceSuggestion, PricingRule, Upstream } from "@/lib/api/types";

/** Test data shaped like the admin API's answers. Each test builds its own copy. */

const hourly = (v: number | null) => Array.from({ length: 24 }, () => v);
const soon = (min: number) => new Date(Date.now() + min * 60_000).toISOString();

export function cred(p: Partial<Credential> & Pick<Credential, "id" | "label">): Credential {
  return { upstream_id: "anthropic", kind: "shared", secret_hint: "sk-••••abcd", max_concurrency: 0, enabled: true, state: { status: "ok" }, ...p };
}

export function upstream(p: Partial<Upstream> & Pick<Upstream, "id" | "name">): Upstream {
  return {
    provider: "provider.anthropic",
    type_label: "Anthropic",
    enabled: true,
    endpoints: [{ id: "messages", protocol: "anthropic", base_url: "https://api.anthropic.com" }],
    source: { kind: "official", name: "anthropic" },
    circuit: { state: "closed", until: null },
    stats: { success_rate_24h: 0.99, requests_24h: 120, hourly: hourly(1) },
    credentials: [],
    key_optional: false,
    scheduled_test: null,
    quota_probe: null,
    model_sync: null,
    ...p,
  };
}

export function upstreams(): Upstream[] {
  return [
    upstream({
      id: "anthropic",
      name: "Anthropic main",
      credentials: [
        cred({ id: "c1", label: "Team plan", state: { status: "ok", quota: { remaining: 0.6, left: 60, limit: 100, checked_at: soon(-5) } } }),
        cred({ id: "c2", label: "Backup", state: { status: "cooldown", reason: "rate_limit", until: soon(30) } }),
        cred({ id: "c3", label: "Old org", state: { status: "quarantined", reason: "banned" } }),
      ],
    }),
    upstream({
      id: "bedrock",
      name: "Bedrock us",
      type_label: "Amazon Bedrock (Anthropic models)",
      source: { kind: "official", name: "bedrock" },
      circuit: { state: "open", until: soon(2), reason: "failures" },
      credentials: [cred({ id: "c4", upstream_id: "bedrock", label: "AWS prod", state: { status: "circuit_open", until: soon(2), reason: "upstream circuit breaker is open" } })],
    }),
    upstream({
      id: "openai",
      name: "OpenAI",
      provider: "provider.openai",
      type_label: "OpenAI",
      source: { kind: "official", name: "openai" },
      endpoints: [
        { id: "responses", protocol: "openai-responses", base_url: "https://api.openai.com/v1" },
        { id: "chat", protocol: "openai-chat", base_url: "https://api.openai.com/v1" },
        { id: "embeddings", protocol: "openai-embeddings", base_url: "https://api.openai.com/v1" },
      ],
      credentials: [cred({ id: "c5", upstream_id: "openai", label: "Project A" }), cred({ id: "c6", upstream_id: "openai", label: "Bob's", kind: "personal", owner: { id: "u2", name: "Bob" } })],
    }),
  ];
}

export function manifests(): Manifest[] {
  return [
    { name: "anthropic", title: "Anthropic", category: "vendor", kind: "official", key_optional: false, protocols: ["anthropic"], docs_url: "https://docs.claude.com", variables: [] },
    { name: "deepseek", title: "DeepSeek", category: "vendor", kind: "official", key_optional: false, protocols: ["openai-chat", "anthropic"], docs_url: "https://api-docs.deepseek.com", variables: [] },
    { name: "openai", title: "OpenAI", category: "vendor", kind: "official", key_optional: false, protocols: ["openai-responses", "openai-chat"], variables: [] },
    { name: "volcengine-ark", title: "Volcengine Ark", title_zh: "火山方舟", category: "cloud", kind: "official", key_optional: false, protocols: ["openai-chat", "anthropic"], variables: [] },
    {
      name: "azure-openai",
      title: "Azure OpenAI",
      category: "cloud",
      kind: "official", key_optional: false,
      protocols: ["openai-chat"],
      variables: [
        { name: "resource", description: "Azure OpenAI resource name.", description_zh: "Azure OpenAI 资源名。", required: true },
        { name: "api_version", description: "api-version.", description_zh: "api-version。", default: "2024-10-21", required: false },
      ],
    },
    { name: "bedrock", title: "Amazon Bedrock (Anthropic models)", title_zh: "Amazon Bedrock（Anthropic 模型）", category: "cloud", kind: "official", key_optional: false, protocols: ["anthropic"], variables: [] },
    { name: "ollama", title: "Ollama", category: "self_hosted", kind: "official", key_optional: true, protocols: ["openai-chat"], variables: [{ name: "base_url", default: "http://localhost:11434/v1", required: false }] },
    { name: "openrouter", title: "OpenRouter", category: "gateway", kind: "official", key_optional: false, protocols: ["openai-chat"], variables: [] },
    { name: "openai-compatible", title: "Generic OpenAI-compatible service", category: "custom", kind: "official", key_optional: true, protocols: ["openai-chat"], variables: [] },
  ];
}

export function groups(): Group[] {
  return [
    { id: "g1", name: "default", member_count: 3, models: ["claude-haiku-*", "gpt-5.4-mini"] },
    { id: "g2", name: "vip", member_count: 2, models: [] },
  ];
}

export function models(): Model[] {
  return [
    {
      id: "claude-sonnet-4-6",
      name: "claude-sonnet-4-6",
      enabled: true,
      info: { id: "claude-sonnet-4-6", display_name: "Claude Sonnet 4.6", context_window: 1_000_000, tools: true, vision: true, reasoning: true },
      routes: [
        { upstream_id: "anthropic", upstream_model: "claude-sonnet-4-6", priority: 10, weight: 3 },
        { upstream_id: "bedrock", upstream_model: "us.anthropic.claude-sonnet-4-6", priority: 10, weight: 1 },
        { upstream_id: "openai", upstream_model: "gpt-5.4", priority: 0, weight: 1, groups: ["g2"] },
      ],
      synced_at: null,
    },
    { id: "claude-haiku-4-5", name: "claude-haiku-4-5", enabled: true, routes: [{ upstream_id: "anthropic", upstream_model: "claude-haiku-4-5", priority: 10, weight: 1 }], synced_at: soon(-60) },
    { id: "gpt-5.4-mini", name: "gpt-5.4-mini", enabled: false, disabled_reason: "upstream_gone", routes: [], synced_at: null },
    { id: "lonely", name: "lonely", enabled: true, routes: [], synced_at: null },
  ];
}

export function presets(): Preset[] {
  return [
    { name: "best", models: ["claude-sonnet-4-6", "claude-haiku-4-5"], strategy: "in_order" },
    { name: "cheap", models: ["gpt-5.4-mini"], strategy: "cheapest" },
  ];
}

function priceSuggestions(): PriceSuggestion[] {
  return [
    {
      model: "claude-haiku-4-5",
      ref: "anthropic/claude-haiku-4-5",
      prices: { currency: "USD", input: 1_000_000, cache_read: 100_000, cache_write_5m: 1_250_000, cache_write_1h: 2_000_000, output: 5_000_000 },
      current: { currency: "USD", input: 1_000_000, cache_read: 0, cache_write_5m: 0, cache_write_1h: 0, output: 5_000_000 },
      current_rule: "r2",
      follow: false,
      fetched_at: "2026-11-07T06:00:00Z",
      hints: { context_window: 200_000, max_output: 64_000, reasoning: true, vision: true, tools: true },
    },
  ];
}

export function pricingList(): ApiBody<"GET /pricing-rules"> {
  return { items: pricing(), suggestions: priceSuggestions(), sync: { enabled: true, matched: 3, ran_at: "2026-11-07T06:00:00Z" } };
}

export function pricing(): PricingRule[] {
  return [
    { id: "r1", match: "claude-sonnet-4-6", source: "pricesync", follow: true, updated_at: "2026-11-06T06:00:00Z", prices: { currency: "USD", input: 3_750_000, cache_read: 375_000, cache_write_5m: 0, cache_write_1h: 0, output: 18_750_000 } },
    { id: "r2", match: "claude-haiku-*", source: "manual", prices: { currency: "USD", input: 1_000_000, cache_read: 0, cache_write_5m: 0, cache_write_1h: 0, output: 5_000_000 } },
  ];
}
