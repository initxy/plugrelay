import { screen, waitFor, within } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import type { AuditEntry } from "@/lib/api/types";
import { renderPage, type ApiRoutes } from "@/test/api";
import { expectPlainWords } from "@/test/plain-words";
import { AuditPage } from "./audit-page";
import { diffLeaves } from "./audit-words";

const recent = new Date(Date.now() - 5 * 60_000).toISOString();
const admin = { id: "u_admin", name: "Admin" };

const entries: AuditEntry[] = [
  {
    id: "a1",
    time: recent,
    actor: admin,
    action: "feature.enable",
    target: { type: "feature", id: "billing", name: "Billing" },
    ip: "198.51.100.10",
    changes: [{ field: "enabled", from: false, to: true }],
  },
  {
    id: "a2",
    time: recent,
    actor: admin,
    action: "settings.update",
    target: { type: "settings", id: "site" },
    changes: [{ field: "site.name", from: "plugrelay", to: "Acme relay" }],
  },
  {
    id: "a3",
    time: recent,
    actor: admin,
    action: "upstream.update",
    target: { type: "upstream", id: "anthropic-main", name: "anthropic-main" },
    changes: [
      {
        field: "upstream",
        from: { enabled: true, config: { region: "us-east-1" } },
        to: { enabled: false, config: { region: "us-east-1" } },
      },
    ],
  },
  {
    id: "a4",
    time: recent,
    actor: { id: "cli", name: "command line" },
    action: "user.mfa_reset",
    target: { type: "user", id: "u_2", name: "Alice" },
  },
];

const routes: ApiRoutes = {
  "GET /audit": ({ query }) => ({
    items: query.get("target_id") ? entries.filter((e) => e.target.id === query.get("target_id")) : entries,
    next_cursor: null,
  }),
  "GET /features": {
    items: [
      {
        id: "billing",
        name: { en: "Billing", "zh-CN": "计费" },
        description: { en: "" },
        enabled: true,
        default: false,
        modules: [],
        settings: [],
        pages: [],
      },
    ],
  },
  "GET /settings": {
    items: [
      {
        key: "site.name",
        section: "branding",
        type: "string",
        value: "",
        apply: "live",
        source: "default",
        title: { en: "Site name", "zh-CN": "站点名称" },
        help: { en: "" },
      },
    ],
  },
  "GET /users": {
    items: [
      {
        id: "u_admin",
        username: "admin",
        display_name: "Admin",
        role: "admin",
        status: "active",
        created_at: recent,
        month_spend: { micro: 0, currency: "USD" },
      },
    ],
  },
};

describe("AuditPage", () => {
  it("says who did what to which object in plain words", async () => {
    renderPage(<AuditPage />, { locale: "zh-CN", routes, path: "/audit" });
    expect(await screen.findByText("打开了功能")).toBeInTheDocument();
    expect(screen.getByText("计费")).toBeInTheDocument();
    expect(screen.getByText("修改了站点设置")).toBeInTheDocument();
    expect(screen.getByText("站点名称")).toBeInTheDocument();
    expect(screen.getByText("命令行")).toBeInTheDocument();
    expect(screen.getByText("1 处改动")).toBeInTheDocument();
    expectPlainWords();
  });

  it("keeps filters in the URL and sends them as action patterns", async () => {
    const { user, api, router } = renderPage(<AuditPage />, { locale: "zh-CN", routes, path: "/audit" });
    await screen.findByText("打开了功能");
    await user.click(screen.getByRole("button", { name: /全部类别/ }));
    await user.click(await screen.findByRole("menuitemradio", { name: "密钥" }));
    await waitFor(() => expect(router.state.location.search).toMatchObject({ cat: "keys" }));
    await waitFor(() => expect(api.calls.at(-1)?.query.get("action")).toBe("key.*"));
  });

  it("opens an entry with each changed field, then that object's history", async () => {
    const { user, api } = renderPage(<AuditPage />, { locale: "zh-CN", routes, path: "/audit?id=a3" });
    const drawer = await screen.findByRole("dialog");
    expect(within(drawer).getByRole("heading", { name: "修改了上游" })).toBeInTheDocument();
    expect(within(drawer).getByText("enabled")).toBeInTheDocument();
    expect(within(drawer).getByText("是")).toHaveClass("line-through");
    expect(within(drawer).queryByText("config.region")).not.toBeInTheDocument();
    expectPlainWords(drawer);
    await user.click(within(drawer).getByRole("button", { name: "这个对象的全部记录" }));
    await waitFor(() => {
      const last = api.calls.filter((c) => c.path === "/audit").at(-1)!;
      expect(last.query.get("target_type")).toBe("upstream");
      expect(last.query.get("target_id")).toBe("anthropic-main");
    });
    expect(await screen.findByText("只看：anthropic-main")).toBeInTheDocument();
  });
});

describe("diffLeaves", () => {
  it("lists only the leaves that changed", () => {
    expect(diffLeaves({ a: 1, b: { c: 2, d: 3 } }, { a: 1, b: { c: 2, d: 4 }, e: true })).toEqual([
      { path: "b.d", from: 3, to: 4 },
      { path: "e", from: undefined, to: true },
    ]);
    expect(diffLeaves("x", "y")).toEqual([{ path: "", from: "x", to: "y" }]);
  });
});
