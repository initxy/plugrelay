import { screen, waitFor, within } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { apiError, adminMe, renderPage, type ApiRoutes } from "@/test/api";
import { translate } from "@/i18n";
import { expectPlainWords } from "@/test/plain-words";
import { McpPage } from "./mcp-page";
import type { McpServer } from "./mcp-settings";

const github: McpServer = {
  id: "github",
  name: "GitHub",
  url: "https://api.githubcopilot.com/mcp/",
  transport: "auto",
  enabled: true,
  secret_headers: ["Authorization"],
};
const on = adminMe({
  pages: ["mcp.servers"],
  site: {
    name: "plugrelay",
    currency: "USD",
    show_upstream_names: true,
    public_base_url: "https://relay.example.com",
  },
});

function routes() {
  const items: McpServer[] = [github];
  const routes: ApiRoutes = {
    "GET /mcp/servers": () => ({ items }),
    "POST /mcp/servers": ({ body }) => {
      if (!body.id)
        return apiError(422, "invalid", "id: use 1-32 lowercase letters, digits or dashes", {
          params: { count: 1 },
          fields: [{ path: "id", code: "id_format", message: "use 1-32 lowercase letters, digits or dashes" }],
        });
      const s: McpServer = {
        ...body,
        id: body.id,
        url: body.url ?? "",
        secret_headers: Object.keys(body.secret_headers ?? {}),
      };
      items.push(s);
      return s;
    },
    "POST /mcp/servers/{id}/test": { ok: true, results: [{ ok: true, tools: ["a", "b"], latency_ms: 120 }] },
  };
  return { items, routes };
}

describe("McpPage", () => {
  it("shows the address clients use and each server, once titled", async () => {
    renderPage(<McpPage />, { locale: "zh-CN", routes: routes().routes, me: on });
    expect(await screen.findByText("https://api.githubcopilot.com/mcp/")).toBeInTheDocument();
    expect(screen.getByText("https://relay.example.com/mcp")).toBeInTheDocument();
    expect(screen.getAllByRole("heading", { level: 1 })).toHaveLength(1);
    expectPlainWords();
  });

  it("adds a server with a secret Authorization header", async () => {
    const r = routes();
    const { user } = renderPage(<McpPage />, { routes: r.routes, me: on, locale: "en" });
    await screen.findByText(github.url);
    await user.click(screen.getByRole("button", { name: "Add server" }));
    const drawer = await screen.findByRole("dialog");
    await user.click(within(drawer).getByRole("button", { name: "Save" }));
    expect(await within(drawer).findByText("Use 1–32 lowercase letters, digits or dashes.")).toBeInTheDocument();
    await user.type(within(drawer).getByLabelText("Server ID"), "docs");
    await user.type(within(drawer).getByLabelText("Server URL"), "http://127.0.0.1:9000/mcp");
    await user.type(within(drawer).getByLabelText("Authorization header"), "Bearer xyz");
    await user.type(within(drawer).getByLabelText("Other headers"), "X-Team: eng");
    await user.click(within(drawer).getByRole("switch", { name: "Allow private address" }));
    await user.click(within(drawer).getByRole("button", { name: "Save" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    expect(await screen.findByText("http://127.0.0.1:9000/mcp")).toBeInTheDocument();
    expect(r.items.at(-1)).toMatchObject({
      id: "docs",
      allow_private: true,
      headers: { "X-Team": "eng" },
      secret_headers: ["Authorization"],
    });
  });

  it("tests a server and keeps the result on its row", async () => {
    const { user } = renderPage(<McpPage />, { routes: routes().routes, me: on, locale: "en" });
    await screen.findByText(github.url);
    await user.click(screen.getByRole("button", { name: "Test" }));
    expect(await screen.findByText("Connected · 2 tools · 120 ms")).toBeInTheDocument();
  });
});

describe("McpPage drawers in plain words (zh-CN)", () => {
  it("shows the new and the edit drawer without raw ids or keys", async () => {
    const { user } = renderPage(<McpPage />, { routes: routes().routes, me: on, locale: "zh-CN" });
    await screen.findByText(github.url);
    await user.click(screen.getByRole("button", { name: translate("zh-CN", "mcp.add") }));
    expectPlainWords(await screen.findByRole("dialog"));
    await user.click(within(screen.getByRole("dialog")).getByRole("button", { name: translate("zh-CN", "common.close") }));
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    await user.click(screen.getByRole("button", { name: translate("zh-CN", "common.edit") }));
    expectPlainWords(await screen.findByRole("dialog"));
  });
});
