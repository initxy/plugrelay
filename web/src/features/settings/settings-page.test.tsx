import { screen, waitFor, within } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import type { SettingItem } from "@/lib/api/types";
import { apiError, adminMe, renderPage } from "@/test/api";
import { expectPlainWords } from "@/test/plain-words";
import { SECTIONS } from "./layout";
import { SettingsPage } from "./settings-page";

const text = (en: string, zh: string) => ({ en, "zh-CN": zh });

function item(over: Partial<SettingItem> & Pick<SettingItem, "key" | "section">): SettingItem {
  return {
    type: "string",
    value: "",
    apply: "live",
    source: "default",
    title: text(over.key, over.key),
    help: text("", ""),
    ...over,
  };
}

const items = (): SettingItem[] => [
  item({
    key: "site.name",
    section: "branding",
    value: "plugrelay",
    title: text("Site name", "站点名称"),
    help: text("Shown on the console.", "显示在控制台和登录页上。"),
  }),
  item({ key: "site.currency", section: "branding", value: "USD", title: text("Currency", "币种") }),
  item({ key: "site.public_base_url", section: "general", title: text("Public address", "对外地址") }),
  item({
    key: "listen",
    section: "network",
    apply: "file",
    source: "file",
    value: "127.0.0.1:8080",
    title: text("Listen address", "监听地址"),
    help: text("Set in relay.yaml.", "在 relay.yaml 里设置。"),
  }),
  item({
    key: "network.listen_public",
    section: "network",
    type: "bool",
    apply: "restart",
    value: false,
    title: text("Listen on all interfaces", "对外监听所有网卡"),
  }),
  item({
    key: "store.driver",
    section: "network",
    apply: "file",
    source: "file",
    value: "sqlite",
    title: text("Database", "数据库"),
  }),
  item({
    key: "security.require_2fa_admins",
    section: "security",
    type: "bool",
    value: false,
    title: text("Admins must use two-step sign-in", "管理员必须开启两步验证"),
  }),
  item({
    key: "security.require_2fa_grace_days",
    section: "security",
    type: "int",
    value: 7,
    title: text("Grace days", "宽限天数"),
  }),
  item({
    key: "security.admin_token",
    section: "security",
    type: "secret",
    value: null,
    source: "database",
    title: text("Admin API token", "管理 API 令牌"),
  }),
];

