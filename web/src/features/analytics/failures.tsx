import { useQuery } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { CircleCheck } from "lucide-react";
import { lazy, Suspense } from "react";
import { EmptyState, ErrorState, Panel } from "@/components/page";
import { Skeleton } from "@/components/ui/skeleton";
import { useLogsPath } from "@/app/session";
import { useTimeRange } from "@/app/time-range";
import { api, unwrap } from "@/lib/api/client";
import type { Timeseries } from "@/lib/api/types";
import { rangeStart } from "@/lib/time-range";
import { useI18n } from "@/i18n";

const UsageChart = lazy(() => import("../overview/usage-chart"));

/** Each cause keeps its color across ranges. */
const CAUSE_COLORS: Record<string, string> = {
  rate_limit: "var(--warn)",
  transient: "var(--bad)",
  quota: "var(--amber)",
  not_sent: "var(--chart-4)",
  request: "var(--info)",
  banned: "var(--violet)",
  auth_expired: "var(--chart-5)",
  content: "var(--chart-2)",
  unknown: "var(--ink-3)",
};

/** Failed requests over time, stacked by why they failed. */
export function FailuresPanel() {
  const { t, locale } = useI18n();
  const { range } = useTimeRange();
  const logs = useLogsPath();
  const q = useQuery({
    queryKey: ["usage-failures", range],
    queryFn: () => unwrap(api.GET("/usage/failures", { params: { query: { range } } })),
    placeholderData: (p) => p,
  });
  const total = (q.data?.series ?? []).reduce((a, s) => a + s.total, 0);
  const chart: Timeseries | undefined = q.data && {
    metric: "failures",
    bucket_seconds: q.data.bucket_seconds,
    series: q.data.series.map((s) => ({ key: s.cause, label: t(`cause.${s.cause}`), points: s.points })),
  };
  return (
    <Panel
      title={t("analytics.failures")}
      aside={
        total > 0 ? (
          <Link to={logs} search={{ from: rangeStart(range), status: "error" }} className="text-accent hover:underline">
            {t("analytics.failuresTotal", { n: total })}
          </Link>
        ) : undefined
      }
    >
      {q.isError ? (
        <ErrorState error={q.error} onRetry={() => void q.refetch()} />
      ) : !chart ? (
        <Skeleton className="h-[220px]" />
      ) : total === 0 ? (
        <EmptyState compact icon={CircleCheck} title={t("analytics.noFailures")} hint={t("analytics.noFailuresHint")} />
      ) : (
        <Suspense fallback={<Skeleton className="h-[220px]" />}>
          <UsageChart data={chart} locale={locale} otherLabel={t("common.other")} height={190} colors={CAUSE_COLORS} />
        </Suspense>
      )}
    </Panel>
  );
}
