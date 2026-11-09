import { screen, waitFor, within } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { renderPage, reply, type ApiRoutes } from "@/test/api";
import * as fx from "@/features/upstreams/upstreams.fixtures";
import { expectPlainWords } from "@/test/plain-words";
import { globMatch, priceFor } from "./lookup";
import { ModelsPage } from "./models-page";
import { presetAudience } from "./presets-tab";
import { parseDiscountPercent } from "./pricing-tab";
import { routeShares } from "./route-bars";

function fixtures(extra: ApiRoutes = {}): ApiRoutes {
  return {
    "GET /models": { items: fx.models() },
    "GET /presets": { items: fx.presets() },
    "GET /pricing-rules": fx.pricingList(),
    "GET /upstreams": { items: fx.upstreams() },
    "GET /groups": { items: fx.groups() },
    ...extra,
  };
}

/** The drawer once its body (not the loading shell) is up. */
async function drawer(name: string | RegExp) {
  await screen.findByRole("heading", { name });
  return screen.getByRole("dialog");
}

describe("models tab", () => {
  it("draws routes as first choice and fallback shares, with prices and plain states", async () => {
    renderPage(<ModelsPage />, { path: "/models", routes: fixtures() });
    const row = (await screen.findByText("claude-sonnet-4-6")).closest("tr")!;
    expect(within(row).getAllByText("First choice · 75%")).toHaveLength(1);
    expect(within(row).getByText("First choice · 25%")).toBeInTheDocument();
    expect(within(row).getByText("Fallback 1 · 100%")).toBeInTheDocument();
    expect(within(row).getByText("Only vip")).toBeInTheDocument();
    expect(within(row).getByText("Anthropic main")).toBeInTheDocument();
    expect(row.textContent).toContain("$3.75/$18.75");
    expect(within((await screen.findByText("gpt-5.4-mini")).closest("tr")!).getByText("Gone from upstream")).toBeInTheDocument();
    expect(within(screen.getByText("lonely").closest("tr")!).getAllByText(/No routes/).length).toBeGreaterThan(0);
    expect(document.body.textContent).not.toMatch(/p10|w1\b/);
  });

  it("filters models by upstream through the URL", async () => {
    const { user, router } = renderPage(<ModelsPage />, { path: "/models", routes: fixtures() });
    await screen.findByText("claude-haiku-4-5");
    await user.click(screen.getByRole("button", { name: "All upstreams" }));
    await user.click(await screen.findByRole("menuitemradio", { name: "OpenAI" }));
    await waitFor(() => expect(screen.queryByText("claude-haiku-4-5")).not.toBeInTheDocument());
    expect(router.state.location.search).toMatchObject({ upstream: "openai" });
  });

  it("adds a route and saves the whole route list", async () => {
    const r = fixtures({ "PATCH /models/{id}": (c) => ({ ...fx.models()[1]!, ...(c.body as object) }) });
    const { user, api: f } = renderPage(<ModelsPage />, { path: "/models?id=claude-haiku-4-5", routes: r });
    const d = await drawer("claude-haiku-4-5");
    await user.click(within(d).getByRole("button", { name: "Add route" }));
    const routes = within(within(d).getAllByRole("list")[0]!).getAllByRole("listitem");
    const added = routes[routes.length - 1]!;
    await user.click(within(added).getByRole("combobox", { name: "Upstream" }));
    await user.click(await screen.findByRole("option", { name: "OpenAI" }));
    await user.click(within(added).getByRole("button", { name: "vip" }));
    await user.click(within(d).getByRole("button", { name: "Save" }));
    await waitFor(() => expect(f.calls.find((c) => c.method === "PATCH")).toBeDefined());
    expect(f.calls.find((c) => c.method === "PATCH")!.body).toEqual({
      enabled: true,
      routes: [
        { upstream_id: "anthropic", upstream_model: "claude-haiku-4-5", priority: 10, weight: 1, groups: [] },
        {
          upstream_id: "openai",
          upstream_model: "claude-haiku-4-5",
          priority: 10,
          weight: 1,
          groups: ["g2"],
        },
      ],
    });
  });

  it("deletes a model after confirming", async () => {
    const r = fixtures({ "DELETE /models/{id}": undefined });
    const { user, api: f } = renderPage(<ModelsPage />, { path: "/models?id=lonely", routes: r });
    const d = await drawer("lonely");
    await user.click(within(d).getByRole("button", { name: "More actions" }));
    await user.click(await screen.findByRole("menuitem", { name: /Delete model/ }));
    await user.click(await screen.findByRole("button", { name: "Delete" }));
    await waitFor(() => expect(f.calls.some((c) => c.method === "DELETE" && c.path === "/models/lonely")).toBe(true));
  });
});

