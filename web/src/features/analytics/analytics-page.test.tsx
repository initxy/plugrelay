import { screen, waitFor, within } from "@testing-library/react";
import { beforeEach, describe, expect, it } from "vitest";
import { TimeRangeProvider } from "@/app/time-range";
import { renderPage, type ApiBody, type ApiRoutes } from "@/test/api";
import { expectPlainWords } from "@/test/plain-words";
import { AnalyticsPage } from "./analytics-page";
import { heatGrid } from "./heatmap";

const usd = (micro: number) => ({ micro, currency: "USD" });
// A Monday 09:00 and 10:00 local time, then a Wednesday 15:00.
const mon9 = new Date(2026, 10, 2, 9).toISOString();
const mon10 = new Date(2026, 10, 2, 10).toISOString();
const wed15 = new Date(2026, 10, 4, 15).toISOString();

const top = (by: ApiBody<"GET /usage/top">["by"]): ApiBody<"GET /usage/top"> => ({
  range: "7d",
  from: "2026-10-31T12:00:00Z",
  to: "2026-11-07T12:00:00Z",
  by,
  items:
    by === "client"
      ? [
          { key: "claude-code", label: "Claude Code", requests: 120, errors: 2, spend: usd(9_000_000), tokens: 2_400_000, cache_hit_rate: 0.8, ttfb_p95_ms: 1800 },
          { key: "other", label: "", requests: 3, errors: 0, spend: usd(20_000), tokens: 900, cache_hit_rate: null, ttfb_p95_ms: null },
        ]
      : [
          { key: "claude-sonnet-4-6", label: "claude-sonnet-4-6", requests: 100, errors: 10, spend: usd(8_000_000), tokens: 2_000_000, cache_hit_rate: 0.7, ttfb_p95_ms: 2100 },
          { key: "gpt-5.4", label: "gpt-5.4", requests: 40, errors: 0, spend: usd(1_000_000), tokens: 300_000, cache_hit_rate: 0.2, ttfb_p95_ms: 900 },
        ],
});

function fixtures(extra: ApiRoutes = {}): ApiRoutes {
  return {
    "GET /usage/timeseries": ({ query }) =>
      query.get("bucket") === "hour"
        ? {
            metric: query.get("metric") ?? "requests",
            bucket_seconds: 3600,
            series: [{ key: "all", label: "all", points: [{ t: mon9, v: 5 }, { t: mon10, v: 20 }, { t: wed15, v: 2 }] }],
          }
        : {
            metric: "spend",
            bucket_seconds: 21600,
            currency: "USD",
            series: [
              { key: "claude-code", label: "Claude Code", points: [{ t: mon9, v: 9_000_000 }] },
              { key: "codex", label: "Codex", points: [{ t: mon9, v: 2_000_000 }] },
            ],
          },
    "GET /usage/failures": {
      range: "7d",
      bucket_seconds: 21600,
      series: [
        { cause: "rate_limit", total: 9, points: [{ t: mon9, v: 9 }] },
        { cause: "not_sent", total: 3, points: [{ t: mon9, v: 3 }] },
      ],
    },
    "GET /usage/top": ({ query }) => top((query.get("by") ?? "model") as "model"),
    ...extra,
  };
}

const page = (
  <TimeRangeProvider>
    <AnalyticsPage />
  </TimeRangeProvider>
);

describe("heatGrid", () => {
  it("sums hourly points into local weekday × hour cells", () => {
    const g = heatGrid([
      { t: mon9, v: 5 },
      { t: mon9, v: 1 },
      { t: wed15, v: 2 },
    ]);
    expect(g[1]![9]).toBe(6);
    expect(g[3]![15]).toBe(2);
    expect(g.flat().reduce((a, v) => a + v, 0)).toBe(8);
  });
});

describe("AnalyticsPage", () => {
  beforeEach(() => localStorage.clear());

  it("shows busy hours, failures by cause, spend by client and the top table", async () => {
    const { user, router, api } = renderPage(page, { routes: fixtures() });
    const heat = (await screen.findByRole("heading", { name: "Busy hours" })).closest("section")!;
    expect(await within(heat).findByLabelText(/10:00 – 10:59: 20$/)).toBeInTheDocument();
    expect(api.lastQuery("/usage/timeseries")).toBeDefined();
    const failures = screen.getByRole("heading", { name: "Failures by cause" }).closest("section")!;
    expect(await within(failures).findByRole("link", { name: "12 failed requests" })).toBeInTheDocument();
    const clients = screen.getByRole("heading", { name: "Spend by client" }).closest("section")!;
    expect(await within(clients).findByText("$9.00")).toBeInTheDocument();
    expect(within(clients).queryByRole("link", { name: /Usage analytics/ })).not.toBeInTheDocument();

    const topPanel = screen.getByRole("heading", { name: "Who uses what" }).closest("section")!;
    expect(await within(topPanel).findByText("claude-sonnet-4-6")).toBeInTheDocument();
    expect(within(topPanel).getByText("10%")).toBeInTheDocument();
    expect(within(topPanel).getByText("2.1s")).toBeInTheDocument();
    await user.click(within(topPanel).getByRole("radio", { name: "Clients" }));
    expect(await within(topPanel).findByText("Other")).toBeInTheDocument();
    await waitFor(() => expect(api.lastQuery("/usage/top")?.by).toBe("client"));
    await user.click(within(topPanel).getAllByRole("row")[1]!);
    await waitFor(() => expect(router.state.location.search).toMatchObject({ client: "claude-code" }));
  });

  it("says when nothing failed", async () => {
    renderPage(page, { routes: fixtures({ "GET /usage/failures": { range: "7d", bucket_seconds: 21600, series: [] } }) });
    expect(await screen.findByText("No failures in this range")).toBeInTheDocument();
  });

  it("shows nothing raw in zh-CN", async () => {
    renderPage(page, { routes: fixtures(), locale: "zh-CN" });
    await screen.findByText("claude-sonnet-4-6");
    await screen.findByText("$9.00");
    expectPlainWords();
  });

  it("names clients in Chinese in spend by client", async () => {
    const spend = {
      metric: "spend",
      bucket_seconds: 21600,
      currency: "USD",
      series: [
        { key: "playground", label: "Playground", points: [{ t: mon9, v: 9_000_000 }] },
        { key: "other", label: "", points: [{ t: mon9, v: 2_000_000 }] },
      ],
    };
    renderPage(page, { routes: fixtures({ "GET /usage/timeseries": ({ query }) => (query.get("bucket") === "hour" ? { metric: "requests", bucket_seconds: 3600, series: [] } : spend) }), locale: "zh-CN" });
    const clients = (await screen.findByRole("heading", { name: "按客户端看花费" })).closest("section")!;
    expect(await within(clients).findByText("试用对话")).toBeInTheDocument();
    expect(within(clients).getByText("其他")).toBeInTheDocument();
    expect(within(clients).queryByText("Playground")).not.toBeInTheDocument();
  });
});
