import { screen, within } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { memberMe, renderPage, type ApiBody, type ApiRoutes } from "@/test/api";
import { expectPlainWords } from "@/test/plain-words";
import { StatusPage } from "./status-page";

const member = memberMe();
const hours = (v: number | null) => Array.from({ length: 24 }, () => v);

describe("StatusPage", () => {
  it("puts troubled models first and sums up the state", async () => {
    const r: ApiRoutes = {
      "GET /status": {
        updated_at: "2026-11-07T08:00:00Z",
        items: [
          { model: "a-ok", state: "ok", success_rate_24h: 1, ttfb_p50_ms: 800, hourly: hours(1) },
          { model: "z-down", state: "down", success_rate_24h: 0.2, hourly: hours(0.2) },
          { model: "quiet", state: "ok", success_rate_24h: 1, hourly: hours(null) },
        ],
      },
    };
    renderPage(<StatusPage />, { path: "/status", me: member, routes: r });
    expect(await screen.findByText("1 model has trouble")).toBeInTheDocument();
    const rows = screen.getAllByRole("listitem");
    expect(rows[0]).toHaveTextContent("z-down");
    expect(rows[0]).toHaveTextContent("Down");
    expect(screen.getByText(/first token 800ms/)).toBeInTheDocument();
    expect(within(rows.find((r) => r.textContent?.includes("quiet"))!).getByText("No requests")).toBeInTheDocument();
  });
});

describe("StatusPage in plain words (zh-CN)", () => {
  const status: ApiBody<"GET /status"> = {
    updated_at: "2026-11-07T08:00:00Z",
    items: [
      {
        model: "claude-sonnet-4-6",
        state: "ok",
        success_rate_24h: 0.998,
        ttfb_p50_ms: 800,
        hourly: hours(1),
      },
      { model: "deepseek-v4-flash", state: "degraded", success_rate_24h: 0.91, hourly: hours(0.9) },
    ],
  };

  it("shows the status page without raw ids or keys", async () => {
    renderPage(<StatusPage />, {
      path: "/status",
      me: member,
      routes: { "GET /status": status },
      locale: "zh-CN",
    });
    await screen.findByText("deepseek-v4-flash");
    expectPlainWords();
  });
});
