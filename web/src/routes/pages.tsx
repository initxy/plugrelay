import { createRoute, lazyRouteComponent } from "@tanstack/react-router";
import { parseLogFilters, parseLogView, type LogFilters } from "@/lib/url-filters";
import { appRoute } from "./app";
import { catalogSearch, idSearch, str } from "./search";

export const overviewRoute = createRoute({
  getParentRoute: () => appRoute,
  path: "/",
  component: lazyRouteComponent(() => import("@/features/overview/overview-page"), "OverviewPage"),
});

export const keysRoute = createRoute({
  getParentRoute: () => appRoute,
  path: "keys",
  validateSearch: (s: Record<string, unknown>): { id?: string; q?: string; status?: string; owner?: string; connect?: string } => ({
    ...idSearch(s),
    ...(str(s.q) ? { q: str(s.q) } : {}),
    ...(str(s.status) ? { status: str(s.status) } : {}),
    ...(str(s.owner) ? { owner: str(s.owner) } : {}),
    ...(str(s.connect) ? { connect: str(s.connect) } : {}),
  }),
  component: lazyRouteComponent(() => import("@/features/keys/keys-page"), "KeysPage"),
});

export const logsRoute = createRoute({
  getParentRoute: () => appRoute,
  path: "logs",
  validateSearch: (s: Record<string, unknown>): LogFilters & { id?: string; view?: "sessions" } => ({
    ...parseLogFilters(s),
    ...parseLogView(s),
    ...idSearch(s),
  }),
  component: lazyRouteComponent(() => import("@/features/logs/logs-page"), "LogsPage"),
});

export const upstreamsRoute = createRoute({
  getParentRoute: () => appRoute,
  path: "upstreams",
  validateSearch: (s: Record<string, unknown>): { id?: string; tab?: string; q?: string; attention?: true } => ({
    ...idSearch(s),
    ...(str(s.tab) ? { tab: str(s.tab) } : {}),
    ...(str(s.q) ? { q: str(s.q) } : {}),
    ...(s.attention === true || s.attention === "true" || s.attention === "1" ? { attention: true as const } : {}),
  }),
  component: lazyRouteComponent(() => import("@/features/upstreams/upstreams-page"), "UpstreamsPage"),
});

export const upstreamsNewRoute = createRoute({
  getParentRoute: () => appRoute,
  path: "upstreams/new",
  validateSearch: (s: Record<string, unknown>): { q?: string; cat?: string; m?: string } => ({
    ...(str(s.q) ? { q: str(s.q) } : {}),
    ...(str(s.cat) ? { cat: str(s.cat) } : {}),
    ...(str(s.m) ? { m: str(s.m) } : {}),
  }),
  component: lazyRouteComponent(() => import("@/features/upstreams/add-upstream-page"), "AddUpstreamPage"),
});

export const modelsRoute = createRoute({
  getParentRoute: () => appRoute,
  path: "models",
  validateSearch: (s: Record<string, unknown>): { id?: string; tab?: "pricing" | "presets"; q?: string; upstream?: string; state?: string } => ({
    ...idSearch(s),
    ...(s.tab === "pricing" || s.tab === "presets" ? { tab: s.tab } : {}),
    ...(str(s.q) ? { q: str(s.q) } : {}),
    ...(str(s.upstream) ? { upstream: str(s.upstream) } : {}),
    ...(str(s.state) ? { state: str(s.state) } : {}),
  }),
  component: lazyRouteComponent(() => import("@/features/models/models-page"), "ModelsPage"),
});

export const usersRoute = createRoute({
  getParentRoute: () => appRoute,
  path: "users",
  validateSearch: (
    s: Record<string, unknown>,
  ): { id?: string; tab?: "users" | "groups"; q?: string; role?: string; in_group?: string } => ({
    ...idSearch(s),
    ...(s.tab === "groups" ? { tab: "groups" as const } : {}),
    ...(str(s.q) ? { q: str(s.q) } : {}),
    ...(str(s.role) ? { role: str(s.role) } : {}),
    ...(str(s.in_group) ? { in_group: str(s.in_group) } : {}),
  }),
  component: lazyRouteComponent(() => import("@/features/users/users-page"), "UsersPage"),
});

