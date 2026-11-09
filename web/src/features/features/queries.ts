import { useQueryClient } from "@tanstack/react-query";
import { api, unwrap } from "@/lib/api/client";

export const featuresQuery = { queryKey: ["features"], queryFn: () => unwrap(api.GET("/features")) };
export const extensionsQuery = { queryKey: ["extensions"], queryFn: () => unwrap(api.GET("/extensions")) };

/** After a change, the lists and the menu (`/me` pages) follow. */
export function useFeaturesRefresh() {
  const qc = useQueryClient();
  return () => {
    void qc.invalidateQueries({ queryKey: featuresQuery.queryKey });
    void qc.invalidateQueries({ queryKey: extensionsQuery.queryKey });
    void qc.invalidateQueries({ queryKey: ["me"] });
  };
}
