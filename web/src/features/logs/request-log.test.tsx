import { screen, waitFor, within } from "@testing-library/react";
import { beforeEach, describe, expect, it } from "vitest";
import { translate } from "@/i18n";
import { memberMe, renderPage, type ApiRoutes } from "@/test/api";
import { expectPlainWords } from "@/test/plain-words";
import { retriedDetail, rows, sessions } from "./fixtures";
import { LogActions, RequestLog } from "./request-log";

const histogram = {
  from: "2026-11-06T10:00:00Z",
  to: "2026-11-07T10:00:00Z",
  bucket_seconds: 1800,
  points: [
    { t: "2026-11-07T09:00:00Z", requests: 4, failed: 1 },
    { t: "2026-11-07T09:30:00Z", requests: 0, failed: 0 },
  ],
};

function fixtures(extra: ApiRoutes = {}): ApiRoutes {
  return {
    "GET /logs": { items: rows, next_cursor: null },
    "GET /logs/histogram": histogram,
    "GET /logs/{id}": retriedDetail,
    "GET /keys": { items: [] },
    "GET /catalog": { items: [] },
    "GET /upstreams": { items: [] },
    "GET /clients": {
      items: [
        { id: "claude-code", name: "Claude Code" },
        { id: "codex", name: "Codex" },
        { id: "other", name: "" },
      ],
    },
    "GET /logs/sessions": { items: sessions, next_cursor: null },
    ...extra,
  };
}

describe("RequestLog", () => {
  beforeEach(() => localStorage.clear());

  it("lists requests with handling in plain words, failures and presets", async () => {
    renderPage(<RequestLog admin />, { path: "/logs", routes: fixtures() });
    const table = await screen.findByRole("table");
    await within(table).findByText(/Failed · 503/);
    expect(within(table).getByText("upstream error", { exact: false })).toBeInTheDocument();
    expect(within(table).getByText("retried once")).toBeInTheDocument();
    expect(within(table).getByText("converted")).toBeInTheDocument();
    expect(within(table).getByText("auto-coding →", { exact: false })).toBeInTheDocument();
    expect(within(table).getByText("claude-opus-4-8")).toBeInTheDocument();
    expect(screen.getByText("4 requests")).toBeInTheDocument();
    expect(screen.getByText(/1 failed/)).toBeInTheDocument();
  });

  it("keeps filters in the URL and sends them to the list and the histogram", async () => {
    const { user, router, api } = renderPage(<RequestLog admin />, {
      path: "/logs?tier=convert",
      routes: fixtures(),
    });
    await screen.findByRole("table");
    await waitFor(() => expect(api.lastQuery("/logs")?.tier).toBe("convert"));
    expect(api.lastQuery("/logs/histogram")?.tier).toBe("convert");

    await user.click(screen.getByRole("button", { name: "Failed only" }));
    await waitFor(() => expect(router.state.location.search).toMatchObject({ status: "error", tier: "convert" }));
    await waitFor(() => expect(api.lastQuery("/logs")?.status).toBe("error"));
    expect(api.lastQuery("/logs/histogram")?.status).toBe("error");

    await user.click(screen.getByRole("button", { name: /Clear filters\s*\(\d+\)/ }));
    await waitFor(() => expect(router.state.location.search).toEqual({}));
  });

  it("narrows to a histogram bar's time when it is clicked", async () => {
    const { user, router } = renderPage(<RequestLog admin />, { path: "/logs", routes: fixtures() });
    const bars = await screen.findAllByRole("listitem", { name: /requests/ });
    expect(bars[1]).toBeDisabled();
    await user.click(bars[0]!);
    await waitFor(() =>
      expect(router.state.location.search).toMatchObject({
        from: "2026-11-07T09:00:00Z",
        to: "2026-11-07T09:30:00.000Z",
      }),
    );
  });

  it("shows an empty state that depends on the filters", async () => {
    renderPage(<RequestLog admin />, {
      path: "/logs?model=x",
      routes: fixtures({ "GET /logs": { items: [], next_cursor: null } }),
    });
    expect(await screen.findByText("Nothing matches these filters")).toBeInTheDocument();
  });

  it("hides the upstream filter and column from members", async () => {
    const { api } = renderPage(<RequestLog admin={false} />, {
      path: "/usage",
      me: memberMe(),
      routes: fixtures(),
    });
    const table = await screen.findByRole("table");
    expect(within(table).queryByText("Upstream")).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /All upstreams/ })).not.toBeInTheDocument();
    expect(api.calls.some((c) => c.path === "/upstreams")).toBe(false);
  });
});