export const featuresRoute = createRoute({
  getParentRoute: () => appRoute,
  path: "features",
  validateSearch: (s: Record<string, unknown>): { id?: string; tab?: "extensions" } => ({
    ...idSearch(s),
    ...(s.tab === "extensions" ? { tab: "extensions" as const } : {}),
  }),
  component: lazyRouteComponent(() => import("@/features/features/features-page"), "FeaturesPage"),
});

export const mcpRoute = createRoute({
  getParentRoute: () => appRoute,
  path: "mcp",
  component: lazyRouteComponent(() => import("@/features/mcp/mcp-page"), "McpPage"),
});

export const notificationsRoute = createRoute({
  getParentRoute: () => appRoute,
  path: "notifications",
  component: lazyRouteComponent(() => import("@/features/notifications/notifications-page"), "NotificationsPage"),
});

export const ssoRoute = createRoute({
  getParentRoute: () => appRoute,
  path: "sso",
  component: lazyRouteComponent(() => import("@/features/auth/sso-page"), "SsoPage"),
});

/** Every shared component in both themes, for design review. Not in production builds. */
export const devComponentsRoute = createRoute({
  getParentRoute: () => appRoute,
  path: "dev/components",
  validateSearch: (s: Record<string, unknown>): { id?: string; status?: string; model?: string; failed?: true; q?: string } => ({
    ...idSearch(s),
    ...(str(s.status) ? { status: str(s.status) } : {}),
    ...(str(s.model) ? { model: str(s.model) } : {}),
    ...(s.failed === true || s.failed === "true" ? { failed: true as const } : {}),
    ...(str(s.q) ? { q: str(s.q) } : {}),
  }),
  component: import.meta.env.DEV ? lazyRouteComponent(() => import("@/features/dev/components-page"), "ComponentsPage") : () => null,
});

export const billingRoute = createRoute({
  getParentRoute: () => appRoute,
  path: "billing",
  component: lazyRouteComponent(() => import("@/features/billing/routes"), "BillingIndex"),
});

export const billingPageRoute = createRoute({
  getParentRoute: () => appRoute,
  path: "billing/$page",
  component: lazyRouteComponent(() => import("@/features/billing/routes"), "BillingPageRoute"),
});

export const auditRoute = createRoute({
  getParentRoute: () => appRoute,
  path: "audit",
  validateSearch: (s: Record<string, unknown>): { id?: string; cat?: string; actor?: string; type?: string; target?: string } => {
    const out: Record<string, string> = {};
    for (const k of ["id", "cat", "actor", "type", "target"]) {
      const v = str(s[k]);
      if (v) out[k] = v;
    }
    return out;
  },
  component: lazyRouteComponent(() => import("@/features/audit/audit-page"), "AuditPage"),
});

export const settingsRoute = createRoute({
  getParentRoute: () => appRoute,
  path: "settings",
  validateSearch: (s: Record<string, unknown>): { section?: string } => (str(s.section) ? { section: str(s.section) } : {}),
  component: lazyRouteComponent(() => import("@/features/settings/settings-page"), "SettingsPage"),
});

export const analyticsRoute = createRoute({
  getParentRoute: () => appRoute,
  path: "analytics",
  component: lazyRouteComponent(() => import("@/features/analytics/analytics-page"), "AnalyticsPage"),
});

export const usageRoute = createRoute({
  getParentRoute: () => appRoute,
  path: "usage",
  validateSearch: (s: Record<string, unknown>): LogFilters & { id?: string; view?: "sessions" } => ({
    ...parseLogFilters(s),
    ...parseLogView(s),
    ...idSearch(s),
  }),
  component: lazyRouteComponent(() => import("@/features/usage/usage-page"), "UsagePage"),
});

export const playgroundRoute = createRoute({
  getParentRoute: () => appRoute,
  path: "playground",
  component: lazyRouteComponent(() => import("@/features/playground/playground-page"), "PlaygroundPage"),
});

export const catalogRoute = createRoute({
  getParentRoute: () => appRoute,
  path: "catalog",
  validateSearch: catalogSearch,
  component: lazyRouteComponent(() => import("@/features/catalog/catalog-page"), "CatalogPage"),
});

export const statusRoute = createRoute({
  getParentRoute: () => appRoute,
  path: "status",
  component: lazyRouteComponent(() => import("@/features/status/status-page"), "StatusPage"),
});

export const securityRoute = createRoute({
  getParentRoute: () => appRoute,
  path: "security",
  component: lazyRouteComponent(() => import("@/features/auth/security-page"), "SecurityPage"),
});
