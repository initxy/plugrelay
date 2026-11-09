import { MutationCache, QueryCache, QueryClient } from "@tanstack/react-query";
import { createRouter } from "@tanstack/react-router";
import { ErrorState } from "@/components/page";
import { rootRoute } from "@/routes/__root";
import { appRoute } from "@/routes/app";
import {
  analyticsRoute,
  auditRoute,
  catalogRoute,
  keysRoute,
  logsRoute,
  modelsRoute,
  overviewRoute,
  playgroundRoute,
  billingPageRoute,
  billingRoute,
  devComponentsRoute,
  featuresRoute,
  mcpRoute,
  ssoRoute,
  notificationsRoute,
  securityRoute,
  settingsRoute,
  statusRoute,
  upstreamsNewRoute,
  upstreamsRoute,
  usageRoute,
  usersRoute,
} from "@/routes/pages";
import { loginRoute, publicModelsRoute, publicStatusRoute, setupRoute, signupRoute } from "@/routes/public";
import { meQuery } from "@/app/session";
import { ApiError } from "@/lib/api/errors";

const routeTree = rootRoute.addChildren([
  setupRoute,
  loginRoute,
  signupRoute,
  publicModelsRoute,
  publicStatusRoute,
  appRoute.addChildren([
    overviewRoute,
    keysRoute,
    playgroundRoute,
    logsRoute,
    analyticsRoute,
    upstreamsRoute,
    upstreamsNewRoute,
    modelsRoute,
    usersRoute,
    featuresRoute,
    mcpRoute,
    ssoRoute,
    notificationsRoute,
    ...(import.meta.env.DEV ? [devComponentsRoute] : []),
    billingRoute,
    billingPageRoute,
    auditRoute,
    settingsRoute,
    usageRoute,
    catalogRoute,
    statusRoute,
    securityRoute,
  ]),
]);

export function createAppRouter(queryClient: QueryClient) {
  return createRouter({
    routeTree,
    context: { queryClient },
    defaultPreload: "intent",
    defaultErrorComponent: ({ error, reset }) => (
      <div className="p-6">
        <ErrorState error={error} onRetry={reset} />
      </div>
    ),
  });
}

/**
 * onUnauthorized runs when any request answers 401 after the console had a
 * signed-in user: the session expired or was revoked elsewhere.
 */
export function createQueryClient(onUnauthorized?: () => void): QueryClient {
  const onError = (err: unknown) => {
    if (err instanceof ApiError && err.status === 401 && client.getQueryData(meQuery.queryKey)) onUnauthorized?.();
  };
  const client: QueryClient = new QueryClient({
    queryCache: new QueryCache({ onError }),
    mutationCache: new MutationCache({ onError }),
    defaultOptions: {
      queries: {
        staleTime: 15_000,
        refetchOnWindowFocus: false,
        retry: (count, err) => !(err instanceof ApiError && err.status < 500) && count < 2,
      },
    },
  });
  return client;
}

declare module "@tanstack/react-router" {
  interface Register {
    router: ReturnType<typeof createAppRouter>;
  }
}