describe("clients, sessions and export", () => {
  beforeEach(() => localStorage.clear());

  it("names each request's client and filters by it", async () => {
    const { user, router, api } = renderPage(<RequestLog admin />, { path: "/logs", routes: fixtures() });
    const table = await screen.findByRole("table");
    expect((await within(table).findAllByText("Claude Code")).length).toBe(3);
    await user.click(screen.getByRole("button", { name: /All clients/ }));
    await user.click(await screen.findByRole("menuitemradio", { name: "Codex" }));
    await waitFor(() => expect(router.state.location.search).toMatchObject({ client: "codex" }));
    await waitFor(() => expect(api.lastQuery("/logs")?.client).toBe("codex"));
    await waitFor(() => expect(api.lastQuery("/logs/histogram")?.client).toBe("codex"));
  });

  it("sends every filter but the cause to the histogram", async () => {
    const { api } = renderPage(<RequestLog admin />, {
      path: "/logs?client=codex&cause=upstream&model=m&retried=true",
      routes: fixtures(),
    });
    await screen.findByRole("table");
    await waitFor(() => expect(api.lastQuery("/logs/histogram")).toMatchObject({ client: "codex", model: "m", retried: "true" }));
    expect(api.lastQuery("/logs/histogram")?.cause).toBeUndefined();
  });

  it("groups by session and opens a session's requests", async () => {
    const { user, router, api } = renderPage(
      <>
        <LogActions />
        <RequestLog admin />
      </>,
      { path: "/logs?model=claude-sonnet-4-6", routes: fixtures() },
    );
    await user.click(await screen.findByRole("radio", { name: "By session" }));
    await waitFor(() => expect(router.state.location.search).toMatchObject({ view: "sessions", model: "claude-sonnet-4-6" }));
    const table = await screen.findByRole("table");
    await within(table).findByText("$3.12");
    expect(api.lastQuery("/logs/sessions")?.model).toBe("claude-sonnet-4-6");
    expect(within(table).getByText("laptop-claude (Alice)")).toBeInTheDocument();
    expect(within(table).getByText("+1")).toBeInTheDocument();
    expect(within(table).getByText("1 failed")).toBeInTheDocument();
    expect(within(table).getByText("Other")).toBeInTheDocument();
    await user.click(within(table).getAllByRole("row")[1]!);
    await waitFor(() => expect(router.state.location.search).toEqual({ model: "claude-sonnet-4-6", session_id: "sess-1" }));
  });

  it("says the session view covers the last 30 days unless a time is picked", async () => {
    const { user, router, api } = renderPage(<RequestLog admin />, { path: "/logs?view=sessions", routes: fixtures() });
    await within(await screen.findByRole("table")).findByText("$3.12");
    expect(api.lastQuery("/logs/sessions")?.from).toBeUndefined();
    const histFrom = Date.parse(api.lastQuery("/logs/histogram")!.from!);
    expect(Math.abs(Date.now() - 30 * 86_400_000 - histFrom)).toBeLessThan(5 * 60_000);

    await user.click(screen.getByRole("button", { name: /Last 30 days \(default\)/ }));
    expect(screen.getAllByRole("menuitemradio").map((o) => o.textContent)).toEqual([
      "Last 30 days (default)",
      "Last hour",
      "Last 24 hours",
      "Last 7 days",
      "Last 90 days",
    ]);
    await user.click(screen.getByRole("menuitemradio", { name: "Last 90 days" }));
    await waitFor(() => expect(router.state.location.search).toHaveProperty("from"));
    await waitFor(() => expect(api.lastQuery("/logs/sessions")?.from).toBeDefined());
  });

  it("keeps any time in the list of each request", async () => {
    const { user } = renderPage(<RequestLog admin />, { path: "/logs", routes: fixtures() });
    await screen.findByRole("table");
    await user.click(screen.getByRole("button", { name: /Any time/ }));
    expect(screen.getByRole("menuitemradio", { name: "Last 30 days" })).toBeInTheDocument();
  });

  it("downloads the filtered log as CSV in the console's language", async () => {
    renderPage(<LogActions />, { path: "/logs?status=error&client=codex", routes: fixtures(), locale: "zh-CN" });
    const link = await screen.findByRole("link", { name: /导出 CSV/ });
    const href = new URL(link.getAttribute("href")!, "http://x");
    expect(href.pathname).toBe("/api/v1/logs/export");
    expect(Object.fromEntries(href.searchParams)).toEqual({ status: "error", client: "codex", lang: "zh-CN" });
  });
});

