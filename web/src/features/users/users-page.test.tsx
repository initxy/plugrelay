import { screen, waitFor, within } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import type { Feature, Group, Key, Setting, User } from "@/lib/api/types";
import { adminMe, apiError, renderPage, reply, type ApiRoutes } from "@/test/api";
import { translate } from "@/i18n";
import { expectPlainWords } from "@/test/plain-words";
import { UsersPage } from "./users-page";

const usd = (n: number) => ({ micro: n * 1_000_000, currency: "USD" });

const alice: User = {
  id: "u_alice",
  username: "alice@example.com",
  display_name: "Alice",
  email: "alice@example.com",
  role: "member",
  group: { id: "g_vip", name: "vip" },
  status: "active",
  created_at: "2026-10-01T00:00:00Z",
  last_seen_at: "2026-11-07T08:00:00Z",
  key_count: 2,
  month_spend: usd(12.5),
  balance: usd(80),
  mfa_enabled: true,
};
const bob: User = {
  ...alice,
  id: "u_bob",
  username: "bob@example.com",
  display_name: "Bob",
  role: "user",
  group: undefined,
  key_count: 0,
  balance: usd(-1),
  mfa_enabled: false,
};
const vip: Group = {
  id: "g_vip",
  name: "vip",
  description: "Pro plan",
  member_count: 1,
  models: ["claude-*"],
  price_multiplier: 0.8,
  log_bodies: false,
  show_upstream_names: true,
};

const label = (en: string, zh: string) => ({ en, "zh-CN": zh });
const setting = (over: Partial<Setting>): Setting => ({
  module: "m",
  key: "k",
  type: "int",
  label: label("x", "x"),
  help: label("", ""),
  scopes: ["group", "user"],
  merge: "strictest",
  value: null,
  effective: { value: 0, source: "default" },
  ...over,
});
const layer: Setting[] = [
  setting({
    module: "middleware.access",
    key: "allowed_ips",
    type: "list",
    label: label("Allowed source addresses", "允许的来源地址"),
  }),
  setting({
    module: "meter.quota",
    key: "user_monthly_micro",
    unit: "money",
    label: label("Spend per user, per month", "每个用户每月花费上限"),
    feature: "limits",
    effective: { value: 100, source: "group" },
  }),
  setting({
    module: "selector.routing",
    key: "strategy",
    type: "string",
    merge: "override",
    enum: ["priority_weight", "cheapest"],
    enum_labels: { priority_weight: label("By priority", "按优先级"), cheapest: label("Cheapest", "最便宜") },
    label: label("Routing strategy", "路由策略"),
    feature: "routing",
    effective: { value: "priority_weight", source: "default" },
  }),
  setting({
    module: "middleware.access",
    key: "mcp_servers",
    type: "list",
    label: label("MCP servers allowed", "可用的 MCP 服务"),
    feature: "mcp",
  }),
];
const features = [
  { id: "limits", name: label("Rate limits & quotas", "限流与额度") },
  { id: "routing", name: label("Routing", "路由") },
  { id: "mcp", name: label("MCP gateway", "MCP 网关") },
] as Feature[];

function routes(extra: ApiRoutes = {}): ApiRoutes {
  return {
    "GET /users": (c) => ({ items: c.query.get("group_id") === "g_vip" ? [alice] : [alice, bob] }),
    "GET /groups": { items: [vip] },
    "GET /me/security": {
      totp_enabled: false,
      recovery_codes_left: 0,
      passkeys: [],
      has_password: true,
      required: false,
    },
    "GET /users/{id}/identities": {
      items: [
        {
          id: "i_1",
          provider: "google",
          provider_name: "Google",
          subject: "1234",
          user_id: "u_alice",
          email: "alice@example.com",
        },
      ],
    },
    "GET /users/{id}/settings": { items: layer },
    "GET /groups/{id}/settings": { items: layer },
    "GET /keys": { items: [] },
    "GET /features": { items: features },
    "GET /extensions": { items: [] },
    "GET /catalog": { items: [] },
    ...extra,
  };
}

const billingMe = adminMe({ features: ["limits", "routing", "billing"] });

