import { useQuery } from "@tanstack/react-query";
import { api, unwrap } from "./client";

/**
 * `GET /upstreams`, shared by every page that lists upstreams. Pages showing live state pass a
 * refetch interval; `enabled: false` skips it (members may not list upstreams).
 */
export function useUpstreams(options: { refetchInterval?: number; enabled?: boolean } = {}) {
  return useQuery({ queryKey: ["upstreams"], queryFn: () => unwrap(api.GET("/upstreams")), ...options });
}

/** `GET /manifests`, the upstream types. They only change with a new binary or an import. */
export function useManifests(options: { enabled?: boolean } = {}) {
  return useQuery({ queryKey: ["manifests"], queryFn: () => unwrap(api.GET("/manifests")), staleTime: Infinity, ...options });
}
