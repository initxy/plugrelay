import { queryOptions, useSuspenseQuery } from "@tanstack/react-query";
import { api, unwrap } from "@/lib/api/client";
import { isOperator } from "./nav";

export const setupStatusQuery = queryOptions({
  queryKey: ["setup-status"],
  queryFn: () => unwrap(api.GET("/setup/status")),
  staleTime: Infinity,
});

export const meQuery = queryOptions({
  queryKey: ["me"],
  queryFn: () => unwrap(api.GET("/me")),
  staleTime: 60_000,
  retry: false,
});

/** The caller; only used under the authenticated layout, which has loaded it. */
export function useMe() {
  return useSuspenseQuery(meQuery).data;
}

/** Where the caller's requests are listed: the request log for operators, the usage page for members. */
export function useLogsPath(): "/logs" | "/usage" {
  return isOperator(useMe()) ? "/logs" : "/usage";
}

/** Whether /me lists the feature page for the caller. */
export function usePageOn(id: string): boolean {
  return useMe().pages.includes(id);
}

/** Whether the feature is on. */
export function useFeatureOn(id: string): boolean {
  return useMe().features.includes(id);
}
