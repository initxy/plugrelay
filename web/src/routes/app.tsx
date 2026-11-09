import { createRoute, redirect } from "@tanstack/react-router";
import { AppShell } from "@/app/shell";
import { meQuery, setupStatusQuery } from "@/app/session";
import { ApiError } from "@/lib/api/errors";
import { rootRoute } from "./__root";

/** Layout for every signed-in page: first-run and login checks, then the shell. */
export const appRoute = createRoute({
  getParentRoute: () => rootRoute,
  id: "app",
  beforeLoad: async ({ context, location }) => {
    const status = await context.queryClient.ensureQueryData(setupStatusQuery);
    if (status.needs_setup) throw redirect({ to: "/setup" });
    try {
      await context.queryClient.ensureQueryData(meQuery);
    } catch (e) {
      if (e instanceof ApiError && e.status === 401) {
        throw redirect({ to: "/login", search: { next: location.href } });
      }
      throw e;
    }
  },
  component: AppShell,
});
