import createClient, { type Middleware } from "openapi-fetch";
import { ApiError } from "./errors";
import type { paths } from "./schema.gen";

const API_BASE = "/api/v1";

function readCookie(name: string): string | undefined {
  const hit = document.cookie.split("; ").find((c) => c.startsWith(name + "="));
  return hit ? decodeURIComponent(hit.slice(name.length + 1)) : undefined;
}

const UNSAFE = new Set(["POST", "PUT", "PATCH", "DELETE"]);

/** Double-submit CSRF: echo the readable `pr_csrf` cookie on unsafe methods. */
const csrfMiddleware: Middleware = {
  onRequest({ request }) {
    if (UNSAFE.has(request.method)) {
      const token = readCookie("pr_csrf");
      if (token) request.headers.set("X-CSRF-Token", token);
    }
    return request;
  },
};

let fetchImpl: typeof fetch = (input, init) => globalThis.fetch(input, init);

/** Swap the transport; tests route requests to their fixtures this way. */
export function setFetch(f: typeof fetch): void {
  fetchImpl = f;
}

/** fetch with the same transport and CSRF handling, for the module APIs under /api/plugins/. */
export async function rawFetch(path: string, init: RequestInit = {}): Promise<Response> {
  const req = new Request(new URL(path, location.origin), { credentials: "same-origin", ...init });
  if (UNSAFE.has(req.method)) {
    const token = readCookie("pr_csrf");
    if (token) req.headers.set("X-CSRF-Token", token);
  }
  return fetchImpl(req);
}

export const api = createClient<paths>({
  baseUrl: API_BASE,
  credentials: "same-origin",
  fetch: (req) => fetchImpl(req),
});
api.use(csrfMiddleware);

interface FetchResult<T> {
  data?: T;
  error?: unknown;
  response: Response;
}

/** Turn an openapi-fetch result into data or a thrown ApiError. */
export async function unwrap<T>(p: Promise<FetchResult<T>>): Promise<T> {
  const { data, error, response } = await p;
  if (!response.ok) {
    throw new ApiError(
      response.status,
      error as ConstructorParameters<typeof ApiError>[1],
      `${response.status} ${response.statusText || "request failed"}`,
    );
  }
  return data as T;
}
