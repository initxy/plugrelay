import { screen, waitFor, within } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import type { ConnectInfo, Key, Setting } from "@/lib/api/types";
import { memberMe, renderPage, type ApiRoutes } from "@/test/api";
import { translate } from "@/i18n";
import { expectPlainWords } from "@/test/plain-words";
import { KeysPage } from "./keys-page";

const usd = (n: number) => ({ micro: n * 1_000_000, currency: "USD" });

const laptop: Key = {
  id: "k_1",
  name: "laptop",
  prefix: "pr_",
  last4: "ab12",
  owner: { id: "u_alice", name: "Alice" },
  group: { id: "g_vip", name: "vip" },
  status: "active",
  created_at: "2026-11-01T08:00:00Z",
  last_used_at: "2026-11-07T08:00:00Z",
  month_spend: usd(45),
  settings: { budget_monthly: usd(50), rpm: 60, clients: ["codex"] },
};
const old: Key = {
  id: "k_2",
  name: "old-script",
  last4: "zz99",
  owner: { id: "u_bob", name: "Bob" },
  status: "expired",
  created_at: "2026-10-01T08:00:00Z",
  month_spend: usd(0),
  settings: { expires_at: "2026-11-01T23:59:59Z" },
};

const connect: ConnectInfo = {
  base_url: "https://relay.example.com",
  placeholder: "{{api_key}}",
  clients: [
    {
      client: "claude-code",
      name: "Claude Code",
      os: "linux",
      snippets: [
        {
          title: { en: "~/.bashrc" },
          path: "~/.bashrc",
          language: "bash",
          content: "export ANTHROPIC_AUTH_TOKEN={{api_key}}",
        },
      ],
      notes: [{ en: "Or run: plugrelay setup --key {{api_key}}", "zh-CN": "也可以运行" }],
    },
    {
      client: "codex",
      name: "Codex",
      os: "linux",
      snippets: [
        {
          title: { en: "Model catalog", "zh-CN": "模型清单" },
          language: "bash",
          content: "curl -H 'Authorization: Bearer {{api_key}}'",
        },
      ],
    },
  ],
  common_errors: [{ symptom: "401 invalid x-api-key", fix: { en: "Copy the key again." } }],
};

function routes(extra: ApiRoutes = {}): ApiRoutes {
  return {
    "GET /keys": { items: [laptop, old] },
    "GET /keys/{id}/connect": connect,
    "GET /keys/{id}/settings": { items: [] },
    "GET /catalog": {
      items: [
        {
          model: "gpt-5.5",
          kind: "model",
          price: {
            currency: "USD",
            input: 1,
            cache_read: 0,
            cache_write_5m: 0,
            cache_write_1h: 0,
            output: 1,
          },
        },
        {
          model: "claude-sonnet-4-6",
          kind: "model",
          price: {
            currency: "USD",
            input: 1,
            cache_read: 0,
            cache_write_5m: 0,
            cache_write_1h: 0,
            output: 1,
          },
        },
      ],
    },
    "GET /groups": { items: [{ id: "g_vip", name: "vip", member_count: 1, models: ["gpt-*"] }] },
    "GET /users": { items: [] },
    "GET /features": { items: [] },
    "GET /extensions": { items: [] },
    "GET /clients": {
      items: [
        { id: "playground", name: "Playground" },
        { id: "claude-code", name: "Claude Code" },
        { id: "codex", name: "Codex" },
        { id: "roo", name: "Roo Code" },
        { id: "sdk-python", name: "Python SDK" },
        { id: "other", name: "" },
      ],
    },
    ...extra,
  };
}

