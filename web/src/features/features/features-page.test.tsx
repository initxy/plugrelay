import { screen, waitFor, within } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import type { Extension, Feature, Setting } from "@/lib/api/types";
import { apiError, renderPage, type ApiRoutes } from "@/test/api";
import { expectPlainWords } from "@/test/plain-words";
import { FeaturesPage } from "./features-page";

const text = (en: string, zh: string) => ({ en, "zh-CN": zh });

function setting(over: Partial<Setting> & Pick<Setting, "module" | "key">): Setting {
  return {
    type: "int",
    label: text(over.key, over.key),
    help: text("", ""),
    scopes: ["global"],
    merge: "override",
    value: null,
    ...over,
  };
}

function features(): Feature[] {
  return [
    {
      id: "limits",
      name: text("Rate limits & quotas", "限流与额度"),
      description: text("Limits.", "限制每分钟请求数和花费。"),
      enabled: true,
      default: true,
      modules: [{ id: "middleware.ratelimit", status: "running" }],
      pages: [],
      settings: [
        setting({
          module: "middleware.ratelimit",
          key: "rpm",
          label: text("Requests per minute", "每分钟请求数"),
          help: text("0 means no limit.", "0 表示不限。"),
          merge: "strictest",
          default: 0,
        }),
        setting({
          module: "meter.quota",
          key: "key_daily_micro",
          unit: "money",
          label: text("Daily spend per key", "每个密钥每天花费上限"),
          default: 0,
          value: 12.5,
        }),
      ],
    },
    {
      id: "routing",
      name: text("Routing", "路由"),
      description: text("Which upstream goes first.", "决定请求先发给哪个上游。"),
      enabled: true,
      default: true,
      always_on: true,
      modules: [],
      pages: [],
      settings: [
        setting({
          module: "selector.routing",
          key: "strategy",
          type: "string",
          label: text("Routing strategy", "路由策略"),
          enum: ["priority_weight", "cheapest"],
          enum_labels: {
            priority_weight: text("Priority and weight", "按优先级和权重"),
            cheapest: text("Cheapest first", "便宜的优先"),
          },
          default: "priority_weight",
        }),
        setting({
          module: "job.modelsync",
          key: "interval",
          type: "duration",
          label: text("Model sync every", "多久同步一次模型列表"),
          default: "6h0m0s",
          restart: true,
        }),
      ],
    },
    {
      id: "billing",
      name: text("Billing", "计费"),
      description: text("Prepaid balances.", "预付余额、在线充值。"),
      enabled: false,
      default: false,
      modules: [{ id: "meter.balance", status: "stopped" }],
      pages: ["billing.balances", "billing.wallet"],
      settings: [],
    },
    {
      id: "sso",
      name: text("Single sign-on", "单点登录"),
      description: text("Sign in with GitHub.", "用 GitHub 账号登录。"),
      enabled: false,
      default: false,
      modules: [],
      pages: ["sso.providers"],
      settings: [],
    },
  ];
}

const routes = (over: ApiRoutes = {}): ApiRoutes => ({
  "GET /features": { items: features() },
  "GET /extensions": { items: [] },
  ...over,
});

