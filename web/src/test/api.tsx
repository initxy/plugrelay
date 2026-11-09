import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { createMemoryHistory, createRootRoute, createRoute, createRouter, Outlet, RouterProvider } from "@tanstack/react-router";
import { render } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { Suspense, type ReactElement } from "react";
import { Toaster } from "sonner";
import { meQuery } from "@/app/session";
import { ConfirmProvider } from "@/components/confirm";
import { TooltipProvider } from "@/components/ui/tooltip";
import { setFetch } from "@/lib/api/client";
import type { paths } from "@/lib/api/schema.gen";
import type { ApiErrorBody, Me } from "@/lib/api/types";
import { I18nProvider, type Locale } from "@/i18n";

// ---------------------------------------------------------------- route table types

type Method = "get" | "put" | "post" | "delete" | "patch";
type Op<P extends keyof paths, M extends Method> = NonNullable<paths[P][M]>;

/** "GET /upstreams/{id}" for every operation the OpenAPI document declares. */
type ApiRoute = {
  [P in keyof paths]: {
    [M in Method]: [NonNullable<paths[P][M]>] extends [never] ? never : `${Uppercase<M>} ${P & string}`;
  }[Method];
}[keyof paths];

type Split<R> = R extends `${infer M} ${infer P}` ? [Lowercase<M> & Method, P & keyof paths] : never;
type OpOf<R extends ApiRoute> = Split<R> extends [infer M extends Method, infer P extends keyof paths] ? Op<P, M> : never;

type JsonOf<X> = X extends { content: { "application/json": infer B } } ? B : never;
/** The success body of an operation; `undefined` for 204. */
type OkBody<O> = O extends { responses: infer R }
  ? { [S in keyof R]: S extends 200 | 201 | 202 ? JsonOf<R[S]> : S extends 204 ? undefined : never }[keyof R]
  : never;
/** The success body of a route, for fixtures declared apart from the route table: `ApiBody<"GET /catalog">`. */
export type ApiBody<R extends ApiRoute> = OkBody<OpOf<R>>;
type ReqBody<O> = O extends { requestBody?: infer B } ? JsonOf<NonNullable<B>> : never;

export interface ApiRequest<B = unknown> {
  method: string;
  /** Path below /api/v1 ("/upstreams/u1"), or the full path for other APIs ("/api/plugins/…"). */
  path: string;
  /** Values of `{name}` segments in the matched route. */
  params: Record<string, string>;
  query: URLSearchParams;
  body: B;
}

const REPLY = Symbol("reply");
/** A non-success answer: any status, any body. */
export interface Reply {
  [REPLY]: true;
  status: number;
  body: unknown;
}

type Answer<T> = T | Reply | Response;
type Handler<T, B> = Answer<T> | ((req: ApiRequest<B>) => Answer<T> | Promise<Answer<T>>);

/**
 * Answers by route. Admin API routes are keyed like the OpenAPI document ("PATCH /keys/{id}") and
 * typed by it, so a fixture that does not match the API fails the typecheck. Module APIs
 * outside /api/v1 are keyed by their full path ("GET /api/plugins/admin.signup/config"), untyped.
 */
export type ApiRoutes = { [R in ApiRoute]?: Handler<OkBody<OpOf<R>>, ReqBody<OpOf<R>>> } & {
  [R: `${Uppercase<Method>} /api/${string}`]: Handler<unknown, unknown>;
};

/** A non-success answer. For API errors prefer `apiError`. */
export function reply(status: number, body?: unknown): Reply {
  return { [REPLY]: true, status, body };
}

type ApiErrorFields = Pick<ApiErrorBody["error"], "params" | "fields">;

/**
 * The admin API's error body: a code the console words itself (use a real one from
 * internal/admin/errtext.go), its params and the English message.
 */
export function apiError(status: number, code: string, message: string, extra: ApiErrorFields = {}): Reply {
  const body: ApiErrorBody = { error: { code, message, ...extra } };
  return reply(status, body);
}

// ---------------------------------------------------------------- fetch

function matchRoute(pattern: string, path: string): Record<string, string> | null {
  const a = pattern.split("/");
  const b = path.split("/");
  if (a.length !== b.length) return null;
  const params: Record<string, string> = {};
  for (let i = 0; i < a.length; i++) {
    const seg = a[i]!;
    if (seg.startsWith("{") && seg.endsWith("}")) params[seg.slice(1, -1)] = decodeURIComponent(b[i]!);
    else if (seg !== b[i]) return null;
  }
  return params;
}

export interface FakeApi {
  fetch: typeof fetch;
  /** Every request that matched a route, in order. */
  calls: ApiRequest[];
  /** "METHOD /path" of every request no route answered (it got a 404). */
  unmatched: string[];
  /** The matched requests to one route, `called("PATCH /keys/{id}")`, or to one path, `called("PATCH", "/keys/k_1")`. */
  called(route: ApiRoute | `${Uppercase<Method>} /api/${string}`): ApiRequest[];
  called(method: Uppercase<Method>, path: string): ApiRequest[];
  /** The query of the last request to `path` (below /api/v1), as a plain object. */
  lastQuery(path: string): Record<string, string> | undefined;
}

/**
 * A fetch that answers from `routes`. Exact paths win over `{name}` patterns, so "/logs/histogram"
 * beats "/logs/{id}". Unlisted requests answer 404 and land in `unmatched`.
 */