describe("presets tab", () => {
  it("says who sees each preset and creates one with ordered models", async () => {
    const r = fixtures({ "POST /presets": (c) => reply(201, c.body) });
    const { user, api: f } = renderPage(<ModelsPage />, { path: "/models?tab=presets", routes: r });
    expect(await screen.findByText("best")).toBeInTheDocument();
    expect(within(screen.getByText("best").closest("tr")!).getByText("Everyone")).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "New preset" }));
    const d = await drawer("New preset");
    await user.type(within(d).getByLabelText("Name"), "fast");
    await user.click(within(d).getByRole("radio", { name: "Fastest" }));
    for (const m of ["claude-haiku-4-5", "claude-sonnet-4-6"]) {
      await user.click(within(d).getByRole("combobox", { name: "Add a model" }));
      await user.click(await screen.findByRole("option", { name: m }));
      await user.click(within(d).getByRole("button", { name: "Add" }));
    }
    await user.click(within(d).getAllByRole("button", { name: "Move up" })[1]!);
    await user.click(within(d).getByRole("button", { name: "Create preset" }));
    await waitFor(() =>
      expect(f.calls.find((c) => c.method === "POST")?.body).toEqual({
        name: "fast",
        models: ["claude-sonnet-4-6", "claude-haiku-4-5"],
        strategy: "fastest",
      }),
    );
  });
});

describe("prices tab", () => {
  it("edits prices in currency units and stores micro-units", async () => {
    const r = fixtures({
      "PATCH /pricing-rules/{id}": (c) => ({ ...fx.pricing()[0]!, ...(c.body as object) }),
    });
    const { user, api: f } = renderPage(<ModelsPage />, { path: "/models?tab=pricing&id=r1", routes: r });
    const d = await drawer("claude-sonnet-4-6");
    const input = within(d).getByLabelText("Input");
    await user.clear(input);
    await user.type(input, "4.2");
    await user.type(within(d).getByLabelText(/Batch discount/), "50");
    await user.click(within(d).getByRole("button", { name: "Save" }));
    await waitFor(() => expect(f.calls.find((c) => c.method === "PATCH")).toBeDefined());
    expect(f.calls.find((c) => c.method === "PATCH")!.body).toEqual({
      match: "claude-sonnet-4-6",
      prices: {
        currency: "USD",
        input: 4_200_000,
        cache_read: 375_000,
        cache_write_5m: 0,
        cache_write_1h: 0,
        output: 18_750_000,
        batch_discount: 0.5,
      },
    });
  });
});

