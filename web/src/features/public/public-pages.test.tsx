import { screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { apiError, renderPage, type ApiBody, type ApiRoutes } from "@/test/api";
import { expectPlainWords } from "@/test/plain-words";
import { LoginPage } from "@/features/auth/login-page";
import { PublicModelsPage, PublicStatusPage } from "./public-pages";

const site = { name: "Acme AI" };
const providers = (on: boolean): ApiBody<"GET /auth/providers"> => ({
  password_login: true,
  providers: [],
  site,
  ...(on ? { public_pages: { models: true, status: true } } : {}),
});
const catalog: ApiBody<"GET /public/catalog"> = {
  site,
  currency: "CNY",
  updated_at: "2026-11-07T08:00:00Z",
  items: [
    {
      model: "claude-sonnet-4-6",
      kind: "model",
      info: { id: "claude-sonnet-4-6", context_window: 1_000_000, reasoning: true, vision: true, tools: true },
      price: { currency: "CNY", input: 21_600_000, cache_read: 2_160_000, cache_write_5m: 0, cache_write_1h: 0, output: 108_000_000 },
    },
  ],
};
const hours = (v: number | null) => Array.from({ length: 24 }, () => v);
const status: ApiBody<"GET /public/status"> = {
  site,
  overall: "degraded",
  updated_at: "2026-11-07T08:00:00Z",
  items: [
    { model: "claude-sonnet-4-6", state: "ok", success_rate_24h: 0.999, hourly: hours(1) },
    { model: "deepseek-v4-flash", state: "degraded", success_rate_24h: 0.9, hourly: hours(0.9) },
  ],
};
const routes = (extra: ApiRoutes = {}): ApiRoutes => ({
  "GET /auth/providers": providers(true),
  "GET /public/catalog": catalog,
  "GET /public/status": status,
  ...extra,
});

describe("public pages", () => {
  it("lists models and prices without signing in, under the relay's name", async () => {
    renderPage(<PublicModelsPage />, { path: "/public/models", me: null, routes: routes() });
    expect(await screen.findByText("claude-sonnet-4-6")).toBeInTheDocument();
    expect(screen.getByText("¥108.00")).toBeInTheDocument();
    expect(await screen.findByText("Acme AI")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Service status" })).toHaveAttribute("href", "/public/status");
    expect(screen.getByRole("link", { name: "Sign in" })).toHaveAttribute("href", "/login");
  });

  it("shows each model's last day", async () => {
    renderPage(<PublicStatusPage />, { path: "/public/status", me: null, routes: routes() });
    expect(await screen.findByText("1 model has trouble")).toBeInTheDocument();
  });

  it("says the page is not public while the feature is off", async () => {
    const off = routes({
      "GET /auth/providers": providers(false),
      "GET /public/catalog": apiError(404, "public_pages_off", "public pages are turned off"),
    });
    renderPage(<PublicModelsPage />, { path: "/public/models", me: null, routes: off });
    expect(await screen.findByText("This page is not public")).toBeInTheDocument();
  });

  it("links the public pages from the login page while they are on", async () => {
    renderPage(<LoginPage />, { path: "/login", me: null, routes: routes() });
    expect(await screen.findByRole("link", { name: "Models & prices" })).toHaveAttribute("href", "/public/models");
    expect(screen.getByRole("link", { name: "Service status" })).toBeInTheDocument();
  });

  it("shows both pages in plain words (zh-CN)", async () => {
    const a = renderPage(<PublicModelsPage />, { path: "/public/models", me: null, routes: routes(), locale: "zh-CN" });
    await screen.findByText("claude-sonnet-4-6");
    expectPlainWords();
    a.unmount();
    renderPage(<PublicStatusPage />, { path: "/public/status", me: null, routes: routes(), locale: "zh-CN" });
    await screen.findByText("deepseek-v4-flash");
    expectPlainWords();
  });
});
