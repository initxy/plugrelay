import { screen, within } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { memberMe, renderPage, type ApiBody } from "@/test/api";
import { expectPlainWords } from "@/test/plain-words";
import { CatalogPage } from "./catalog-page";

const member = memberMe();
const price = (input: number, output: number) => ({
  currency: "USD",
  input,
  cache_read: input / 10,
  cache_write_5m: 0,
  cache_write_1h: 0,
  output,
});
const catalog: ApiBody<"GET /catalog"> = {
  items: [
    {
      model: "claude-sonnet-4-6",
      kind: "model",
      info: {
        id: "x",
        display_name: "Claude Sonnet 4.6",
        owned_by: "anthropic",
        context_window: 1_000_000,
        tools: true,
        vision: true,
        reasoning: true,
      },
      price: price(3_000_000, 15_000_000),
    },
    {
      model: "deepseek-v4-flash",
      kind: "model",
      info: {
        id: "y",
        display_name: "DeepSeek V4 Flash",
        owned_by: "deepseek",
        context_window: 128_000,
        tools: true,
        vision: false,
        reasoning: false,
      },
      price: price(300_000, 1_200_000),
    },
    {
      model: "best",
      kind: "preset",
      price: price(0, 0),
      preset_models: ["claude-sonnet-4-6", "deepseek-v4-flash"],
    },
  ],
};

describe("CatalogPage", () => {
  it("shows each model's prices per million tokens and what a preset picks from", async () => {
    renderPage(<CatalogPage />, { path: "/catalog", me: member, routes: { "GET /catalog": catalog } });
    const row = (await screen.findByText("claude-sonnet-4-6")).closest("tr")!;
    expect(within(row).getByText("$3.00")).toBeInTheDocument();
    expect(within(row).getByText("$15.00")).toBeInTheDocument();
    expect(within(row).getByText("$0.30")).toBeInTheDocument();
    expect(within(row).getByRole("img", { name: "Thinks before answering" })).toBeInTheDocument();
    expect(screen.getByText("Picks from claude-sonnet-4-6 → deepseek-v4-flash; charged as the model that answers")).toBeInTheDocument();
    expect(screen.getByText(/Prices per million tokens, in USD/)).toBeInTheDocument();
  });

  it("narrows to models that think, from the URL", async () => {
    renderPage(<CatalogPage />, {
      path: "/catalog?reasoning=true",
      me: member,
      routes: { "GET /catalog": catalog },
    });
    expect(await screen.findByText("claude-sonnet-4-6")).toBeInTheDocument();
    expect(screen.queryByText("deepseek-v4-flash")).not.toBeInTheDocument();
  });

  it("says so when the caller may use no model", async () => {
    renderPage(<CatalogPage />, { path: "/catalog", me: member, routes: { "GET /catalog": { items: [] } } });
    expect(await screen.findByText("No models for you yet")).toBeInTheDocument();
  });
});

describe("CatalogPage in plain words (zh-CN)", () => {
  it("shows the catalog without raw ids or keys", async () => {
    renderPage(<CatalogPage />, {
      path: "/catalog",
      me: member,
      routes: { "GET /catalog": catalog },
      locale: "zh-CN",
    });
    await screen.findByText("claude-sonnet-4-6");
    expectPlainWords();
  });
});
