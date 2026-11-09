import { describe, expect, it } from "vitest";
import type { Me } from "@/lib/api/types";
import { buildMenu, menuItems, pageIdAt } from "./nav";

const ALL: Me["permissions"] = [
  "upstreams.manage",
  "models.manage",
  "users.manage",
  "features.manage",
  "settings.manage",
  "audit.read",
  "keys.manage_all",
  "logs.read_all",
];
const admin = (pages: string[] = []) => ({ pages, permissions: ALL, user: { id: "u1", username: "a", role: "admin" as const } });
const customer = (pages: string[] = []) => ({ pages, permissions: [], user: { id: "u4", username: "alice", role: "user" as const } });
const ids = (me: Parameters<typeof buildMenu>[0]) => menuItems(buildMenu(me)).map((i) => i.id);
const groups = (me: Parameters<typeof buildMenu>[0]) => buildMenu(me).map((s) => s.group);

describe("buildMenu", () => {
  it("operators get one grouped menu", () => {
    expect(groups(admin())).toEqual(["overview", "access", "routing", "observe", "members", "system"]);
    expect(ids(admin())).toEqual(["overview", "keys", "playground", "upstreams", "models", "logs", "analytics", "users", "features", "settings", "audit"]);
  });

  it("pages of features that are on appear in their group, at fixed routes", () => {
    const sections = buildMenu(admin(["billing.codes", "mcp.servers", "sso.providers"]));
    const ops = sections.find((s) => s.group === "operations")!;
    expect(ops.items.map((i) => [i.id, i.to])).toEqual([
      ["billing.codes", "/billing/codes"],
      ["mcp", "/mcp"],
    ]);
    expect(sections.find((s) => s.group === "system")!.items.map((i) => i.id)).toEqual(["features", "sso", "settings", "audit"]);
  });

  it("hides pages the caller has no permission for", () => {
    const limited = { ...admin(), permissions: ["audit.read"] as Me["permissions"] };
    expect(ids(limited)).toEqual(["overview", "keys", "playground", "logs", "analytics", "audit"]);
  });

  it("end users get their own pages plus the ones /me lists", () => {
    expect(ids(customer(["billing.wallet", "billing.plan"]))).toEqual([
      "overview", "keys", "catalog", "playground", "usage", "status", "billing.wallet", "billing.plan",
    ]);
  });

  it("operator pages never reach end users, even when listed", () => {
    expect(ids(customer(["billing.codes", "mcp.servers"]))).toEqual(["overview", "keys", "catalog", "playground", "usage", "status"]);
  });

  it("maps a route back to its page id", () => {
    expect(pageIdAt("/billing")).toBe("billing.wallet");
    expect(pageIdAt("/billing/codes")).toBe("billing.codes");
    expect(pageIdAt("/mcp")).toBe("mcp.servers");
    expect(pageIdAt("/keys")).toBeUndefined();
  });
});
