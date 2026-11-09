import { screen, waitFor, within } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { renderPage, reply, type ApiRoutes } from "@/test/api";
import type { Upstream } from "@/lib/api/types";
import { diffDraft, toDraft } from "./upstream-draft";
import { UpstreamsPage } from "./upstreams-page";
import { upstreamsRoute } from "@/routes/pages";
import { translate } from "@/i18n";
import { expectPlainWords } from "@/test/plain-words";
import * as fx from "./upstreams.fixtures";

function fixtures(extra: ApiRoutes = {}): ApiRoutes {
  const ups = fx.upstreams();
  return {
    "GET /upstreams": { items: ups },
    "GET /upstreams/{id}": (c) => ups.find((u) => c.path.endsWith("/" + u.id)) ?? reply(404, { error: { code: "not_found", message: "nope" } }),
    "GET /manifests": { items: fx.manifests() },
    "GET /models": { items: fx.models() },
    "GET /users": {
      items: [
        {
          id: "u2",
          username: "bob",
          display_name: "Bob",
          role: "member",
          status: "active",
          created_at: "2026-01-01T00:00:00Z",
          month_spend: { micro: 0, currency: "USD" },
        },
      ],
    },
    ...extra,
  };
}

describe("upstreams list", () => {
  it("names every state in plain words and never shows internal ids or counters", async () => {
    const r = fixtures();
    renderPage(<UpstreamsPage />, { path: "/upstreams", routes: r });
    expect(await screen.findByText("Anthropic main")).toBeInTheDocument();
    expect(screen.getByText(/Rate limited, back at \d\d:\d\d/)).toBeInTheDocument();
    expect(screen.getByText("Possibly suspended; stopped")).toBeInTheDocument();
    expect(screen.getByText(/Upstream paused · retries at \d\d:\d\d/)).toBeInTheDocument();
    // Its own credential repeats the same news; the row says it once.
    expect(screen.queryByText("AWS prod")).not.toBeInTheDocument();
    expect(screen.getByText("Amazon Bedrock (Anthropic models) · official template")).toBeInTheDocument();
    expect(screen.getByText("OpenAI Responses · OpenAI Chat")).toBeInTheDocument();
    const text = document.body.textContent ?? "";
    for (const raw of ["openai-chat", "circuit", "consecutive", "rate_limit", "provider."]) expect(text).not.toContain(raw);
    expect(screen.getByText("1 paused")).toBeInTheDocument();
  });

  it("filters by name through the URL", async () => {
    const r = fixtures();
    const { user, router } = renderPage(<UpstreamsPage />, { path: "/upstreams", routes: r });
    await screen.findByText("Anthropic main");
    await user.type(screen.getByRole("searchbox"), "bedr{Enter}");
    await waitFor(() => expect(screen.queryByText("Anthropic main")).not.toBeInTheDocument());
    expect(screen.getByText("Bedrock us")).toBeInTheDocument();
    expect(router.state.location.search).toMatchObject({ q: "bedr" });
  });

  it("reads the open drawer, its tab and the attention filter from the URL", () => {
    const validate = upstreamsRoute.options.validateSearch as (s: Record<string, unknown>) => Record<string, unknown>;
    expect(validate({ id: "a", tab: "credentials", attention: "1" })).toEqual({
      id: "a",
      tab: "credentials",
      attention: true,
    });
  });
});

describe("upstream drawer", () => {
  it("shows a paused upstream's reason and deletes it after confirming", async () => {
    const r = fixtures({ "DELETE /upstreams/{id}": undefined });
    const { user, api: f } = renderPage(<UpstreamsPage />, { path: "/upstreams?id=bedrock", routes: r });
    await screen.findByRole("tab", { name: /Overview/ });
    const dialog = screen.getByRole("dialog");
    expect(await within(dialog).findByText("Paused after repeated failures")).toBeInTheDocument();
    await user.click(within(dialog).getByRole("button", { name: "More actions" }));
    await user.click(await screen.findByRole("menuitem", { name: /Delete upstream/ }));
    await user.click(await screen.findByRole("button", { name: "Delete" }));
    await waitFor(() => expect(f.calls.some((c) => c.method === "DELETE" && c.path === "/upstreams/bedrock")).toBe(true));
  });

  it("edits a credential and adds several from pasted lines", async () => {
    const r = fixtures({
      "PATCH /credentials/{id}": (c) => ({ ...fx.cred({ id: "c2", label: "x" }), ...(c.body as object) }),
      "POST /upstreams/{id}/credentials": (c) => fx.cred({ id: "n", label: c.body.label }),
    });
    const { user, api: f } = renderPage(<UpstreamsPage />, {
      path: "/upstreams?id=anthropic&tab=credentials",
      routes: r,
    });
    await screen.findByRole("tab", { name: /Overview/ });
    const dialog = screen.getByRole("dialog");
    const backup = (await within(dialog).findByText("Backup")).closest("li")!;
    await user.click(within(backup).getByRole("button", { name: "Edit" }));
    const name = within(backup).getByLabelText("Name");
    await user.clear(name);
    await user.type(name, "Backup 2");
    await user.type(within(backup).getByLabelText(/Requests at once/), "4");
    await user.click(within(backup).getByRole("button", { name: "Save" }));
    await waitFor(() =>
      expect(f.calls.find((c) => c.method === "PATCH")?.body).toEqual({
        label: "Backup 2",
        proxy: "",
        max_concurrency: 4,
      }),
    );

    await user.click(within(dialog).getByRole("button", { name: "Add credential" }));
    await user.type(within(dialog).getByPlaceholderText("Team plan"), "Pool");
    await user.type(within(dialog).getByLabelText("API key"), "sk-one{Enter}sk-two");
    await user.click(within(dialog).getByRole("button", { name: "Add 2 credentials" }));
    await waitFor(() => expect(f.calls.filter((c) => c.method === "POST")).toHaveLength(2));
    expect(f.calls.filter((c) => c.method === "POST").map((c) => c.body)).toEqual([
      { label: "Pool 1", kind: "shared", secret: "sk-one" },
      { label: "Pool 2", kind: "shared", secret: "sk-two" },
    ]);
  });
});

