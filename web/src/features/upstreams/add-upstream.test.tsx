import { screen, waitFor, within } from "@testing-library/react";
import { beforeAll, describe, expect, it } from "vitest";
import { renderPage, type ApiRoutes } from "@/test/api";
import { AddUpstreamPage } from "./add-upstream-page";
import { splitSecrets } from "./labels";
import { expectPlainWords } from "@/test/plain-words";
import * as fx from "./upstreams.fixtures";

// The wide layout (gallery with the form beside it).
beforeAll(() => {
  window.matchMedia = ((q: string) => ({
    matches: true,
    media: q,
    addEventListener() {},
    removeEventListener() {},
  })) as unknown as typeof window.matchMedia;
});

function fixtures(testOk: boolean): ApiRoutes {
  return {
    "GET /manifests": { items: fx.manifests() },
    "POST /upstreams": (c) => fx.upstream({ id: "deepseek", name: c.body.name ?? "deepseek", enabled: false }),
    "PATCH /upstreams/{id}": (c) => fx.upstream({ id: "deepseek", name: "deepseek", ...(c.body as object) }),
    "DELETE /upstreams/{id}": undefined,
    "DELETE /credentials/{id}": undefined,
    "POST /upstreams/{id}/credentials": (c) => fx.cred({ id: "k" + Math.random(), label: c.body.label }),
    "POST /upstreams/{id}/test": testOk
      ? { ok: true, results: [{ ok: true, ttfb_ms: 420, model: "deepseek-flash" }] }
      : {
          ok: false,
          results: [{ ok: false, status: 401, error: "invalid api key", model: "deepseek-flash" }],
        },
  };
}

describe("add-upstream gallery", () => {
  it("groups providers, filters by category and searches local names", async () => {
    const r = fixtures(true);
    const { user } = renderPage(<AddUpstreamPage />, {
      path: "/upstreams/new",
      routes: r,
      locale: "zh-CN",
    });
    expect(await screen.findByText("常用")).toBeInTheDocument();
    expectPlainWords();
    const cats = screen.getByRole("group", { name: "分类" });
    await user.click(within(cats).getByRole("button", { name: /自己部署/ }));
    await waitFor(() => expect(screen.queryByText("DeepSeek")).not.toBeInTheDocument());
    expect(screen.getByText("Ollama")).toBeInTheDocument();
    await user.click(within(cats).getByRole("button", { name: /^全部/ }));
    await user.type(screen.getByRole("searchbox"), "火山");
    await waitFor(() => expect(screen.queryByText("Ollama")).not.toBeInTheDocument());
    expect(screen.getByText("火山方舟")).toBeInTheDocument();
  });

  it("stores the upstream off, adds one credential per pasted key, tests, then turns it on", async () => {
    const r = fixtures(true);
    const { user, router, api: f } = renderPage(<AddUpstreamPage />, { path: "/upstreams/new?m=deepseek", routes: r });
    const form = (await screen.findByRole("button", { name: "Test and save" })).closest("form")!;
    await user.type(within(form).getByLabelText("API key"), "sk-aaa{Enter}sk-bbb");
    expect(within(form).getByText("2 keys: adds 2 credentials")).toBeInTheDocument();
    await user.click(within(form).getByRole("button", { name: "Test and save" }));
    await waitFor(() => expect(router.state.location.search).toMatchObject({ id: "deepseek" }));
    const seq = f.calls.filter((c) => c.method !== "GET").map((c) => `${c.method} ${c.path}`);
    expect(seq).toEqual([
      "POST /upstreams",
      "POST /upstreams/deepseek/credentials",
      "POST /upstreams/deepseek/credentials",
      "POST /upstreams/deepseek/test",
      "PATCH /upstreams/deepseek",
    ]);
    expect(f.calls[seq.indexOf("POST /upstreams") + 1]?.body).toMatchObject({
      manifest: "deepseek",
      enabled: false,
      model_sync: true,
    });
    expect(f.calls.find((c) => c.method === "PATCH")?.body).toEqual({ enabled: true });
  });

  it("keeps nothing when the test fails, unless the operator saves anyway", async () => {
    const r = fixtures(false);
    const { user, api: f } = renderPage(<AddUpstreamPage />, {
      path: "/upstreams/new?m=deepseek",
      routes: r,
    });
    const form = (await screen.findByRole("button", { name: "Test and save" })).closest("form")!;
    await user.type(within(form).getByLabelText("API key"), "sk-bad");
    await user.click(within(form).getByRole("button", { name: "Test and save" }));
    expect(await within(form).findByText(/1 of 1 keys failed; nothing is saved yet/)).toBeInTheDocument();
    expect(within(form).getByText("invalid api key")).toBeInTheDocument();
    expect(f.calls.some((c) => c.method === "PATCH")).toBe(false);
    await user.click(within(form).getByRole("button", { name: "Save anyway" }));
    await waitFor(() => expect(f.calls.find((c) => c.method === "PATCH")?.body).toEqual({ enabled: true }));
  });

  it("deletes the stored draft when the operator gives up after a failed test", async () => {
    const r = fixtures(false);
    const { user, api: f } = renderPage(<AddUpstreamPage />, {
      path: "/upstreams/new?m=deepseek",
      routes: r,
    });
    const form = (await screen.findByRole("button", { name: "Test and save" })).closest("form")!;
    await user.type(within(form).getByLabelText("API key"), "sk-bad");
    await user.click(within(form).getByRole("button", { name: "Test and save" }));
    await within(form).findByText(/nothing is saved yet/);
    await user.click(within(form).getByRole("button", { name: "Cancel" }));
    await waitFor(() => expect(f.calls.some((c) => c.method === "DELETE" && c.path === "/upstreams/deepseek")).toBe(true));
  });

  it("asks for required provider settings by their plain name", async () => {
    const r = fixtures(true);
    const { user, api: f } = renderPage(<AddUpstreamPage />, {
      path: "/upstreams/new?m=azure-openai",
      routes: r,
    });
    const form = (await screen.findByRole("button", { name: "Test and save" })).closest("form")!;
    expect(within(form).getByText("Resource name")).toBeInTheDocument();
    expect(within(form).queryByText("resource")).not.toBeInTheDocument();
    await user.type(within(form).getByLabelText("API key"), "k");
    await user.click(within(form).getByRole("button", { name: "Test and save" }));
    expect(await within(form).findByText("Required")).toBeInTheDocument();
    expect(f.calls.some((c) => c.method === "POST")).toBe(false);
  });
});

