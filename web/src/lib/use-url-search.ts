import { useNavigate, useSearch } from "@tanstack/react-router";
import { useCallback } from "react";
import { cleanSearch, type SearchValue } from "./url-filters";

type Patch = Record<string, SearchValue>;

/**
 * The current route's search params and a setter that merges a patch into them. Empty values
 * (undefined, "", false) are dropped, so a cleared filter disappears from the URL.
 * The route's `validateSearch` decides which params survive.
 */
export function useUrlSearch(): [Record<string, unknown>, (patch: Patch, opts?: { replace?: boolean }) => void] {
  const search = useSearch({ strict: false }) as Record<string, unknown>;
  const navigate = useNavigate();
  const set = useCallback(
    (patch: Patch, opts?: { replace?: boolean }) =>
      void navigate({
        to: ".",
        search: ((prev: Record<string, unknown>) => cleanSearch({ ...prev, ...patch })) as never,
        replace: opts?.replace,
      }),
    [navigate],
  );
  return [search, set];
}

/**
 * A drawer whose open item lives in the URL as `?id=…`, so a link opens the same drawer and Back
 * closes it. Every page uses `id`; a page has one drawer open at a time.
 */
export function useDrawerParam() {
  const [search, set] = useUrlSearch();
  const raw = search.id;
  const id = typeof raw === "string" && raw !== "" ? raw : undefined;
  const open = useCallback((next: string) => set({ id: next }, { replace: id !== undefined }), [set, id]);
  const close = useCallback(() => set({ id: undefined }), [set]);
  return { id, open, close, onOpenChange: (o: boolean) => !o && close() };
}
