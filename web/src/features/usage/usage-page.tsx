import { useQuery } from "@tanstack/react-query";
import { BarChart3 } from "lucide-react";
import { lazy, Suspense, useState } from "react";
import { useClientLabel } from "@/components/client-name";
import { useMoney } from "@/components/money";
import { EmptyState, ErrorState, PageHeader, Panel } from "@/components/page";
import { Segmented } from "@/components/ui/segmented";
import { Skeleton } from "@/components/ui/skeleton";
import { isOperator } from "@/app/nav";
import { useMe } from "@/app/session";
import { TimeRangePicker, useTimeRange } from "@/app/time-range";
import { api, unwrap } from "@/lib/api/client";
import { formatCompact } from "@/lib/format";
import { useI18n } from "@/i18n";
import { HeatmapPanel } from "../analytics/heatmap";
import { LogActions, RequestLog } from "../logs/request-log";
import { QuotaWindows } from "../overview/quota-windows";

const UsageChart = lazy(() => import("../overview/usage-chart"));
type Metric = "spend" | "requests" | "tokens";
type GroupBy = "model" | "key" | "client";

function UsagePanel() {
  const { t, locale } = useI18n();
  const money = useMoney();
  const { range } = useTimeRange();
  const [metric, setMetric] = useState<Metric>("spend");
  const [groupBy, setGroupBy] = useState<GroupBy>("model");
  const clientLabel = useClientLabel();
  const q = useQuery({
    queryKey: ["timeseries", range, metric, groupBy],
    queryFn: () => unwrap(api.GET("/usage/timeseries", { params: { query: { range, metric, group_by: groupBy } } })),
    placeholderData: (p) => p,
  });
  // Client series carry the relay's English name; show the console's.
  const data =
    q.data && groupBy === "client"
      ? { ...q.data, series: q.data.series.map((s) => ({ ...s, label: clientLabel({ id: s.key, name: s.label }) })) }
      : q.data;
  const totals = (data?.series ?? [])
    .map((s) => ({ key: s.key, label: s.label, total: s.points.reduce((a, p) => a + p.v, 0) }))
    .sort((a, b) => b.total - a.total);
  const sum = totals.reduce((a, r) => a + r.total, 0);
  const fmt = (v: number) => (q.data?.currency ? money({ micro: v, currency: q.data.currency }) : formatCompact(v, locale));
  const empty = q.data && sum === 0;
  return (
    <Panel
      title={t("usage.chart")}
      aside={
        <div className="flex flex-wrap justify-end gap-2">
          <Segmented
            size="sm"
            aria-label={t("usage.metric")}
            value={metric}
            onChange={setMetric}
            options={[
              { value: "spend", label: t("usage.metricSpend") },
              { value: "requests", label: t("usage.metricRequests") },
              { value: "tokens", label: t("usage.metricTokens") },
            ]}
          />
          <Segmented
            size="sm"
            aria-label={t("usage.groupBy")}
            value={groupBy}
            onChange={setGroupBy}
            options={[
              { value: "model", label: t("usage.byModel") },
              { value: "key", label: t("usage.byKey") },
              { value: "client", label: t("usage.byClient") },
            ]}
          />
        </div>
      }
    >
      {q.isError ? (
        <ErrorState error={q.error} onRetry={() => void q.refetch()} />
      ) : !q.data ? (
        <Skeleton className="h-[240px]" />
      ) : empty ? (
        <EmptyState icon={BarChart3} title={t("overview.noUsage")} hint={t("overview.noUsageHint")} />
      ) : (
        <div className="grid gap-5 lg:grid-cols-[minmax(0,1fr)_260px]">
          <Suspense fallback={<Skeleton className="h-[240px]" />}>
            <UsageChart data={data ?? q.data} locale={locale} otherLabel={t("common.other")} height={210} />
          </Suspense>
          <div>
            <h3 className="mb-2 text-xs font-medium text-ink-3">{t("usage.totals")}</h3>
            <ul className="grid gap-2">
              {totals.slice(0, 8).map((r) => (
                <li key={r.key} className="text-sm">
                  <div className="flex items-baseline justify-between gap-2">
                    <span className="truncate">{r.label}</span>
                    <span className="tabular-nums">{fmt(r.total)}</span>
                  </div>
                  <div className="mt-1 h-1.5 overflow-hidden rounded-full bg-surface-2">
                    <div className="h-full rounded-full bg-accent/70" style={{ width: `${sum ? (r.total / sum) * 100 : 0}%` }} />
                  </div>
                </li>
              ))}
            </ul>
          </div>
        </div>
      )}
    </Panel>
  );
}

/** Usage and logs: what the caller used over time, their quota windows, and their own requests. */
export function UsagePage() {
  const { t } = useI18n();
  const admin = isOperator(useMe());
  return (
    <>
      <PageHeader title={t("nav.usage")} description={t("usage.description")} actions={<TimeRangePicker />} />
      <div className="grid gap-4">
        <UsagePanel />
        <HeatmapPanel />
        <QuotaWindows />
        <section className="grid gap-3" aria-labelledby="usage-requests">
          <div className="flex flex-wrap items-center justify-between gap-2">
            <h2 id="usage-requests" className="text-base font-semibold">
              {t("usage.requests")}
            </h2>
            <LogActions />
          </div>
          <RequestLog admin={admin} tableHeight="560px" />
        </section>
      </div>
    </>
  );
}