describe("UsersPage", () => {
  it("lists people with role, group, keys, spend and — with billing on — balance", async () => {
    renderPage(<UsersPage />, { path: "/users", me: billingMe, routes: routes() });
    const row = (await screen.findByText("Alice")).closest("tr")!;
    expect(within(row).getByText("Member")).toBeInTheDocument();
    expect(within(row).getByText("vip")).toBeInTheDocument();
    expect(within(row).getByText("$12.50")).toBeInTheDocument();
    expect(within(row).getByText("$80.00")).toBeInTheDocument();
    expect(within(screen.getByText("Bob").closest("tr")!).getByText("No group")).toBeInTheDocument();
    expect(screen.getByRole("columnheader", { name: "Balance" })).toBeInTheDocument();
  });

  it("has no balance column while billing is off", async () => {
    renderPage(<UsersPage />, { path: "/users", routes: routes() });
    await screen.findByText("Alice");
    expect(screen.queryByRole("columnheader", { name: "Balance" })).not.toBeInTheDocument();
  });

  it("filters by role from the URL", async () => {
    renderPage(<UsersPage />, { path: "/users?role=user", routes: routes() });
    expect(await screen.findByText("Bob")).toBeInTheDocument();
    expect(screen.queryByText("Alice")).not.toBeInTheDocument();
  });

  it("changes a role after the admin confirms it is them", async () => {
    let reauthed = false;
    const r = routes({
      "PATCH /users/{id}": (c) => (reauthed ? { ...alice, ...(c.body as object) } : reply(403, { error: { code: "reauth_required", message: "confirm" } })),
      "POST /me/reauth": () => {
        reauthed = true;
        return reply(204);
      },
    });
    const { user, api: stub } = renderPage(<UsersPage />, { path: "/users?id=u_alice", routes: r });
    const drawer = await screen.findByRole("dialog", { name: /Alice/ });
    await user.click(await within(drawer).findByRole("radio", { name: "Admin" }));
    await user.click(within(drawer).getByRole("button", { name: "Save" }));
    const reauth = await screen.findByRole("dialog", { name: "Confirm it's you" });
    await user.type(within(reauth).getByLabelText("Password"), "correct-horse-1");
    await user.click(within(reauth).getByRole("button", { name: "Confirm" }));
    expect(await screen.findByText("Alice saved")).toBeInTheDocument();
    const patches = stub.called("PATCH", "/users/u_alice");
    expect(patches).toHaveLength(2);
    expect(patches[1]!.body).toMatchObject({ role: "admin", group_id: "g_vip", status: "active" });
  });

  it("resets two-step sign-in and unlinks a single sign-on account after confirming", async () => {
    const r = routes({
      "DELETE /users/{id}/mfa": reply(204),
      "DELETE /users/{id}/identities/{identity_id}": reply(204),
    });
    const { user, api: stub } = renderPage(<UsersPage />, { path: "/users?id=u_alice", routes: r });
    const drawer = await screen.findByRole("dialog", { name: /Alice/ });
    await user.click(await within(drawer).findByRole("button", { name: "Reset" }));
    const confirm = await screen.findByRole("alertdialog");
    expect(confirm).toHaveTextContent("Reset two-step sign-in of Alice?");
    await user.click(within(confirm).getByRole("button", { name: "Reset" }));
    expect(await screen.findByText("Two-step sign-in of Alice reset")).toBeInTheDocument();

    await user.click(await within(drawer).findByRole("button", { name: "Unlink Google" }));
    await user.click(within(await screen.findByRole("alertdialog")).getByRole("button", { name: "Unlink" }));
    await waitFor(() => expect(stub.called("DELETE", "/users/u_alice/identities/i_1")).toHaveLength(1));
  });

  it("explains a refused deletion and deletes anyway when asked", async () => {
    const r = routes({
      "DELETE /users/{id}": (c) =>
        c.query.get("force") === "true"
          ? reply(204)
          : apiError(409, "user_delete_blocked", "the user cannot be deleted yet: subscription could not be cancelled", {
              params: { reason: "subscription could not be cancelled" },
            }),
    });
    const { user } = renderPage(<UsersPage />, { path: "/users?id=u_alice", routes: r });
    const drawer = await screen.findByRole("dialog", { name: /Alice/ });
    await user.click(await within(drawer).findByRole("button", { name: "Delete user" }));
    await user.click(within(await screen.findByRole("alertdialog")).getByRole("button", { name: "Delete" }));
    const blocked = await screen.findByRole("alertdialog", { name: "Alice cannot be deleted yet" });
    expect(blocked).toHaveTextContent("subscription could not be cancelled");
    await user.click(within(blocked).getByRole("button", { name: "Delete anyway" }));
    expect(await screen.findByText("Alice deleted")).toBeInTheDocument();
  });

  it("shows a person's own settings grouped by feature and sends only what changed, in currency units", async () => {
    const r = routes({ "PUT /users/{id}/settings": { items: layer } });
    const { user, api: stub } = renderPage(<UsersPage />, { path: "/users?id=u_alice", routes: r });
    const drawer = await screen.findByRole("dialog", { name: /Alice/ });
    await user.click(await within(drawer).findByRole("tab", { name: "Limits & settings" }));
    expect(await within(drawer).findByRole("heading", { name: "Rate limits & quotas" })).toBeInTheDocument();
    expect(within(drawer).getByRole("heading", { name: "Routing" })).toBeInTheDocument();
    expect(within(drawer).getByRole("heading", { name: "Access" })).toBeInTheDocument();
    // MCP is off, so its setting is not offered; no module or key name is shown.
    expect(within(drawer).queryByText("MCP servers allowed")).not.toBeInTheDocument();
    expect(within(drawer).queryByText(/meter\.quota|user_monthly_micro/)).not.toBeInTheDocument();
    expect(within(drawer).getByText(/Now: \$100\.00 \(Group\)\./)).toBeInTheDocument();

    await user.type(within(drawer).getByLabelText("Spend per user, per month"), "25.5");
    await user.click(within(drawer).getByRole("button", { name: "Save" }));
    await waitFor(() => expect(stub.called("PUT", "/users/u_alice/settings")).toHaveLength(1));
    expect(stub.called("PUT", "/users/u_alice/settings")[0]!.body).toEqual({
      settings: { "meter.quota": { user_monthly_micro: 25.5 } },
    });
  });

  it("lists a person's keys", async () => {
    const r = routes({
      "GET /keys": {
        items: [
          {
            id: "k_1",
            name: "laptop",
            last4: "ab12",
            status: "active",
            created_at: "",
            month_spend: usd(3),
            settings: { rpm: 10 },
          },
        ],
      },
    });
    const { user, api: stub } = renderPage(<UsersPage />, { path: "/users?id=u_alice", routes: r });
    const drawer = await screen.findByRole("dialog", { name: /Alice/ });
    await user.click(await within(drawer).findByRole("tab", { name: /Keys/ }));
    expect(await within(drawer).findByText("laptop")).toBeInTheDocument();
    expect(within(drawer).getByText("10/min")).toBeInTheDocument();
    expect(stub.calls.some((c) => c.path === "/keys" && c.query.get("user_id") === "u_alice")).toBe(true);
  });
});

