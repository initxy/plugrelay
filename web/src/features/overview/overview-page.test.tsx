import { screen, waitFor, within } from "@testing-library/react";
import { beforeEach, describe, expect, it } from "vitest";
import { TimeRangeProvider } from "@/app/time-range";
import type { Upstream } from "@/lib/api/types";
import { translate } from "@/i18n";
import { memberMe, renderPage, type ApiBody, type ApiRoutes } from "@/test/api";
import { expectPlainWords } from "@/test/plain-words";
import { logSummary } from "../logs/fixtures";
import { OverviewPage } from "./overview-page";
import { attentionItems } from "./upstream-state";

const usd = (micro: number) => ({ micro, currency: "USD" });
const NOW = Date.parse("2026-11-07T12:00:00Z");

function upstream(patch: Partial<Upstream> & { id: string }): Upstream {
  return {
    name: patch.id,
    provider: "provider.anthropic",
    enabled: true,
    endpoints: [],
    source: { kind: "official", name: patch.id } as Upstream["source"],
    circuit: { state: "closed", until: null },
    stats: { success_rate_24h: 0.999, requests_24h: 120, hourly: Array.from({ length: 24 }, () => 1) },
    credentials: [],
    key_optional: true,
    ...patch,
  };
}

function cred(id: string, state: Upstream["credentials"][number]["state"], patch: Partial<Upstream["credentials"][number]> = {}) {
  return {
    id,
    upstream_id: "u",
    label: id,
    kind: "shared" as const,
    secret_hint: "sk-••••",
    enabled: true,
    state,
    ...patch,
  };
}

const upstreams: Upstream[] = [
  upstream({
    id: "anthropic",
    name: "Anthropic",
    credentials: [
      cred("Team plan key", { status: "ok", until: null }),
      cred("Backup key", { status: "cooldown", reason: "rate_limit", until: "2026-11-07T12:30:00Z" }),
      cred("Old org key", { status: "quarantined", reason: "banned", until: null }),
    ],
  }),
  upstream({ id: "bedrock", name: "Bedrock", circuit: { state: "open", until: "2026-11-07T12:02:00Z" } }),
  upstream({
    id: "openai",
    name: "OpenAI",
    credentials: [cred("Project A", { status: "ok", quota: { remaining: 0.1, checked_at: "2026-11-07T11:00:00Z" } })],
  }),
  upstream({ id: "off", name: "Switched off", enabled: false, circuit: { state: "open", until: null } }),
];

const t = (k: Parameters<typeof translate>[1], p?: Record<string, string | number>) => translate("en", k, p);

describe("attentionItems", () => {
  it("lists paused upstreams first, then suspected bans, then cool-downs and low quota; skips switched-off upstreams", () => {
    const items = attentionItems(upstreams, t, "en", NOW);
    expect(items.map((i) => [i.tone, i.upstreamId])).toEqual([
      ["bad", "bedrock"],
      ["violet", "anthropic"],
      ["warn", "anthropic"],
      ["warn", "openai"],
    ]);
    expect(items[0]!.title).toBe("Bedrock is paused, retrying in 2 min.");
    expect(items[0]!.testable).toBe(true);
    expect(items[1]!.title).toBe("Old org key looks suspended and is out of use");
    expect(items[2]!.detail).toBe("Anthropic · rate limited");
    expect(items[3]!.title).toBe("Project A has 10% of its upstream quota left");
  });

  it("is empty when everything runs", () => {
    expect(attentionItems([upstream({ id: "a" })], t, "en", NOW)).toEqual([]);
  });

  it("flags an upstream that is not routed for want of a key", () => {
    const items = attentionItems([upstream({ id: "cloud", key_optional: false }), upstream({ id: "off", key_optional: false, enabled: false })], t, "en", NOW);
    expect(items.map((i) => [i.tone, i.title, i.detail])).toEqual([["bad", "cloud has no API key", "Requests are not sent to it until a key is added."]]);
  });
});

const overview: ApiBody<"GET /overview"> = {
  range: "7d",
  from: "2026-10-31T12:00:00Z",
  to: "2026-11-07T12:00:00Z",
  spend: usd(33_460_000),
  spend_prev: usd(30_770_000),
  requests: 914,
  requests_prev: 855,
  success_rate: 0.972,
  success_rate_prev: 0.974,
  cache_hit_rate: 0.86,
  cache_hit_rate_prev: 0.88,
  cache_saved: usd(66_900_000),
  avg_price_per_mtok: usd(1_120_000),
  ttfb_p95_ms: 2380,
  ttfb_p95_prev_ms: 2425,
};

const series = (key: string, values: number[]) => ({
  key,
  label: key,
  points: values.map((v, i) => ({ t: new Date(NOW - (values.length - i) * 6 * 3600_000).toISOString(), v })),
});

