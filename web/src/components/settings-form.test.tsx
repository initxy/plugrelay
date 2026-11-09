import { screen, waitFor } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import type { Setting } from "@/lib/api/types";
import { renderPage } from "@/test/api";
import { expectPlainWords } from "@/test/plain-words";
import { LayerSettingsTab } from "./layer-settings-tab";
import { SettingsForm } from "./settings-form";

const text = (en: string, zh = en) => ({ en, "zh-CN": zh });

function setting(over: Partial<Setting> & Pick<Setting, "module" | "key">): Setting {
  return { type: "int", label: text(over.key), help: text(""), scopes: ["global"], merge: "override", value: null, ...over };
}

const site: Setting[] = [
  setting({ module: "middleware.ratelimit", key: "rpm", label: text("Requests per minute", "每分钟请求数"), merge: "strictest", default: 0 }),
  setting({ module: "middleware.ratelimit", key: "thinking", type: "bool", label: text("Thinking"), default: true, restart: true }),
  setting({ module: "plugin.x", key: "api_key", type: "secret", label: text("API key"), secret_set: true }),
  setting({ module: "plugin.x", key: "region", type: "string", label: text("Region"), value: "eu" }),
];

describe("SettingsForm, site-wide", () => {
  it("shows one control per setting with defaults, restarts and write-only secrets", async () => {
    renderPage(<SettingsForm items={site} layer="global" saved="Saved" save={async () => {}} />);
    expect(await screen.findByLabelText("Requests per minute")).toHaveAttribute("placeholder", "0");
    expect(screen.getByRole("switch", { name: "Thinking" })).toBeChecked();
    expect(screen.getByText("Applies after restart")).toBeInTheDocument();
    // Nothing is above the site layer to tighten.
    expect(screen.queryByText("can only tighten")).not.toBeInTheDocument();
    expect(screen.getByLabelText("API key")).toHaveAttribute("placeholder", "Saved · type to replace");
    expect(screen.getByLabelText("API key")).toHaveValue("");
    expect(screen.getByLabelText("Region")).toHaveValue("eu");
  });

  it("checks numbers before saving and then sends every setting shown", async () => {
    const save = vi.fn(async () => {});
    const { user } = renderPage(<SettingsForm items={site} layer="global" saved="Saved" save={save} />);
    const rpm = await screen.findByLabelText("Requests per minute");
    await user.type(rpm, "-1");
    await user.click(screen.getByRole("button", { name: "Save" }));
    expect(await screen.findByText("Must be at least 0")).toBeInTheDocument();
    expect(save).not.toHaveBeenCalled();

    await user.clear(rpm);
    await user.type(rpm, "60");
    await user.click(screen.getByRole("switch", { name: "Thinking" }));
    await user.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(save).toHaveBeenCalledTimes(1));
    expect(save).toHaveBeenCalledWith({
      "middleware.ratelimit": { rpm: 60, thinking: false },
      "plugin.x": { api_key: null, region: "eu" },
    });
  });

  it("uses the module's own translation", async () => {
    renderPage(<SettingsForm items={site} layer="global" saved="Saved" save={async () => {}} />, { locale: "zh-CN" });
    expect(await screen.findByLabelText("每分钟请求数")).toBeInTheDocument();
  });
});

const groupLayer: Setting[] = [
  setting({
    module: "middleware.ratelimit",
    key: "rpm",
    label: text("Requests per minute"),
    scopes: ["global", "group"],
    merge: "strictest",
    feature: "limits",
    effective: { value: 120, source: "global" },
  }),
  setting({ module: "middleware.ratelimit", key: "burst", label: text("Burst"), scopes: ["global", "group"], feature: "limits" }),
  setting({ module: "mcp.servers", key: "allowed", type: "list", label: text("MCP servers allowed"), scopes: ["group"], feature: "mcp" }),
];

