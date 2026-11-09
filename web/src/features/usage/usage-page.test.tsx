import { screen, waitFor } from "@testing-library/react";
import { beforeEach, describe, expect, it } from "vitest";
import { TimeRangeProvider } from "@/app/time-range";
import { memberMe, renderPage, type ApiRoutes } from "@/test/api";
import { expectPlainWords } from "@/test/plain-words";
import { rows } from "../logs/fixtures";
import { UsagePage } from "./usage-page";

const usd = (micro: number) => ({ micro, currency: "USD" });
const points = (values: number[]) => values.map((v, i) => ({ t: new Date(Date.UTC(2026, 10, 7, i * 6)).toISOString(), v }));

const routes: ApiRoutes = {
  "GET /usage/timeseries": ({ query }) => ({
    metric: query.get("metric") ?? "spend",
    bucket_seconds: 21600,
    currency: query.get("metric") === "spend" ? "USD" : undefined,
    series: [
      { key: "claude-sonnet-4-6", label: "claude-sonnet-4-6", points: points([2_000_000, 1_000_000]) },
      { key: "deepseek-v4-flash", label: "deepseek-v4-flash", points: points([300_000, 100_000]) },
    ],
  }),
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
    ],
  },
  "GET /logs": { items: rows, next_cursor: null },
  "GET /logs/histogram": {
    from: "2026-11-06T10:00:00Z",
    to: "2026-11-07T10:00:00Z",
    bucket_seconds: 1800,
    points: [],
  },
  "GET /keys": { items: [] },
  "GET /catalog": { items: [] },
};

describe("UsagePage", () => {
  beforeEach(() => localStorage.clear());

  it("shows a member's usage, quota and requests in plain words (zh-CN)", async () => {
    renderPage(
      <TimeRangeProvider>
        <UsagePage />
      </TimeRangeProvider>,
      { path: "/usage", me: memberMe(), routes, locale: "zh-CN" },
    );
    await screen.findByRole("table");
    await waitFor(() => expect(screen.getAllByText("claude-sonnet-4-6").length).toBeGreaterThan(0));
    expectPlainWords();
  });

  it("names clients in Chinese when grouped by client", async () => {
    const byClient: ApiRoutes = {
      ...routes,
      "GET /usage/timeseries": ({ query }) => ({
        metric: "spend",
        bucket_seconds: 21600,
        currency: "USD",
        series:
          query.get("group_by") === "client"
            ? [
                { key: "playground", label: "Playground", points: points([2_000_000]) },
                { key: "other", label: "", points: points([100_000]) },
              ]
            : [{ key: "claude-sonnet-4-6", label: "claude-sonnet-4-6", points: points([2_000_000]) }],
      }),
    };
    const { user } = renderPage(
      <TimeRangeProvider>
        <UsagePage />
      </TimeRangeProvider>,
      { path: "/usage", me: memberMe(), routes: byClient, locale: "zh-CN" },
    );
    await user.click(await screen.findByRole("radio", { name: "客户端" }));
    expect((await screen.findAllByText("试用对话")).length).toBeGreaterThan(0);
    expect(screen.getAllByText("其他").length).toBeGreaterThan(0);
    expect(screen.queryByText("Playground")).not.toBeInTheDocument();
  });
});