function fixtures(extra: ApiRoutes = {}): ApiRoutes {
  return {
    "GET /overview": overview,
    "GET /usage/timeseries": ({ query }) => ({
      metric: query.get("metric") ?? "spend",
      bucket_seconds: 21600,
      currency: "USD",
      series:
        query.get("group_by") === "none"
          ? [series("all", [1, 2, 3, 4])]
          : query.get("group_by") === "client"
            ? [{ ...series("claude-code", [3_000_000, 4_000_000]), label: "Claude Code" }, { ...series("other", [500_000]), label: "" }]
            : [series("claude-sonnet-4-6", [1_000_000, 2_000_000])],
    }),
    "GET /quota": { windows: [] },
    "GET /upstreams": { items: upstreams },
    "GET /logs": {
      items: [logSummary({ id: "r1" }), logSummary({ id: "r2", status: 429, cause: "rate_limit" })],
      next_cursor: null,
    },
    ...extra,
  };
}

describe("OverviewPage", () => {
  beforeEach(() => localStorage.clear());

  it("shows the headline numbers with their change on the previous period", async () => {
    renderPage(
      <TimeRangeProvider>
        <OverviewPage />
      </TimeRangeProvider>,
      { routes: fixtures() },
    );
    expect(await screen.findByText("$33.46")).toBeInTheDocument();
    expect(screen.getByText("914")).toBeInTheDocument();
    expect(screen.getByText("97.2% succeeded")).toBeInTheDocument();
    expect(screen.getByText("2.4s")).toBeInTheDocument();
    expect(screen.getByText("+8.7%")).toBeInTheDocument();
    expect(screen.getByText("saved $66.90")).toBeInTheDocument();
    expect(screen.getByText("$1.12 per million tokens on average")).toBeInTheDocument();
  });

  it("lists what needs attention for operators and links each item to its upstream", async () => {
    renderPage(
      <TimeRangeProvider>
        <OverviewPage />
      </TimeRangeProvider>,
      { routes: fixtures() },
    );
    const panel = (await screen.findByRole("heading", { name: /Needs attention/ })).closest("section")!;
    await within(panel).findByText("Old org key looks suspended and is out of use");
    const links = within(panel).getAllByRole("link", { name: "Open" });
    expect(links[0]).toHaveAttribute("href", "/upstreams?id=bedrock");
    expect(within(panel).getByRole("button", { name: "Test now" })).toBeInTheDocument();
    const health = screen.getByRole("heading", { name: "Upstream health · last 24 hours" }).closest("section")!;
    expect(await within(health).findByText("Bedrock")).toBeInTheDocument();
  });

  it("splits spend by client and opens a client's requests", async () => {
    const { user, router } = renderPage(
      <TimeRangeProvider>
        <OverviewPage />
      </TimeRangeProvider>,
      { routes: fixtures() },
    );
    const panel = (await screen.findByRole("heading", { name: "Spend by client" })).closest("section")!;
    expect(await within(panel).findByText("$7.00")).toBeInTheDocument();
    expect(within(panel).getByText("Other")).toBeInTheDocument();
    expect(within(panel).getByRole("link", { name: /Usage analytics/ })).toHaveAttribute("href", "/analytics");
    await user.click(within(panel).getByRole("button", { name: /Claude Code/ }));
    await waitFor(() => expect(router.state.location.search).toMatchObject({ client: "claude-code" }));
  });

  it("gives members their own numbers without upstream state, linking requests to their usage page", async () => {
    const { api } = renderPage(
      <TimeRangeProvider>
        <OverviewPage />
      </TimeRangeProvider>,
      { me: memberMe(), routes: fixtures() },
    );
    expect(await screen.findByText("$33.46")).toBeInTheDocument();
    expect(screen.queryByRole("heading", { name: /Needs attention/ })).not.toBeInTheDocument();
    const recent = screen.getByRole("heading", { name: "Recent requests" }).closest("section")!;
    await waitFor(() => expect(within(recent).getAllByRole("link")[1]).toHaveAttribute("href", "/usage?id=r1"));
    expect(api.calls.some((c) => c.path === "/upstreams")).toBe(false);
  });
});

describe("overview in plain words (zh-CN)", () => {
  beforeEach(() => localStorage.clear());
  const quota: ApiRoutes = {
    "GET /quota": {
      windows: [
        {
          id: "key_monthly",
          kind: "monthly",
          scope: "key",
          used: usd(12_000_000),
          limit: usd(50_000_000),
          resets_at: "2026-12-01T00:00:00Z",
        },
        {
          id: "user_daily",
          kind: "daily",
          scope: "user",
          used: usd(1_000_000),
          limit: usd(5_000_000),
          resets_at: "2026-11-08T00:00:00Z",
        },
      ],
    },
  };

  it.each([
    ["an operator", undefined],
    ["a member", memberMe()],
  ])("shows %s nothing raw", async (_, me) => {
    renderPage(
      <TimeRangeProvider>
        <OverviewPage />
      </TimeRangeProvider>,
      { me, routes: fixtures(quota), locale: "zh-CN" },
    );
    expect(await screen.findByText("$33.46")).toBeInTheDocument();
    await waitFor(() => expect(document.querySelector('a[href$="id=r1"]')).not.toBeNull());
    expectPlainWords();
  });
});