describe("request drawer", () => {
  beforeEach(() => localStorage.clear());

  it("opens from a row and puts the request in the URL", async () => {
    const { user, router } = renderPage(<RequestLog admin />, { path: "/logs", routes: fixtures() });
    const table = await screen.findByRole("table");
    await within(table).findByText(/Failed · 503/);
    await user.click(within(table).getAllByRole("row")[1]!);
    await waitFor(() => expect(router.state.location.search).toMatchObject({ id: "req-retried" }));
    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByText("laptop-claude")).toBeInTheDocument();
    expect(within(dialog).getByText("Claude Code")).toBeInTheDocument();
    expect(within(dialog).getByText("from 10.2.3.18", { exact: false })).toBeInTheDocument();
    expect(within(dialog).getByText("2.4.0")).toBeInTheDocument();
    expect(within(dialog).getByRole("link", { name: "Turn 14 of 20; this session spent $3.12 so far" })).toHaveAttribute(
      "href",
      "/logs?session_id=sess-1",
    );
  });

  it("explains every attempt and the conversion in plain words", async () => {
    const { user } = renderPage(<RequestLog admin />, { path: "/logs?id=req-retried", routes: fixtures() });
    const dialog = await screen.findByRole("dialog");
    await user.click(await within(dialog).findByRole("tab", { name: /Upstream attempts/ }));
    expect(within(dialog).getByText("rate limited (429); moved on to the next one")).toBeInTheDocument();
    expect(within(dialog).getByText("Succeeded: first token after 1.8s, then 5.6s of answer")).toBeInTheDocument();
    const second = within(dialog).getByRole("region", { name: "Attempt 2 · Anthropic" });
    expect(within(second).getByText("Tool namespaces flattened")).toBeInTheDocument();
    expect(within(second).getByText("Parameter frobnicate removed (not supported upstream)")).toBeInTheDocument();
    // An unknown step keeps its text, marked as such, instead of disappearing.
    expect(within(second).getByText("made: up step")).toBeInTheDocument();
    expect(within(second).getByText("Some details lost")).toBeInTheDocument();
    expect(within(second).getByText("Converted from Anthropic Messages to OpenAI Chat")).toBeInTheDocument();
    const first = within(dialog).getByRole("region", { name: "Attempt 1 · Anthropic" });
    expect(within(first).getByText("Too many requests")).toBeInTheDocument();
  });

  it("writes the charge out line by line", async () => {
    const { user } = renderPage(<RequestLog admin />, { path: "/logs?id=req-retried", routes: fixtures() });
    const dialog = await screen.findByRole("dialog");
    await user.click(await within(dialog).findByRole("tab", { name: "Cost" }));
    expect(within(dialog).getByText("1,204 tokens × $21.60 per million")).toBeInTheDocument();
    expect(within(dialog).getByText("$0.302")).toBeInTheDocument();
    expect(within(dialog).getByText("Unit prices include the group's price multiplier ×1.25.")).toBeInTheDocument();
    expect(within(dialog).getByText("1 failed attempt is the operator's cost and not charged.")).toBeInTheDocument();
    expect(within(dialog).getByText("The upstream reported no usage; the token counts are estimates.")).toBeInTheDocument();
  });

  it("says when no content was stored", async () => {
    const { user } = renderPage(<RequestLog admin />, { path: "/logs?id=req-retried", routes: fixtures() });
    const dialog = await screen.findByRole("dialog");
    await user.click(await within(dialog).findByRole("tab", { name: "Content" }));
    expect(within(dialog).getByText("No content stored for this request")).toBeInTheDocument();
  });
});

