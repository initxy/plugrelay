import type { Attempt, LogDetail, LogSummary, Session } from "@/lib/api/types";

const usd = (micro: number) => ({ micro, currency: "USD" });
const usage = (input = 0, output = 0, cacheRead = 0) => ({
  input,
  cache_read: cacheRead,
  cache_write_5m: 0,
  cache_write_1h: 0,
  output,
  reasoning: 0,
});

/** Request-log rows for tests: a retried success, a conversion, a failure, a preset. */
export const sessions: Session[] = [
  {
    id: "sess-1",
    first: "2026-11-07T09:10:00Z",
    last: "2026-11-07T10:02:00Z",
    requests: 20,
    failed: 1,
    key: { id: "k1", name: "laptop-claude" },
    user: { id: "u1", name: "Alice" },
    client: { id: "claude-code", name: "Claude Code" },
    models: ["claude-sonnet-4-6", "claude-haiku-4-5"],
    charge: usd(3_120_000),
    cache_hit_rate: 0.81,
  },
  {
    id: "sess-2",
    first: "2026-11-07T08:00:00Z",
    last: "2026-11-07T08:00:00Z",
    requests: 1,
    failed: 0,
    client: { id: "other" },
    models: ["gpt-5.4"],
    charge: usd(10_000),
    cache_hit_rate: null,
  },
];

export function logSummary(patch: Partial<LogSummary> & { id: string }): LogSummary {
  return {
    time: "2026-11-07T10:00:00Z",
    model: "claude-sonnet-4-6",
    served_model: "claude-sonnet-4-6",
    protocol: "anthropic",
    status: 200,
    stream: true,
    attempts: 1,
    charge: usd(68_000),
    ttfb_ms: 900,
    duration_ms: 7400,
    tier: "passthrough",
    cache_hit_rate: 0.74,
    key: { id: "k1", name: "laptop-claude" },
    client: { id: "claude-code", name: "Claude Code", version: "2.4.0" },
    client_ip: "10.2.3.18",
    upstream: { id: "anthropic", name: "Anthropic" },
    ...patch,
  };
}

export const rows: LogSummary[] = [
  logSummary({ id: "req-retried", attempts: 2, cause: "rate_limit" }),
  logSummary({ id: "req-failed", status: 503, cause: "transient", tier: "passthrough", ttfb_ms: null, charge: usd(0), cache_hit_rate: null }),
  logSummary({ id: "req-preset", model: "auto-coding", served_model: "claude-opus-4-8", tier: "convert" }),
];

function attempt(patch: Partial<Attempt> & { n: number }): Attempt {
  return {
    upstream: { id: "anthropic", name: "Anthropic" },
    credential: { id: "c1", name: "Team plan key" },
    protocol: "anthropic",
    upstream_model: "claude-sonnet-4-6",
    tier: "passthrough",
    status: 200,
    sent: true,
    start_ms: 0,
    ttfb_ms: 1800,
    duration_ms: 7400,
    usage: usage(),
    cost: usd(0),
    charged: true,
    ...patch,
  };
}

export const retriedDetail: LogDetail = {
  ...rows[0]!,
  session_id: "sess-1",
  session: { requests: 20, turn: 14, charge: usd(3_120_000), first: "2026-11-07T09:10:00Z", last: "2026-11-07T10:02:00Z" },
  client_ua: "claude-cli/2.4.0 (external, cli)",
  client_ip: "10.2.3.18",
  usage: { ...usage(1204, 1046, 38_410), raw: { input_tokens: 1204 } },
  has_body: false,
  attempts_detail: [
    attempt({ n: 1, credential: { id: "c2", name: "Backup key" }, status: 429, cause: "rate_limit", ttfb_ms: null, duration_ms: 400, charged: false, error: "Too many requests" }),
    attempt({
      n: 2,
      start_ms: 400,
      tier: "convert",
      protocol: "openai-chat",
      diagnostic: { fidelity: "lossy", steps: ["tools: namespaced tools flattened", "params: frobnicate removed", "made: up step"], dropped: ["param:seed"] },
    }),
  ],
  billing: {
    lines: [
      { bucket: "input", tokens: 1204, unit_price: usd(21_600_000), amount: usd(26_000) },
      { bucket: "output", tokens: 1046, unit_price: usd(108_000_000), amount: usd(113_000) },
    ],
    charged: usd(302_000),
    cost: usd(250_000),
    multiplier: 1.25,
    note: "estimated",
  },
};