describe("keyless servers", () => {
  it("adds a self-hosted server without a key and without a credential", async () => {
    const { user, router, api: f } = renderPage(<AddUpstreamPage />, { path: "/upstreams/new?m=ollama", routes: fixtures(true) });
    const form = (await screen.findByRole("button", { name: "Test and save" })).closest("form")!;
    expect(within(form).getByLabelText("API key (optional)")).toHaveValue("");
    await user.click(within(form).getByRole("button", { name: "Test and save" }));
    await waitFor(() => expect(router.state.location.search).toMatchObject({ id: "deepseek" }));
    expect(f.calls.some((c) => c.method === "POST" && c.path.endsWith("/credentials"))).toBe(false);
    expect(f.calls.some((c) => c.method === "POST" && c.path.endsWith("/test"))).toBe(true);
  });

  it("asks a custom Anthropic server for its key: it is stored on a manifest that needs one", async () => {
    const { user, api: f } = renderPage(<AddUpstreamPage />, { path: "/upstreams/new?m=custom", routes: fixtures(true) });
    const form = (await screen.findByRole("button", { name: "Test and save" })).closest("form")!;
    expect(within(form).getByLabelText("API key (optional)")).toBeInTheDocument();
    await user.click(within(form).getByRole("radio", { name: "Anthropic-compatible" }));
    await user.type(within(form).getByLabelText("Base URL"), "https://llm.example.com");
    await user.click(within(form).getByRole("button", { name: "Test and save" }));
    expect(await within(form).findByText("Required")).toBeInTheDocument();
    expect(f.calls.some((c) => c.method === "POST")).toBe(false);
  });
});

describe("splitSecrets", () => {
  it("splits keys by line but keeps a service-account JSON whole", () => {
    expect(splitSecrets(" a \n\n b\r\n", "key")).toEqual(["a", "b"]);
    expect(splitSecrets('{\n "type": "service_account"\n}', "gcp")).toEqual(['{\n "type": "service_account"\n}']);
  });
});

describe("add-upstream form in plain words (zh-CN)", () => {
  it.each(["deepseek", "azure-openai"])("shows the %s form without raw keys", async (m) => {
    renderPage(<AddUpstreamPage />, {
      path: `/upstreams/new?m=${m}`,
      routes: fixtures(true),
      locale: "zh-CN",
    });
    await screen.findByRole("button", { name: /测试并保存|保存/ });
    expectPlainWords();
  });
});