describe("price suggestions", () => {
  it("accepts, dismisses and follows a models.dev price", async () => {
    const r = fixtures({
      "POST /price-sync/{model}/accept": () => fx.pricing()[1]!,
      "POST /price-sync/{model}/reject": undefined,
      "PATCH /price-sync/{model}": undefined,
    });
    const { user, api: f } = renderPage(<ModelsPage />, { path: "/models?tab=pricing", routes: r });
    const list = await screen.findByRole("region", { name: "Price suggestions" });
    expect(within(list).getByText("claude-haiku-4-5")).toBeInTheDocument();
    expect(list.textContent).toContain("context 200K");
    await user.click(within(list).getByRole("button", { name: "Accept" }));
    await waitFor(() => expect(f.called("POST /price-sync/{model}/accept")).toHaveLength(1));
    expect(f.called("POST /price-sync/{model}/accept")[0]!.params).toEqual({ model: "claude-haiku-4-5" });
    await user.click(within(list).getByRole("button", { name: "Dismiss" }));
    await waitFor(() => expect(f.called("POST /price-sync/{model}/reject")).toHaveLength(1));
    await user.click(within(list).getByRole("switch", { name: "Follow models.dev" }));
    await waitFor(() => expect(f.called("PATCH /price-sync/{model}")[0]?.body).toEqual({ follow: true }));
    expect(screen.getByText(/Last checked models.dev/)).toBeInTheDocument();
    expect(screen.getByText("Follows models.dev")).toBeInTheDocument();
  });

  it("says how to turn price sync on while it is off", async () => {
    const off = { ...fx.pricingList(), suggestions: [], sync: { enabled: false, matched: 0 } };
    renderPage(<ModelsPage />, { path: "/models?tab=pricing", routes: fixtures({ "GET /pricing-rules": off }) });
    expect(await screen.findByRole("link", { name: /Turn on price sync/ })).toHaveAttribute("href", "/features?id=price_sync");
    expect(screen.queryByRole("region", { name: "Price suggestions" })).not.toBeInTheDocument();
  });

  it("switches following off in the drawer of a synced price", async () => {
    const { user, api: f } = renderPage(<ModelsPage />, { path: "/models?tab=pricing&id=r1", routes: fixtures({ "PATCH /price-sync/{model}": undefined }) });
    const d = await drawer("claude-sonnet-4-6");
    await user.click(within(d).getByRole("switch", { name: /Follow models.dev/ }));
    await waitFor(() => expect(f.called("PATCH /price-sync/{model}")[0]?.body).toEqual({ follow: false }));
  });
});

describe("helpers", () => {
  it("splits sessions by weight within a priority and orders fallbacks", () => {
    const s = routeShares(fx.models()[0]!.routes);
    expect(s.map((x) => [x.route.upstream_id, x.tier, x.share])).toEqual([
      ["anthropic", 0, 0.75],
      ["bedrock", 0, 0.25],
      ["openai", 1, 1],
    ]);
  });
  it("matches prices like the relay: exact name first, then the longest pattern", () => {
    expect(globMatch("claude-*", "claude-haiku-4-5")).toBe(true);
    expect(globMatch("claude-?", "claude-ab")).toBe(false);
    expect(priceFor(fx.pricing(), "claude-haiku-4-5")?.id).toBe("r2");
    expect(priceFor(fx.pricing(), "gpt-5.4")).toBeUndefined();
  });
  it("counts a group as an audience when it may use any of the preset's models", () => {
    expect(presetAudience({ models: ["gpt-5.4-mini"] }, fx.groups()).map((g) => g.name)).toEqual(["default", "vip"]);
    expect(presetAudience({ models: ["claude-sonnet-4-6"] }, fx.groups()).map((g) => g.name)).toEqual(["vip"]);
  });
  it("reads a batch discount as percent off", () => {
    expect(parseDiscountPercent("50")).toBe(0.5);
    expect(parseDiscountPercent("12,5%")).toBe(0.125);
    expect(parseDiscountPercent("100")).toBeNull();
  });
});

describe("models, presets and prices in plain words (zh-CN)", () => {
  it.each([
    ["models", "claude-sonnet-4-6"],
    ["presets", "best"],
    ["pricing", "claude-sonnet-4-6"],
  ])("shows the %s tab without raw ids or keys", async (tab, text) => {
    renderPage(<ModelsPage />, { path: `/models?tab=${tab}`, routes: fixtures(), locale: "zh-CN" });
    expect((await screen.findAllByText(text)).length).toBeGreaterThan(0);
    expectPlainWords();
  });

  it.each([
    ["/models?id=claude-sonnet-4-6", "claude-sonnet-4-6"],
    ["/models?id=lonely", "lonely"],
    ["/models?tab=presets&id=best", "best"],
    ["/models?tab=pricing&id=r1", "claude-sonnet-4-6"],
  ])("shows the drawer at %s without raw ids or keys", async (path, name) => {
    renderPage(<ModelsPage />, { path, routes: fixtures(), locale: "zh-CN" });
    expectPlainWords(await drawer(name));
  });
});
