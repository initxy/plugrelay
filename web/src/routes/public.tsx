import { createRoute, lazyRouteComponent, redirect } from "@tanstack/react-router";
import { setupStatusQuery } from "@/app/session";
import { rootRoute } from "./__root";
import { catalogSearch } from "./search";

export const setupRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "setup",
  beforeLoad: async ({ context }) => {
    const status = await context.queryClient.ensureQueryData(setupStatusQuery);
    if (!status.needs_setup) throw redirect({ to: "/" });
  },
  component: lazyRouteComponent(() => import("@/features/setup/setup-page"), "SetupPage"),
});

export const signupRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "signup",
  component: lazyRouteComponent(() => import("@/features/billing/signup"), "SignupPage"),
});

export const loginRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "login",
  validateSearch: (s: Record<string, unknown>): { next?: string; sso_error?: string; mfa?: string } => ({
    ...(typeof s.next === "string" && s.next.startsWith("/") && !s.next.startsWith("//") ? { next: s.next } : {}),
    ...(typeof s.sso_error === "string" ? { sso_error: s.sso_error } : {}),
    ...(s.mfa === "1" || s.mfa === 1 ? { mfa: "1" } : {}),
  }),
  component: lazyRouteComponent(() => import("@/features/auth/login-page"), "LoginPage"),
});

/** The public models-and-prices page (feature "public pages"). */
export const publicModelsRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "public/models",
  validateSearch: catalogSearch,
  component: lazyRouteComponent(() => import("@/features/public/public-pages"), "PublicModelsPage"),
});

export const publicStatusRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "public/status",
  component: lazyRouteComponent(() => import("@/features/public/public-pages"), "PublicStatusPage"),
});