describe("FeaturesPage", () => {
  it("shows features as cards in plain words, grouped, with what they add", async () => {
    renderPage(<FeaturesPage />, { locale: "zh-CN", routes: routes(), path: "/features" });
    expect(await screen.findByRole("heading", { name: "限流与额度" })).toBeInTheDocument();
    expect(screen.getByRole("heading", { name: "常用" })).toBeInTheDocument();
    expect(screen.getByRole("heading", { name: "对外运营" })).toBeInTheDocument();
    const billing = screen.getByRole("article", { name: "计费" });
    expect(within(billing).getByText("余额管理")).toBeInTheDocument();
    expect(within(billing).getByText("钱包")).toBeInTheDocument();
    expect(within(billing).getByText("未开启")).toBeInTheDocument();
    // Always-on features have no switch.
    expect(within(screen.getByRole("article", { name: "路由" })).queryByRole("switch")).not.toBeInTheDocument();
    expectPlainWords();
  });

  it("turns a feature on and explains a refusal", async () => {
    const { user, api } = renderPage(<FeaturesPage />, {
      locale: "zh-CN",
      routes: routes({
        "PATCH /features/{id}": ({ params }) =>
          params.id === "billing"
            ? apiError(409, "change_cannot_run", "this change cannot run: the request log is off", { params: { reason: "the request log is off" } })
            : { ...features()[3]!, enabled: true },
      }),
    });
    await user.click(await screen.findByRole("switch", { name: "开关「计费」" }));
    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent("没能打开「计费」");
    expect(alert).toHaveTextContent("它依赖的部分没开或者跑不起来");
    expect(api.calls.find((c) => c.method === "PATCH")?.body).toEqual({ enabled: true });

    await user.click(screen.getByRole("switch", { name: "开关「单点登录」" }));
    await waitFor(() => expect(api.calls.filter((c) => c.method === "PATCH" && c.params.id === "sso")).toHaveLength(1));
  });

  it("changes the routing strategy right on its card", async () => {
    const { user, api } = renderPage(<FeaturesPage />, {
      locale: "zh-CN",
      routes: routes({ "PATCH /features/{id}": features()[1] }),
    });
    await user.click(await screen.findByRole("radio", { name: /便宜的优先/ }));
    await waitFor(() =>
      expect(api.calls.find((c) => c.method === "PATCH")?.body).toEqual({
        settings: { "selector.routing": { strategy: "cheapest" } },
      }),
    );
  });

  it("shows the last failed reload", async () => {
    renderPage(<FeaturesPage />, {
      locale: "zh-CN",
      routes: routes({
        "GET /features": { items: features(), reload_error: 'plugin "middleware.redact": invalid pattern' },
      }),
    });
    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent("最近一次改动没能生效");
    expect(alert).toHaveTextContent("invalid pattern");
  });

  it("opens a feature's settings from the URL and saves money in currency units", async () => {
    const { user, api } = renderPage(<FeaturesPage />, {
      locale: "zh-CN",
      path: "/features?id=limits",
      routes: routes({ "PATCH /features/{id}": features()[0] }),
    });
    const drawer = await screen.findByRole("dialog");
    expect(within(drawer).getByRole("heading", { name: "限流" })).toBeInTheDocument();
    const money = within(drawer).getByLabelText("每个密钥每天花费上限");
    expect(money).toHaveValue("12.5");
    // The site layer has nothing above it to tighten.
    expect(within(drawer).queryByText("只能收紧")).not.toBeInTheDocument();
    await user.clear(money);
    await user.type(money, "20");
    await user.click(within(drawer).getByRole("button", { name: "保存" }));
    await waitFor(() =>
      expect(api.calls.find((c) => c.method === "PATCH")?.body).toEqual({
        settings: { "middleware.ratelimit": { rpm: null }, "meter.quota": { key_daily_micro: 20 } },
      }),
    );
    expectPlainWords(drawer);
  });

  it("shows durations as a number and a unit, and which settings need a restart", async () => {
    renderPage(<FeaturesPage />, { locale: "zh-CN", path: "/features?id=routing", routes: routes() });
    const drawer = await screen.findByRole("dialog");
    expect(within(drawer).getByText("重启后生效")).toBeInTheDocument();
    expect(within(drawer).getByText(/默认：6 小时/)).toBeInTheDocument();
    expectPlainWords(drawer);
  });

  it.each(["billing", "sso"])("opens the %s drawer without raw ids or keys", async (id) => {
    renderPage(<FeaturesPage />, { locale: "zh-CN", path: `/features?id=${id}`, routes: routes() });
    expectPlainWords(await screen.findByRole("dialog"));
  });
});

function extension(over: Partial<Extension> = {}): Extension {
  return {
    id: "acme-guard",
    kind: "middleware",
    version: "1.2.0",
    title: "Acme guard",
    description: "Checks prompts against Acme's rules.",
    source: "external",
    enabled: false,
    order: 0,
    status: "needs_approval",
    permissions: [{ id: "p1", api: "host.http", reason: "calls the Acme API" }],
    approved_permissions: [],
    settings: [],
    ...over,
  };
}

describe("Extensions", () => {
  it("explains how to install one when there are none", async () => {
    renderPage(<FeaturesPage />, { locale: "zh-CN", path: "/features?tab=extensions", routes: routes() });
    expect(await screen.findByText("还没有安装扩展")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: /怎么写一个扩展/ })).toHaveAttribute("href", expect.stringContaining("docs/plugins.md"));
  });

  it("asks to approve what an extension may do before turning it on", async () => {
    const { user, api } = renderPage(<FeaturesPage />, {
      locale: "zh-CN",
      path: "/features?tab=extensions",
      routes: routes({
        "GET /extensions": { items: [extension()] },
        "PATCH /extensions/{id}": extension({ enabled: true, status: "running" }),
      }),
    });
    expect(await screen.findByText("待批准")).toBeInTheDocument();
    expect(screen.getByText("版本 1.2.0")).toBeInTheDocument();
    expectPlainWords();
    await user.click(screen.getByRole("switch", { name: "开关「Acme guard」" }));
    const dialog = await screen.findByRole("dialog", { name: "启用 Acme guard？" });
    expect(dialog).toHaveTextContent("向其他服务发网络请求");
    expectPlainWords(dialog);
    await user.click(within(dialog).getByRole("button", { name: /确认并启用/ }));
    await waitFor(() =>
      expect(api.calls.find((c) => c.method === "PATCH")?.body).toEqual({
        enabled: true,
        approved_permissions: ["p1"],
      }),
    );
  });
});