describe("request log in plain words (zh-CN)", () => {
  beforeEach(() => localStorage.clear());

  it("shows the list and every drawer tab without raw ids or keys", async () => {
    const { user } = renderPage(<RequestLog admin />, {
      path: "/logs?id=req-retried",
      routes: fixtures(),
      locale: "zh-CN",
    });
    const dialog = await screen.findByRole("dialog");
    await within(dialog).findByText("laptop-claude");
    expectPlainWords();
    for (const tab of ["logs.tabAttempts", "logs.tabCost", "logs.tabContent", "logs.tabRaw"] as const) {
      await user.click(within(dialog).getByRole("tab", { name: new RegExp(translate("zh-CN", tab)) }));
      expectPlainWords(dialog);
    }
  });

  it("shows the session view without raw ids or keys", async () => {
    renderPage(
      <>
        <LogActions />
        <RequestLog admin />
      </>,
      { path: "/logs?view=sessions", routes: fixtures(), locale: "zh-CN" },
    );
    await screen.findByText("$3.12");
    expect(screen.getByRole("button", { name: /近 30 天（默认）/ })).toBeInTheDocument();
    expectPlainWords();
  });

  it("names clients in Chinese, falling back to the relay's name for one it doesn't know", async () => {
    const named = [
      { ...sessions[0]!, id: "s-a", client: { id: "playground", name: "Playground" } },
      { ...sessions[0]!, id: "s-b", client: { id: "sdk-python", name: "Python SDK" } },
      { ...sessions[0]!, id: "s-c", client: { id: "brand-new", name: "Brand New" } },
      { ...sessions[1]!, id: "s-d" },
    ];
    const { user } = renderPage(<RequestLog admin />, {
      path: "/logs?view=sessions",
      routes: fixtures({
        "GET /logs/sessions": { items: named, next_cursor: null },
        "GET /clients": { items: [{ id: "playground", name: "Playground" }, { id: "other", name: "" }] },
      }),
      locale: "zh-CN",
    });
    const table = await screen.findByRole("table");
    await within(table).findByText("试用对话");
    expect(within(table).getByText("Python SDK")).toBeInTheDocument();
    expect(within(table).getByText("Brand New")).toBeInTheDocument();
    expect(within(table).getByText("其他")).toBeInTheDocument();
    expect(within(table).queryByText("Playground")).not.toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: /全部客户端/ }));
    expect(await screen.findByRole("menuitemradio", { name: "试用对话" })).toBeInTheDocument();
  });

  it("shows a member's usage page without raw ids or keys", async () => {
    renderPage(<RequestLog admin={false} />, {
      path: "/usage",
      me: memberMe(),
      routes: fixtures(),
      locale: "zh-CN",
    });
    await screen.findByRole("table");
    expectPlainWords();
  });
});