describe("KeysPage", () => {
  it("lists keys with owner, state, own limits in plain words and spend against the budget", async () => {
    renderPage(<KeysPage />, { path: "/keys", routes: routes() });
    const row = (await screen.findByText("laptop")).closest("tr")!;
    expect(within(row).getByText("Alice")).toBeInTheDocument();
    expect(within(row).getByText("vip")).toBeInTheDocument();
    expect(within(row).getByText("Active")).toBeInTheDocument();
    expect(within(row).getByText("$50.00/mo")).toBeInTheDocument();
    expect(within(row).getByText("60/min")).toBeInTheDocument();
    expect(within(row).getByText("Codex only")).toBeInTheDocument();
    expect(within(row).getByText("/ $50.00")).toBeInTheDocument();
    const expired = screen.getByText("old-script").closest("tr")!;
    expect(within(expired).getByText("Expired")).toBeInTheDocument();
    // The near-budget tile counts the key at 90 %.
    expect(screen.getByText("Close to the budget").parentElement).toHaveTextContent("1");
  });

  it("filters by status from the URL", async () => {
    renderPage(<KeysPage />, { path: "/keys?status=expired", routes: routes() });
    expect(await screen.findByText("old-script")).toBeInTheDocument();
    expect(screen.queryByText("laptop")).not.toBeInTheDocument();
    expect(screen.getByText("1 of 2 keys")).toBeInTheDocument();
  });

  it("saves every limit the key carries, including requests per minute and a total budget", async () => {
    const r = routes({
      "PATCH /keys/{id}": (c) => ({
        ...laptop,
        settings: (c.body as { settings: Key["settings"] }).settings,
      }),
    });
    const { user, api: stub } = renderPage(<KeysPage />, { path: "/keys?id=k_1", routes: r });
    const drawer = await screen.findByRole("dialog");
    expect(await within(drawer).findByText("Spends at most $50.00 a month")).toBeInTheDocument();
    await user.click(await within(drawer).findByRole("tab", { name: "Limits" }));

    await user.type(within(drawer).getByLabelText("In total"), "200");
    const rpm = within(drawer).getByLabelText("Requests per minute");
    await user.clear(rpm);
    await user.type(rpm, "30");
    await user.type(within(drawer).getByLabelText("At the same time"), "4");
    await user.type(within(drawer).getByLabelText("Allowed models"), "gpt-5.5{Enter}");
    await user.click(within(drawer).getByRole("button", { name: "Claude Code" }));
    await user.type(within(drawer).getByLabelText("Allowed addresses"), "10.0.0.0/8{Enter}");
    await user.type(within(drawer).getByLabelText("Expires after"), "2027-01-31");
    await user.click(within(drawer).getByRole("button", { name: "Save" }));

    await waitFor(() => expect(stub.called("PATCH", "/keys/k_1")).toHaveLength(1));
    const body = stub.called("PATCH", "/keys/k_1")[0]!.body as { name: string; settings: Key["settings"] };
    expect(body.name).toBe("laptop");
    expect(body.settings).toMatchObject({
      budget_monthly: usd(50),
      budget_total: usd(200),
      rpm: 30,
      max_concurrency: 4,
      models: ["gpt-5.5"],
      clients: ["codex", "claude-code"],
      ip_allowlist: ["10.0.0.0/8"],
    });
    expect(new Date(body.settings.expires_at!).getDate()).toBe(31);
    expect(await screen.findByText("Key saved")).toBeInTheDocument();
  });

  it("refuses an address that is not an IP or range before sending", async () => {
    const r = routes();
    const { user, api: stub } = renderPage(<KeysPage />, { path: "/keys?id=k_1", routes: r });
    const drawer = await screen.findByRole("dialog");
    await user.click(await within(drawer).findByRole("tab", { name: "Limits" }));
    await user.type(within(drawer).getByLabelText("Allowed addresses"), "office{Enter}");
    await user.click(within(drawer).getByRole("button", { name: "Save" }));
    expect(await within(drawer).findByText('"office" isn\'t an IP address or CIDR range')).toBeInTheDocument();
    expect(stub.called("PATCH", "/keys/k_1")).toHaveLength(0);
  });

  it("creates a key, shows the secret once and fills it into the client setup", async () => {
    const created = { ...laptop, id: "k_new", name: "ci", owner: { id: "u_bob", name: "Bob" }, settings: {} };
    const items = [laptop, old];
    const r = routes({
      "GET /users": {
        items: [
          {
            id: "u_bob",
            username: "bob@example.com",
            display_name: "Bob",
            role: "member",
            status: "active",
            created_at: "",
            month_spend: usd(0),
          },
        ],
      },
      "POST /keys": () => {
        items.push(created);
        return { key: created, secret: "pr_SECRET123" };
      },
      "GET /keys": () => ({ items }),
    });
    const { user, api: stub } = renderPage(<KeysPage />, { path: "/keys", routes: r });
    await screen.findByText("laptop");
    await user.click(screen.getAllByRole("button", { name: "Create key" })[0]!);
    const drawer = await screen.findByRole("dialog");
    await user.type(within(drawer).getByLabelText("Name"), "ci");
    await user.click(within(drawer).getByRole("button", { name: "Create key" }));

    await waitFor(() => expect(stub.called("POST", "/keys")).toHaveLength(1));
    expect(stub.called("POST", "/keys")[0]!.body).toMatchObject({
      name: "ci",
      settings: { models: [], clients: [], ip_allowlist: [], expires_at: null },
    });
    const secret = await screen.findByRole("dialog", { name: "Your new key for ci" });
    expect(within(secret).getByText("pr_SECRET123")).toBeInTheDocument();
    await user.click(within(secret).getByRole("button", { name: "Copy" }));
    await user.click(within(secret).getByRole("button", { name: "Copied — set up a client" }));
    expect(await screen.findByText("export ANTHROPIC_AUTH_TOKEN=pr_SECRET123")).toBeInTheDocument();
    expect(screen.getByText("Or run: plugrelay setup --key pr_SECRET123")).toBeInTheDocument();
  });

  it("replaces a key only after confirming, then shows the new secret", async () => {
    const r = routes({ "POST /keys/{id}/rotate": { key: laptop, secret: "pr_NEW" } });
    const { user, api: stub } = renderPage(<KeysPage />, { path: "/keys", routes: r });
    await user.click(await screen.findByRole("button", { name: "More actions for laptop" }));
    await user.click(await screen.findByRole("menuitem", { name: "New key" }));
    const confirm = await screen.findByRole("alertdialog");
    expect(confirm).toHaveTextContent("Replace the key laptop?");
    await user.click(within(confirm).getByRole("button", { name: "New key" }));
    expect(await screen.findByText("pr_NEW")).toBeInTheDocument();
    expect(stub.called("POST", "/keys/k_1/rotate")).toHaveLength(1);
  });

  it("tests a key from its drawer and links to the request", async () => {
    const r = routes({
      "POST /keys/{id}/test": {
        ok: true,
        results: [{ ok: true, ttfb_ms: 420, model: "gpt-5.5", request_id: "r_1" }],
      },
    });
    const { user, api: stub } = renderPage(<KeysPage />, { path: "/keys?id=k_1", routes: r });
    const drawer = await screen.findByRole("dialog");
    await user.click(await within(drawer).findByRole("button", { name: "Test connection" }));
    expect(await within(drawer).findByText("Connected · first token in 420ms · answered by gpt-5.5")).toBeInTheDocument();
    expect(stub.called("POST", "/keys/k_1/test")[0]!.body).toEqual({ client: "codex" });
    expect(within(drawer).getByRole("link", { name: "View request" })).toHaveAttribute("href", "/logs?id=r_1");
  });

  it("shows members their own keys without owners or admin-only settings", async () => {
    renderPage(<KeysPage />, { path: "/keys?id=k_1", me: memberMe(), routes: routes() });
    const drawer = await screen.findByRole("dialog");
    await within(drawer).findByRole("tab", { name: "Limits" });
    expect(within(drawer).queryByRole("tab", { name: "More settings" })).not.toBeInTheDocument();
    expect(screen.queryByRole("columnheader", { name: "Owner" })).not.toBeInTheDocument();
  });

  it("offers every client the relay recognises, but not other, and keeps a key's own", async () => {
    const r = routes({
      "GET /keys": { items: [{ ...laptop, settings: { clients: ["roo", "retired-tool"] } }, old] },
      "PATCH /keys/{id}": (c) => ({ ...laptop, settings: (c.body as { settings: Key["settings"] }).settings }),
    });
    const { user, api: stub } = renderPage(<KeysPage />, { path: "/keys?id=k_1", routes: r });
    const drawer = await screen.findByRole("dialog");
    await user.click(await within(drawer).findByRole("tab", { name: "Limits" }));
    const group = within(drawer).getByRole("group", { name: "Allowed clients" });
    await within(group).findByRole("button", { name: "Playground" });
    expect(within(group).getAllByRole("button").map((b) => b.textContent)).toEqual([
      "Playground",
      "Claude Code",
      "Codex",
      "Roo Code",
      "Python SDK",
      "retired-tool",
    ]);
    expect(within(group).getByRole("button", { name: "Roo Code" })).toHaveAttribute("aria-pressed", "true");
    await user.click(within(group).getByRole("button", { name: "retired-tool" }));
    await user.click(within(group).getByRole("button", { name: "Python SDK" }));
    await user.click(within(drawer).getByRole("button", { name: "Save" }));
    await waitFor(() => expect(stub.called("PATCH", "/keys/k_1")).toHaveLength(1));
    expect((stub.called("PATCH", "/keys/k_1")[0]!.body as { settings: Key["settings"] }).settings.clients).toEqual(["roo", "sdk-python"]);
  });

  it("switches the setup to another client and shows its localized title", async () => {
    const { user } = renderPage(<KeysPage />, { path: "/keys", routes: routes() });
    await user.click(await screen.findByRole("tab", { name: "Codex" }));
    expect(await screen.findByText("Model catalog")).toBeInTheDocument();
    expect(screen.getByText("curl -H 'Authorization: Bearer <your-key>'")).toBeInTheDocument();
  });
});

