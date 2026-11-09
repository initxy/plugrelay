import type { MessageKey } from "@/i18n/en";
import type { Me, Permission } from "@/lib/api/types";

type NavGroup = "overview" | "access" | "routing" | "observe" | "members" | "operations" | "system";

/** Menu order of the groups; the first one has no heading. */
const NAV_GROUPS: readonly NavGroup[] = ["overview", "access", "routing", "observe", "members", "operations", "system"];

export interface NavItem {
  id: string;
  to: string;
  label: MessageKey;
  icon: string;
  group: NavGroup;
}

interface Entry extends NavItem {
  /** Operators are admins; everyone else gets the end-user pages. */
  audience: "operator" | "user";
  permission?: Permission;
  /** The page id `/me.pages` lists while the feature that adds this page is on. */
  pageId?: string;
}

/**
 * Every console page in the menu, in order. This is the one place that maps the page ids of
 * `/me.pages` to routes.
 */
const MENU: readonly Entry[] = [
  // operators
  { audience: "operator", group: "overview", id: "overview", to: "/", label: "nav.overview", icon: "gauge" },
  { audience: "operator", group: "access", id: "keys", to: "/keys", label: "nav.keys", icon: "key" },
  { audience: "operator", group: "access", id: "playground", to: "/playground", label: "nav.playground", icon: "message" },
  { audience: "operator", group: "routing", id: "upstreams", to: "/upstreams", label: "nav.upstreams", icon: "server", permission: "upstreams.manage" },
  { audience: "operator", group: "routing", id: "models", to: "/models", label: "nav.models", icon: "route", permission: "models.manage" },
  { audience: "operator", group: "observe", id: "logs", to: "/logs", label: "nav.logs", icon: "list" },
  { audience: "operator", group: "observe", id: "analytics", to: "/analytics", label: "nav.analytics", icon: "chart" },
  { audience: "operator", group: "members", id: "users", to: "/users", label: "nav.users", icon: "users", permission: "users.manage" },
  { audience: "operator", group: "operations", id: "billing.balances", to: "/billing/balances", label: "nav.billing.balances", icon: "wallet", pageId: "billing.balances" },
  { audience: "operator", group: "operations", id: "billing.orders", to: "/billing/orders", label: "nav.billing.orders", icon: "receipt", pageId: "billing.orders" },
  { audience: "operator", group: "operations", id: "billing.plans", to: "/billing/plans", label: "nav.billing.plans", icon: "calendar", pageId: "billing.plans" },
  { audience: "operator", group: "operations", id: "billing.codes", to: "/billing/codes", label: "nav.billing.codes", icon: "ticket", pageId: "billing.codes" },
  { audience: "operator", group: "operations", id: "billing.coupons", to: "/billing/coupons", label: "nav.billing.coupons", icon: "percent", pageId: "billing.coupons" },
  { audience: "operator", group: "operations", id: "billing.announcements", to: "/billing/announcements", label: "nav.billing.announcements", icon: "megaphone", pageId: "billing.announcements" },
  { audience: "operator", group: "operations", id: "billing.import", to: "/billing/import", label: "nav.billing.import", icon: "package", pageId: "billing.import" },
  { audience: "operator", group: "operations", id: "mcp", to: "/mcp", label: "nav.mcp", icon: "plug", pageId: "mcp.servers" },
  { audience: "operator", group: "system", id: "features", to: "/features", label: "nav.features", icon: "features", permission: "features.manage" },
  { audience: "operator", group: "system", id: "sso", to: "/sso", label: "nav.sso", icon: "login", pageId: "sso.providers" },
  { audience: "operator", group: "system", id: "notifications", to: "/notifications", label: "nav.notifications", icon: "bell", pageId: "notifications.channels" },
  { audience: "operator", group: "system", id: "settings", to: "/settings", label: "nav.settings", icon: "settings", permission: "settings.manage" },
  { audience: "operator", group: "system", id: "audit", to: "/audit", label: "nav.audit", icon: "history", permission: "audit.read" },
  // everyone else
  { audience: "user", group: "overview", id: "overview", to: "/", label: "nav.overview", icon: "gauge" },
  { audience: "user", group: "access", id: "keys", to: "/keys", label: "nav.keys", icon: "key" },
  { audience: "user", group: "access", id: "catalog", to: "/catalog", label: "nav.catalog", icon: "tag" },
  { audience: "user", group: "access", id: "playground", to: "/playground", label: "nav.playground", icon: "message" },
  { audience: "user", group: "observe", id: "usage", to: "/usage", label: "nav.usage", icon: "chart" },
  { audience: "user", group: "observe", id: "status", to: "/status", label: "nav.status", icon: "activity" },
  { audience: "user", group: "operations", id: "billing.wallet", to: "/billing", label: "nav.billing.wallet", icon: "wallet", pageId: "billing.wallet" },
  { audience: "user", group: "operations", id: "billing.plan", to: "/billing/plan", label: "nav.billing.plan", icon: "calendar", pageId: "billing.plan" },
];

export function isOperator(me: Pick<Me, "user">): boolean {
  return me.user.role === "admin";
}

/** The page id of the feature page at `path`, if one lives there. */
export function pageIdAt(path: string): string | undefined {
  return MENU.find((e) => e.to === path && e.pageId)?.pageId;
}

/** The menu entry a feature page id adds, for the features page's "what appears when it is on". */
export function featurePage(pageId: string): { label: MessageKey; to: string; audience: "operator" | "user" } | undefined {
  const e = MENU.find((m) => m.pageId === pageId);
  return e && { label: e.label, to: e.to, audience: e.audience };
}

export interface NavSection {
  group: NavGroup;
  items: NavItem[];
}

/** The sidebar: one menu filtered by role, permissions and the feature pages /me lists, in groups. */
export function buildMenu(me: Pick<Me, "permissions" | "pages" | "user">): NavSection[] {
  const audience = isOperator(me) ? "operator" : "user";
  const visible = MENU.filter(
    (e) =>
      e.audience === audience &&
      (!e.permission || me.permissions.includes(e.permission)) &&
      (!e.pageId || me.pages.includes(e.pageId)),
  );
  return NAV_GROUPS.map((group) => ({
    group,
    items: visible.filter((e) => e.group === group).map(({ id, to, label, icon, group }) => ({ id, to, label, icon, group })),
  })).filter((s) => s.items.length > 0);
}

/** The menu as one list, for the command palette. */
export function menuItems(sections: readonly NavSection[]): NavItem[] {
  return sections.flatMap((s) => s.items);
}