describe("Groups", () => {
  it("lists groups with their models and price multiplier", async () => {
    renderPage(<UsersPage />, { path: "/users?tab=groups", routes: routes() });
    const row = (await screen.findByText("Pro plan")).closest("tr")!;
    expect(within(row).getByText("claude-*")).toBeInTheDocument();
    expect(within(row).getByText("×0.8")).toBeInTheDocument();
  });

  it("saves the group's models, multiplier and body logging", async () => {
    const r = routes({ "PATCH /groups/{id}": (c) => ({ ...vip, ...(c.body as object) }) });
    const { user, api: stub } = renderPage(<UsersPage />, {
      path: "/users?tab=groups&id=g_vip",
      routes: r,
    });
    const drawer = await screen.findByRole("dialog", { name: /vip/ });
    await user.type(await within(drawer).findByLabelText("Models they may use"), "gpt-5.5{Enter}");
    const mult = within(drawer).getByLabelText("Price multiplier");
    await user.clear(mult);
    await user.type(mult, "1.2");
    expect(within(drawer).getByText(/Members pay 120% of list price/)).toBeInTheDocument();
    await user.click(within(drawer).getByRole("switch", { name: "Store request and response text" }));
    await user.click(within(drawer).getByRole("button", { name: "Save" }));
    await waitFor(() => expect(stub.called("PATCH", "/groups/g_vip")).toHaveLength(1));
    expect(stub.called("PATCH", "/groups/g_vip")[0]!.body).toMatchObject({
      name: "vip",
      models: ["claude-*", "gpt-5.5"],
      price_multiplier: 1.2,
      log_bodies: true,
    });
  });

  it("lists the group's members and warns before deleting a group that has some", async () => {
    const r = routes({ "DELETE /groups/{id}": reply(204) });
    const { user, api: stub } = renderPage(<UsersPage />, {
      path: "/users?tab=groups&id=g_vip",
      routes: r,
    });
    const drawer = await screen.findByRole("dialog", { name: /vip/ });
    await user.click(await within(drawer).findByRole("tab", { name: "Members" }));
    expect(await within(drawer).findByText("alice@example.com")).toBeInTheDocument();
    await user.click(within(drawer).getByRole("tab", { name: "Basics" }));
    await user.click(within(drawer).getByRole("button", { name: "Delete group" }));
    const confirm = await screen.findByRole("alertdialog");
    expect(confirm).toHaveTextContent("1 person is in it");
    await user.click(within(confirm).getByRole("button", { name: "Delete" }));
    await waitFor(() => expect(stub.called("DELETE", "/groups/g_vip")).toHaveLength(1));
  });
});