describe("keys in plain words (zh-CN)", () => {
  it("shows the list and the connect panel without raw ids or keys", async () => {
    renderPage(<KeysPage />, { path: "/keys", routes: routes(), locale: "zh-CN" });
    await screen.findByText("laptop");
    expectPlainWords();
  });

  it("shows every drawer tab without raw ids or keys", async () => {
    const strategy: Setting = {
      module: "selector.routing",
      key: "strategy",
      type: "string",
      label: { en: "Routing strategy", "zh-CN": "路由策略" },
      help: { en: "", "zh-CN": "" },
      scopes: ["global", "key"],
      merge: "override",
      enum: ["priority_weight", "cheapest"],
      enum_labels: {
        priority_weight: { en: "By priority", "zh-CN": "按优先级" },
        cheapest: { en: "Cheapest", "zh-CN": "最便宜" },
      },
      feature: "routing",
      value: "cheapest",
      effective: { value: "cheapest", source: "key" },
    };
    const { user } = renderPage(<KeysPage />, {
      path: "/keys?id=k_1",
      routes: routes({ "GET /keys/{id}/settings": { items: [strategy] } }),
      locale: "zh-CN",
    });
    const drawer = await screen.findByRole("dialog");
    await within(drawer).findByRole("tab", { name: translate("zh-CN", "keys.tabLimits") });
    expectPlainWords(drawer);
    await user.click(within(drawer).getByRole("tab", { name: translate("zh-CN", "keys.tabLimits") }));
    expect(await within(drawer).findByRole("button", { name: "试用对话" })).toBeInTheDocument();
    expectPlainWords(drawer);
    await user.click(within(drawer).getByRole("tab", { name: translate("zh-CN", "keys.tabMore") }));
    expect(await within(drawer).findByText("路由策略")).toBeInTheDocument();
    expectPlainWords(drawer);
  });
});
