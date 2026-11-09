import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import { api, unwrap } from "@/lib/api/client";
import { errorMessage } from "@/lib/api/errors";
import type { TestResult } from "@/lib/api/types";
import { useI18n } from "@/i18n";

const key = (id: string) => ["upstream-test", id] as const;

/** The last test of an upstream in this session (list row and drawer show the same outcome). */
export function useLastTest(id: string): TestResult[] | undefined {
  return useQuery<TestResult[] | null>({ queryKey: key(id), queryFn: () => null, enabled: false, staleTime: Infinity }).data ?? undefined;
}

/** Tests upstreams in parallel (or one credential), remembers each outcome and says how it went. */
export function useTestUpstreams() {
  const { t } = useI18n();
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async ({ ids, credentialId }: { ids: string[]; credentialId?: string }) => {
      const outcomes = await Promise.all(
        ids.map(async (id) => {
          const r = await unwrap(api.POST("/upstreams/{id}/test", { params: { path: { id } }, body: credentialId ? { credential_id: credentialId } : {} }));
          return [id, r.results] as const;
        }),
      );
      return outcomes;
    },
    onSuccess: (outcomes, { credentialId }) => {
      for (const [id, results] of outcomes) {
        if (credentialId) {
          const prev = qc.getQueryData<TestResult[] | null>(key(id)) ?? [];
          qc.setQueryData(key(id), [...prev.filter((r) => r.credential_id !== credentialId), ...results]);
        } else qc.setQueryData(key(id), results);
      }
      const all = outcomes.flatMap(([, r]) => r);
      const failed = all.filter((r) => !r.ok);
      if (failed.length) toast.error(t("upstreams.testFailed", { n: failed.length }), { description: failed[0]?.error?.slice(0, 200) });
      else toast.success(t("upstreams.testOk", { n: all.length }));
      void qc.invalidateQueries({ queryKey: ["upstreams"] });
    },
    onError: (e) => toast.error(errorMessage(e)),
  });
}