describe("SettingsPage", () => {
  it("shows one section at a time in plain words, from the URL", async () => {
    renderPage(<SettingsPage />, {
      locale: "zh-CN",
      routes: { "GET /settings": { items: items() } },
      path: "/settings?section=network",
    });
    expect(await screen.findByText("监听地址")).toBeInTheDocument();
    expect(screen.queryByText("站点名称")).not.toBeInTheDocument();
    // Restart only where it is true; config-file values are read-only text.
    expect(screen.getAllByText("重启后生效")).toHaveLength(1);
    expect(screen.getByText("SQLite")).toBeInTheDocument();
    expect(screen.getAllByText("只读")).toHaveLength(2);
    expect(screen.getByRole("button", { name: /网络与存储/ })).toHaveAttribute("aria-current", "page");
    expectPlainWords();
  });

  it("saves edits with a bar that counts them, and refreshes the site name", async () => {
    const { user, api } = renderPage(<SettingsPage />, {
      locale: "zh-CN",
      routes: {
        "GET /settings": { items: items() },
        "PATCH /settings": { items: items(), restart_required: false },
      },
    });
    const name = await screen.findByLabelText("站点名称");
    await user.clear(name);
    await user.type(name, "Acme relay");
    expect(screen.getByText("有 1 处修改没保存")).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "保存" }));
    await waitFor(() =>
      expect(api.calls.find((c) => c.method === "PATCH")?.body).toEqual({
        values: { "site.name": "Acme relay" },
      }),
    );
    expect(await screen.findByText("设置已保存并生效")).toBeInTheDocument();
    expectPlainWords();
  });

  it("keeps a setting that depends on another off until that one is on", async () => {
    const { user } = renderPage(<SettingsPage />, {
      locale: "zh-CN",
      routes: { "GET /settings": { items: items() } },
      path: "/settings?section=security",
    });
    const grace = await screen.findByLabelText("宽限天数");
    expect(grace).toBeDisabled();
    await user.click(screen.getByRole("switch", { name: "管理员必须开启两步验证" }));
    expect(grace).toBeEnabled();
    expect(screen.getByLabelText("管理 API 令牌")).toHaveAttribute("placeholder", "已保存 · 输入新值即替换");
  });

  it("confirms a currency change while prepaid balances exist", async () => {
    let tries = 0;
    const { user, api } = renderPage(<SettingsPage />, {
      locale: "en",
      routes: {
        "GET /settings": { items: items() },
        "PATCH /settings": () => (++tries === 1 ? apiError(409, "balances_exist", "prepaid balances exist") : { items: items(), restart_required: false }),
      },
    });
    await user.click(await screen.findByLabelText("Currency"));
    await user.click(await screen.findByRole("option", { name: /EUR/ }));
    await user.click(screen.getByRole("button", { name: "Save" }));
    const confirm = await screen.findByRole("alertdialog");
    expect(confirm).toHaveTextContent("Prepaid balances exist");
    await user.click(within(confirm).getByRole("button", { name: "Change currency" }));
    await waitFor(() =>
      expect(api.calls.filter((c) => c.method === "PATCH").at(-1)?.body).toEqual({
        values: { "site.currency": "EUR" },
        confirm_currency_change: true,
      }),
    );
  });

  it("links to the sign-in providers page only while single sign-on is on", async () => {
    const { unmount } = renderPage(<SettingsPage />, {
      locale: "zh-CN",
      routes: { "GET /settings": { items: items() } },
    });
    await screen.findByText("站点名称");
    expect(screen.queryByRole("link", { name: /登录方式/ })).not.toBeInTheDocument();
    unmount();
    renderPage(<SettingsPage />, {
      locale: "zh-CN",
      routes: { "GET /settings": { items: items() } },
      me: adminMe({ pages: ["sso.providers"] }),
    });
    await screen.findByText("站点名称");
    expect(screen.getByRole("link", { name: /登录方式/ })).toHaveAttribute("href", "/sso");
  });
});

describe("settings sections in plain words (zh-CN)", () => {
  const all = (): SettingItem[] => [
    ...items(),
    item({
      key: "security.captcha_provider",
      section: "security",
      value: "turnstile",
      enum: ["off", "turnstile", "hcaptcha"],
      enum_labels: {
        off: text("Off", "关闭"),
        turnstile: text("Cloudflare Turnstile", "Cloudflare Turnstile"),
        hcaptcha: text("hCaptcha", "hCaptcha"),
      },
      title: text("Captcha", "人机验证"),
    }),
    item({
      key: "site.show_upstream_names",
      section: "branding",
      type: "bool",
      value: true,
      title: text("Show upstream names to members", "向成员显示上游名称"),
    }),
    item({
      key: "logging.level",
      section: "logging",
      apply: "restart",
      value: "info",
      enum: ["debug", "info", "warn"],
      title: text("Log level", "日志级别"),
    }),
    item({
      key: "logging.format",
      section: "logging",
      apply: "file",
      source: "file",
      value: "json",
      title: text("Log format", "日志格式"),
    }),
  ];

  it.each(SECTIONS.map((s) => s.id))("shows the %s section without raw keys", async (section) => {
    renderPage(<SettingsPage />, {
      locale: "zh-CN",
      routes: { "GET /settings": { items: all() } },
      path: `/settings?section=${section}`,
    });
    const title = all().find((i) => SECTIONS.find((s) => s.id === section)!.from.includes(i.section))!.title["zh-CN"]!;
    expect(await screen.findByText(title)).toBeInTheDocument();
    expectPlainWords();
  });
});