function fakeApi(routes: ApiRoutes): FakeApi {
  const calls: (ApiRequest & { route: string })[] = [];
  const unmatched: string[] = [];
  const table = Object.entries(routes as Record<string, unknown>)
    .map(([key, answer]) => {
      const [method, pattern] = key.split(" ") as [string, string];
      return { key, method, pattern, answer };
    })
    .sort((x, y) => Number(x.pattern.includes("{")) - Number(y.pattern.includes("{")));
  const fetchImpl = async (input: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
    const req = input instanceof Request ? input : new Request(input, init);
    const url = new URL(req.url, location.origin);
    const path = url.pathname.startsWith("/api/v1/") ? url.pathname.slice("/api/v1".length) : url.pathname;
    const text = req.method === "GET" || req.method === "HEAD" ? "" : await req.text();
    let body: unknown;
    try {
      body = text ? JSON.parse(text) : undefined;
    } catch {
      body = text;
    }
    for (const row of table) {
      if (row.method !== req.method) continue;
      const params = matchRoute(row.pattern, path);
      if (!params) continue;
      const r = { route: row.key, method: req.method, path, params, query: url.searchParams, body };
      calls.push(r);
      const out = typeof row.answer === "function" ? await (row.answer as (q: ApiRequest) => unknown)(r) : row.answer;
      if (out instanceof Response) return out;
      if (out && typeof out === "object" && REPLY in out) {
        const rep = out as Reply;
        return rep.body === undefined ? new Response(null, { status: rep.status }) : Response.json(rep.body, { status: rep.status });
      }
      return out === undefined ? new Response(null, { status: 204 }) : Response.json(out);
    }
    unmatched.push(`${req.method} ${path}`);
    return Response.json({ error: { code: "not_found", message: `no fixture for ${req.method} ${path}` } }, { status: 404 });
  };
  return {
    fetch: fetchImpl as typeof fetch,
    calls,
    unmatched,
    called: (routeOrMethod: string, path?: string) =>
      calls.filter((c) => (path === undefined ? c.route === routeOrMethod : c.method === routeOrMethod && c.path === path)),
    lastQuery: (path) => {
      const hit = calls.findLast((c) => c.path === path);
      return hit && Object.fromEntries(hit.query);
    },
  };
}

// ---------------------------------------------------------------- callers

const ADMIN_PERMISSIONS: Me["permissions"] = [
  "upstreams.manage",
  "models.manage",
  "users.manage",
  "features.manage",
  "settings.manage",
  "audit.read",
  "keys.manage_all",
  "logs.read_all",
];

/** An operator with every permission; override fields per test. */
export function adminMe(over: Partial<Me> = {}): Me {
  return {
    user: { id: "u_admin", username: "admin@example.com", display_name: "Ada Admin", role: "admin" },
    auth: "session",
    permissions: ADMIN_PERMISSIONS,
    features: ["limits", "routing", "request_log", "metrics"],
    pages: [],
    site: { name: "Test relay", currency: "USD", show_upstream_names: true },
    ...over,
  };
}

/** A member without operator permissions. */
export function memberMe(over: Partial<Me> = {}): Me {
  return adminMe({
    user: { id: "u_alice", username: "alice@example.com", display_name: "Alice", role: "member" },
    permissions: [],
    ...over,
  });
}

// ---------------------------------------------------------------- render

export interface RenderPageOptions {
  /** API answers; `GET /me` defaults to `me`. */
  routes?: ApiRoutes;
  /** The memory router's starting URL. */
  path?: string;
  locale?: Locale;
  /** The signed-in caller, or null for a signed-out page (login, setup). */
  me?: Me | null;
}

/**
 * Render a page with the console's providers, a memory router at `path`, the caller `me` and the
 * API answered from `routes`. The page stays mounted across navigation, so a test can read the
 * URL it pushed (`router.state.location`).
 */
export function renderPage(ui: ReactElement, { routes = {}, path = "/", locale = "en", me = adminMe() }: RenderPageOptions = {}) {
  const api = fakeApi(me ? ({ "GET /me": me, ...routes } as ApiRoutes) : routes);
  setFetch(api.fetch);
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  if (me) queryClient.setQueryData(meQuery.queryKey, me);
  const rootRoute = createRootRoute({
    component: () => (
      <>
        {ui}
        <Outlet />
      </>
    ),
  });
  // Any path resolves, so the page can link and navigate anywhere.
  const routeTree = rootRoute.addChildren([
    createRoute({ getParentRoute: () => rootRoute, path: "/", component: () => null }),
    createRoute({ getParentRoute: () => rootRoute, path: "$", component: () => null }),
  ]);
  const router = createRouter({ routeTree, history: createMemoryHistory({ initialEntries: [path] }) });
  const user = userEvent.setup();
  const result = render(
    <I18nProvider locale={locale}>
      <QueryClientProvider client={queryClient}>
        <TooltipProvider>
          <ConfirmProvider>
            <Suspense fallback={null}>
              <RouterProvider router={router} />
            </Suspense>
          </ConfirmProvider>
        </TooltipProvider>
        <Toaster />
      </QueryClientProvider>
    </I18nProvider>,
  );
  return { ...result, user, router, queryClient, api };
}