describe("users and groups in plain words (zh-CN)", () => {
  const key: Key = {
    id: "k_1",
    name: "laptop",
    last4: "ab12",
    owner: { id: "u_alice", name: "Alice" },
    group: { id: "g_vip", name: "vip" },
    status: "active",
    created_at: "2026-11-01T08:00:00Z",
    month_spend: usd(4),
    settings: { rpm: 60 },
  };
  const zh = (path: string) =>
    renderPage(<UsersPage />, {
      path,
      me: billingMe,
      routes: routes({ "GET /keys": { items: [key] } }),
      locale: "zh-CN",
    });

  it.each(["/users", "/users?tab=groups"])("shows %s without raw ids or keys", async (path) => {
    zh(path);
    expect((await screen.findAllByText("vip")).length).toBeGreaterThan(0);
    expectPlainWords();
  });

  it("shows every tab of the user drawer without raw ids or keys", async () => {
    const { user } = zh("/users?id=u_alice");
    const drawer = await screen.findByRole("dialog", { name: /Alice/ });
    await within(drawer).findByText("Google");
    expectPlainWords(drawer);
    await user.click(within(drawer).getByRole("tab", { name: new RegExp(translate("zh-CN", "users.tabKeys")) }));
    expect(await within(drawer).findByText("laptop")).toBeInTheDocument();
    expectPlainWords(drawer);
    await user.click(within(drawer).getByRole("tab", { name: translate("zh-CN", "users.tabSettings") }));
    expect(await within(drawer).findByText("每个用户每月花费上限")).toBeInTheDocument();
    expectPlainWords(drawer);
  });

  it("shows every tab of the group drawer without raw ids or keys", async () => {
    const { user } = zh("/users?tab=groups&id=g_vip");
    const drawer = await screen.findByRole("dialog");
    await within(drawer).findByRole("tab", { name: translate("zh-CN", "groups.tabBasics") });
    expectPlainWords(drawer);
    await user.click(within(drawer).getByRole("tab", { name: translate("zh-CN", "groups.tabSettings") }));
    expect(await within(drawer).findByText("每个用户每月花费上限")).toBeInTheDocument();
    expectPlainWords(drawer);
    await user.click(within(drawer).getByRole("tab", { name: translate("zh-CN", "groups.tabMembers") }));
    expect(await within(drawer).findByText("Alice")).toBeInTheDocument();
    expectPlainWords(drawer);
  });
});
