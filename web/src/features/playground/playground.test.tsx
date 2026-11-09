import { screen, waitFor } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { memberMe, renderPage, type ApiBody, type ApiRoutes } from "@/test/api";
import { expectPlainWords } from "@/test/plain-words";
import type { Key } from "@/lib/api/types";
import { PlaygroundPage } from "./playground-page";
import { parseEvents } from "./stream";

const member = memberMe();
const key = (p: Partial<Key>): Key => ({
  id: "k1",
  name: "alice-laptop",
  last4: "3f9a",
  status: "active",
  created_at: "2026-11-01T00:00:00Z",
  month_spend: { micro: 0, currency: "USD" },
  settings: {},
  owner: { id: "u_alice", name: "Alice" },
  ...p,
});
const catalog: ApiBody<"GET /catalog"> = {
  items: [
    { model: "claude-sonnet-4-6", kind: "model", price: { currency: "USD", input: 3e6, cache_read: 0, cache_write_5m: 0, cache_write_1h: 0, output: 15e6 } },
    { model: "best", kind: "preset", price: { currency: "USD", input: 0, cache_read: 0, cache_write_5m: 0, cache_write_1h: 0, output: 0 }, preset_models: ["claude-sonnet-4-6"] },
  ],
};
const sse = (...frames: [string, object][]) =>
  new Response(frames.map(([e, d]) => `event: ${e}\ndata: ${JSON.stringify(d)}\n\n`).join(""), { headers: { "Content-Type": "text/event-stream" } });

function routes(extra: ApiRoutes = {}): ApiRoutes {
  return {
    "GET /keys": { items: [key({}), key({ id: "k2", name: "old", status: "disabled" })] },
    "GET /catalog": catalog,
    "POST /playground": () =>
      sse(
        ["start", { request_id: "req_1" }],
        ["reasoning", { text: "Let me think." }],
        ["text", { text: "Hel" }],
        ["text", { text: "lo!" }],
        ["done", { request_id: "req_1", ok: true, status: 200, ttfb_ms: 420, latency_ms: 1300, usage: { input: 12, cache_read: 0, cache_write_5m: 0, cache_write_1h: 0, output: 5, reasoning: 0 } }],
      ),
    "GET /logs/{id}": (r) =>
      ({ id: r.params.id, charge: { micro: 1234, currency: "USD" } }) as unknown as ApiBody<"GET /logs/{id}">,
    ...extra,
  };
}

describe("playground", () => {
  it("streams an answer through the relay with the member's key and links its log entry", async () => {
    const { user, api } = renderPage(<PlaygroundPage />, { path: "/playground", me: member, routes: routes() });
    const box = await screen.findByRole("textbox", { name: "Message" });
    await waitFor(() => expect(screen.getByRole("button", { name: "Send" })).toBeDisabled());
    await user.type(box, "hi there");
    await user.click(screen.getByRole("button", { name: "Send" }));
    expect(await screen.findByText("Hello!")).toBeInTheDocument();
    expect(screen.getByText("Let me think.")).toBeInTheDocument();
    expect(api.called("POST /playground")[0]!.body).toEqual({
      protocol: "anthropic",
      model: "claude-sonnet-4-6",
      key_id: "k1",
      max_tokens: 1024,
      messages: [{ role: "user", content: "hi there" }],
    });
    expect(await screen.findByText(/first token 420ms · total 1\.3s · 12 in \/ 5 out tokens · cost/)).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "View in request log" })).toHaveAttribute("href", "/usage?id=req_1");
  });

  it("links an operator's answer to the request log", async () => {
    const { user } = renderPage(<PlaygroundPage />, { path: "/playground", routes: routes() });
    await user.type(await screen.findByRole("textbox", { name: "Message" }), "hi");
    await user.click(screen.getByRole("button", { name: "Send" }));
    expect(await screen.findByRole("link", { name: "View in request log" })).toHaveAttribute("href", "/logs?id=req_1");
  });

  it("shows the relay's refusal and keeps the message", async () => {
    const r = routes({
      "POST /playground": () =>
        Response.json({ error: { code: "invalid", message: "1 fields are invalid", fields: [{ path: "key_id", code: "key_unusable", message: "this key is disabled or expired" }] } }, { status: 422 }),
    });
    const { user } = renderPage(<PlaygroundPage />, { path: "/playground", me: member, routes: r });
    await user.type(await screen.findByRole("textbox", { name: "Message" }), "hi");
    await user.click(screen.getByRole("button", { name: "Send" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("This key is disabled or expired.");
    expect(screen.getByRole("textbox", { name: "Message" })).toHaveValue("hi");
  });

  it("asks a member without a key to create one", async () => {
    renderPage(<PlaygroundPage />, { path: "/playground", me: member, routes: routes({ "GET /keys": { items: [] } }) });
    expect(await screen.findByText("You need a key first")).toBeInTheDocument();
  });

  it("reads server-sent events split anywhere", () => {
    const { events, rest } = parseEvents('event: text\ndata: {"text":"a"}\n\nevent: te');
    expect(events).toEqual([{ type: "text", text: "a" }]);
    expect(rest).toBe("event: te");
  });

  it("shows the page in plain words (zh-CN)", async () => {
    const { user } = renderPage(<PlaygroundPage />, { path: "/playground", me: member, routes: routes(), locale: "zh-CN" });
    await user.type(await screen.findByRole("textbox", { name: "消息" }), "你好");
    await user.click(screen.getByRole("button", { name: "发送" }));
    await screen.findByText("Hello!");
    await screen.findByText(/花费/);
    expectPlainWords();
  });
});