describe("credentials of a self-hosted server", () => {
  it("adds one without a key, as the add-upstream form allows", async () => {
    const local = fx.upstream({ id: "local", name: "Local Ollama", source: { kind: "official", name: "ollama" }, key_optional: true });
    const { user, api: f } = renderPage(<UpstreamsPage />, {
      path: "/upstreams?id=local&tab=credentials",
      routes: fixtures({
        "GET /upstreams": { items: [local] },
        "GET /upstreams/{id}": local,
        "POST /upstreams/{id}/credentials": (c) => fx.cred({ id: "n", upstream_id: "local", label: c.body.label }),
      }),
    });
    await screen.findByRole("tab", { name: /Overview/ });
    const dialog = screen.getByRole("dialog");
    await user.click(await within(dialog).findByRole("button", { name: "Add credential" }));
    expect(within(dialog).getByLabelText("API key (optional)")).toHaveValue("");
    await user.type(within(dialog).getByPlaceholderText("Team plan"), "No key");
    await user.click(within(dialog).getByRole("button", { name: "Add credential" }));
    await waitFor(() => expect(f.calls.filter((c) => c.method === "POST")).toHaveLength(1));
    expect(f.calls.find((c) => c.method === "POST")?.body).toEqual({ label: "No key", kind: "shared", secret: "" });
  });

  it.each([
    ["self-hosted", "ollama", true],
    ["gateway", "litellm", true],
    ["cloud", "anthropic", false],
  ] as const)("promises keyless requests only for a key-optional manifest (%s)", async (_, manifest, keyless) => {
    const up = fx.upstream({ id: "server", name: "Server", source: { kind: "official", name: manifest }, credentials: [], key_optional: keyless });
    renderPage(<UpstreamsPage />, {
      path: "/upstreams?id=server&tab=credentials",
      routes: fixtures({ "GET /upstreams": { items: [up] }, "GET /upstreams/{id}": up }),
    });
    await screen.findByRole("tab", { name: /Overview/ });
    const dialog = screen.getByRole("dialog");
    await within(dialog).findByText("Keys can be replaced but never read back.");
    // The manifests load after the drawer.
    if (keyless) await within(dialog).findByText("Requests go out without an API key. Add one if the server needs it.");
    else await within(dialog).findByLabelText("API key");
    expect(within(dialog).queryByText("Requests go out without an API key. Add one if the server needs it.") !== null).toBe(keyless);
  });
});

describe("diffDraft", () => {
  it("sends only what changed, with plain-word switches mapped to the API's fields", () => {
    const u: Upstream = fx.upstream({ id: "a", name: "A", config: { resource: "r1" }, quota_probe: "15m" });
    const before = toDraft(u);
    expect(diffDraft(before, before)).toEqual({});
    const after = {
      ...before,
      allow_private: true,
      config: { resource: "" },
      probe_mode: "off" as const,
      connect: "2.5",
    };
    expect(diffDraft(before, after)).toEqual({
      config: { resource: null, allow_private: true },
      quota_probe: "off",
      timeouts: { connect_ms: 2500, first_byte_ms: 0, idle_ms: 0, total_ms: 0 },
    });
    expect(diffDraft(before, { ...before, probe_mode: "default" })).toEqual({ quota_probe: "" });
  });
});

describe("an upstream without credentials", () => {
  it.each([
    ["en", "Not routed: needs a key", "No key needed"],
    ["zh-CN", "缺 API Key，请求不会发到这里", "不需要密钥"],
  ] as const)("says whether it is routed (%s)", async (locale, needs, keyless) => {
    const cloud = fx.upstream({ id: "cloud", name: "Cloud", credentials: [] });
    const local = fx.upstream({ id: "local", name: "Local", source: { kind: "official", name: "litellm" }, credentials: [], key_optional: true });
    renderPage(<UpstreamsPage />, { path: "/upstreams", routes: fixtures({ "GET /upstreams": { items: [cloud, local] } }), locale });
    const cloudRow = (await screen.findByText("Cloud")).closest("tr")!;
    expect(within(cloudRow).getByText(needs)).toBeInTheDocument();
    expect(within(screen.getByText("Local").closest("tr")!).getByText(keyless)).toBeInTheDocument();
    expectPlainWords();
  });
});

describe("upstreams in plain words (zh-CN)", () => {
  it("shows the list and every drawer tab without raw ids or keys", async () => {
    renderPage(<UpstreamsPage />, { path: "/upstreams", routes: fixtures(), locale: "zh-CN" });
    expect(await screen.findByText("Anthropic main")).toBeInTheDocument();
    expectPlainWords();
  });

  it.each(["anthropic", "bedrock", "openai"])("shows every drawer tab of %s without raw ids or keys", async (id) => {
    const { user } = renderPage(<UpstreamsPage />, {
      path: `/upstreams?id=${id}`,
      routes: fixtures(),
      locale: "zh-CN",
    });
    await screen.findByRole("tab", { name: translate("zh-CN", "upstreams.tab.overview") });
    const drawer = screen.getByRole("dialog");
    expectPlainWords(drawer);
    for (const tab of ["upstreams.tab.credentials", "upstreams.tab.models", "upstreams.tab.advanced"] as const) {
      await user.click(within(drawer).getByRole("tab", { name: new RegExp(translate("zh-CN", tab)) }));
      expectPlainWords(drawer);
    }
  });
});