describe("LayerSettingsTab", () => {
  it("follows the layer above, hides settings of features that are off and sends only what changed", async () => {
    const { user, api } = renderPage(<LayerSettingsTab layer="group" id="g_vip" hint="What this group's members get." saved="Saved" onDirty={() => {}} />, {
      routes: { "GET /groups/{id}/settings": { items: groupLayer }, "PUT /groups/{id}/settings": { items: groupLayer } },
    });
    expect(await screen.findByText("What this group's members get.")).toBeInTheDocument();
    expect(screen.getByText(/Now: 120 \(Site\)\./)).toBeInTheDocument();
    expect(screen.getByText("can only tighten")).toBeInTheDocument();
    expect(screen.queryByText("MCP servers allowed")).not.toBeInTheDocument();
    await user.type(screen.getByLabelText("Burst"), "5");
    await user.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(api.called("PUT /groups/{id}/settings")).toHaveLength(1));
    expect(api.called("PUT /groups/{id}/settings")[0]!.body).toEqual({ settings: { "middleware.ratelimit": { burst: 5 } } });
  });

  it("says so when nothing is left to set", async () => {
    renderPage(<LayerSettingsTab layer="key" id="k_1" hint="" saved="Saved" onDirty={() => {}} />, {
      routes: { "GET /keys/{id}/settings": { items: [groupLayer[2]!] } },
    });
    expect(await screen.findByText("Nothing else to set here")).toBeInTheDocument();
  });
});

const clientsLayer: Setting[] = [
  setting({
    module: "middleware.access",
    key: "allowed_clients",
    type: "list",
    label: text("Allowed clients", "允许的客户端"),
    scopes: ["group"],
    merge: "strictest",
    enum: ["claude-code", "codex", "other"],
    enum_labels: { "claude-code": text("Claude Code"), codex: text("Codex"), other: text("Other clients", "其他客户端") },
    value: ["claude-code"],
  }),
];

describe("a list setting with allowed values", () => {
  it.each([
    ["en", "Other clients", "Save"],
    ["zh-CN", "其他客户端", "保存"],
  ] as const)("is picked by label, never by raw id (%s)", async (locale, other, save) => {
    const { user, api } = renderPage(<LayerSettingsTab layer="group" id="g_vip" hint="" saved="Saved" onDirty={() => {}} />, {
      routes: { "GET /groups/{id}/settings": { items: clientsLayer }, "PUT /groups/{id}/settings": { items: clientsLayer } },
      locale,
    });
    expect(await screen.findByRole("checkbox", { name: "Claude Code" })).toHaveAttribute("aria-checked", "true");
    expect(screen.getByRole("checkbox", { name: "Codex" })).toHaveAttribute("aria-checked", "false");
    for (const id of ["claude-code", "codex", "other"]) expect(screen.queryByText(id)).not.toBeInTheDocument();
    expectPlainWords();
    await user.click(screen.getByRole("checkbox", { name: other }));
    await user.click(screen.getByRole("button", { name: save }));
    await waitFor(() => expect(api.called("PUT /groups/{id}/settings")).toHaveLength(1));
    expect(api.called("PUT /groups/{id}/settings")[0]!.body).toEqual({ settings: { "middleware.access": { allowed_clients: ["claude-code", "other"] } } });
  });
});

describe("a saved value that does not fit its setting", () => {
  it.each([
    ["en", "This saved value is invalid, so nobody is allowed. Save again to fix it.", "This saved value is invalid and has no effect. Save again to fix it."],
    ["zh-CN", "保存的值无效，所以谁都不放行。重新保存一次就好。", "保存的值无效，现在不起作用。重新保存一次就好。"],
  ] as const)("says what the relay makes of it until it is changed (%s)", async (locale, denyAll, ignored) => {
    const items: Setting[] = [
      { ...clientsLayer[0]!, value: "codex", invalid: true },
      setting({ module: "middleware.ratelimit", key: "rpm", label: text("Requests per minute", "每分钟请求数"), scopes: ["group"], merge: "strictest", value: "10", invalid: true }),
    ];
    const { user } = renderPage(<LayerSettingsTab layer="group" id="g_vip" hint="" saved="Saved" onDirty={() => {}} />, {
      routes: { "GET /groups/{id}/settings": { items } },
      locale,
    });
    expect(await screen.findByText(denyAll)).toBeInTheDocument();
    expect(screen.getByText(ignored)).toBeInTheDocument();
    expectPlainWords();
    await user.click(screen.getByRole("checkbox", { name: "Codex" }));
    expect(screen.queryByText(denyAll)).not.toBeInTheDocument();
  });
});
