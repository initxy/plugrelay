import { useQuery } from "@tanstack/react-query";
import { Link, useNavigate } from "@tanstack/react-router";
import { BarChart3, ScrollText } from "lucide-react";
import { lazy, Suspense } from "react";
import { useMoney } from "@/components/money";
import { EmptyState, ErrorState, PageHeader, Panel } from "@/components/page";
import { deltaOf, StatTile } from "@/components/stat-tile";
import { Skeleton } from "@/components/ui/skeleton";
import { isOperator } from "@/app/nav";
import { useLogsPath, useMe } from "@/app/session";
import { TimeRangePicker, useTimeRange } from "@/app/time-range";
import { api, unwrap } from "@/lib/api/client";
import type { TimeRange } from "@/lib/api/types";
import { formatDuration, formatNumber, formatPercent } from "@/lib/format";
import { rangeStart } from "@/lib/time-range";
import { useI18n } from "@/i18n";
import { HandlingPill, ModelName, RequestTime, RetryPills } from "../logs/request-pills";
import { ClientSpendPanel } from "../analytics/client-spend";
import { QuotaWindows } from "./quota-windows";
import { NeedsAttention, UpstreamHealth } from "./upstream-state";

const UsageChart = lazy(() => import("./usage-chart"));

const SPARK_POINTS = 14;

/** At most SPARK_POINTS points, neighbours summed (or averaged for rates), so the line shows the trend, not the time of day. */
function downsample(values: number[], average: boolean): number[] {
  const step = Math.ceil(values.length / SPARK_POINTS);
  if (step <= 1) return values;
  const out: number[] = [];
  for (let i = 0; i < values.length; i += step) {
    const chunk = values.slice(i, i + step);
    const total = chunk.reduce((a, v) => a + v, 0);
    out.push(average ? total / chunk.length : total);
  }
  return out;
}

function useSpark(range: TimeRange, metric: "spend" | "requests" | "cache_hit_rate") {
  return useQuery({
    queryKey: ["timeseries", range, metric, "none"],
    queryFn: () => unwrap(api.GET("/usage/timeseries", { params: { query: { range, metric, group_by: "none" } } })),
    placeholderData: (p) => p,
    select: (d) => downsample(d.series[0]?.points.map((p) => p.v) ?? [], metric === "cache_hit_rate"),
  });
}

function Tiles() {
  const { t, locale } = useI18n();
  const money = useMoney();
  const { range } = useTimeRange();
  const navigate = useNavigate();
  const logs = useLogsPath();
  const q = useQuery({
    queryKey: ["overview", range],
    queryFn: () => unwrap(api.GET("/overview", { params: { query: { range } } })),
    placeholderData: (p) => p,
  });
  const spend = useSpark(range, "spend");
  const requests = useSpark(range, "requests");
  const cache = useSpark(range, "cache_hit_rate");
  if (q.isError) return <ErrorState error={q.error} onRetry={() => void q.refetch()} />;
  const o = q.data;
  const loading = !o;
  const from = rangeStart(range);
  const vs = t(`overview.vs.${range}`);
  const open = (extra?: Record<string, string>) => void navigate({ to: logs, search: { from, ...extra } });
  const balance = o?.balance;
  return (
    <div className={balance ? "grid grid-cols-2 gap-3 lg:grid-cols-5" : "grid grid-cols-2 gap-3 lg:grid-cols-4"}>
      {balance && (
        <StatTile
          className="col-span-2 lg:col-span-1"
          label={t("overview.balance")}
          value={money(balance.amount)}
          note={
            balance.days_left !== undefined ? (
              <span className={balance.days_left < 3 ? "text-warn" : undefined}>{t("overview.daysLeft", { n: Math.floor(balance.days_left) })}</span>
            ) : undefined
          }
        />
      )}
      <StatTile
        label={t("overview.spend")}
        value={o ? money(o.spend) : ""}
        loading={loading}
        delta={o ? withDelta(deltaOf(o.spend.micro, o.spend_prev.micro), "up", vs) : undefined}
        note={o && o.spend_prev.micro === 0 ? t("overview.noPrev") : undefined}
        spark={spend.data}
        onClick={() => open()}
      />
      <StatTile
        label={t("overview.requests")}
        value={o ? formatNumber(o.requests, locale) : ""}
        loading={loading}
        delta={o ? withDelta(deltaOf(o.requests, o.requests_prev), "up") : undefined}
        note={
          o && (
            <span className={o.success_rate < 0.95 ? "text-bad" : undefined}>
              {t("overview.successRate", { pct: formatPercent(o.success_rate, locale) })}
            </span>
          )
        }
        spark={requests.data}
        onClick={() => open(o && o.success_rate < 1 ? { status: "error" } : undefined)}
      />
      <StatTile
        label={t("overview.cacheHit")}
        value={o ? formatPercent(o.cache_hit_rate, locale, 0) : ""}
        loading={loading}
        note={o && o.cache_saved.micro > 0 ? t("overview.saved", { amount: money(o.cache_saved) }) : undefined}
        delta={o ? withDelta(deltaOf(o.cache_hit_rate, o.cache_hit_rate_prev), "up") : undefined}
        spark={cache.data}
        sparkTone="ok"
        onClick={() => open()}
      />
      <StatTile
        label={t("overview.ttfb")}
        value={o ? (o.ttfb_p95_ms !== undefined ? formatDuration(o.ttfb_p95_ms) : "—") : ""}
        loading={loading}
        delta={o && o.ttfb_p95_ms !== undefined ? withDelta(deltaOf(o.ttfb_p95_ms, o.ttfb_p95_prev_ms), "down", vs) : undefined}
        note={o && o.ttfb_p95_ms === undefined ? t("overview.ttfbNone") : undefined}
        onClick={() => open({ status: "ok" })}
      />
    </div>
  );
}

function withDelta(ratio: number | undefined, goodWhen: "up" | "down", label?: string) {
  return ratio === undefined ? undefined : { ratio, goodWhen, label };
}

function SpendPanel() {
  const { t, locale } = useI18n();
  const money = useMoney();
  const { range } = useTimeRange();
  const logs = useLogsPath();
  const q = useQuery({
    queryKey: ["timeseries", range, "spend", "model"],
    queryFn: () => unwrap(api.GET("/usage/timeseries", { params: { query: { range, metric: "spend", group_by: "model" } } })),
    placeholderData: (p) => p,
  });
  const o = useQuery({
    queryKey: ["overview", range],
    queryFn: () => unwrap(api.GET("/overview", { params: { query: { range } } })),
    placeholderData: (p) => p,
  });
  const empty = q.data && q.data.series.every((s) => s.points.every((p) => p.v === 0));
  return (
    <Panel
      title={t("overview.spendTrend")}
      aside={
        <>
          {o.data && o.data.avg_price_per_mtok.micro > 0 && (
            <span className="max-sm:hidden">{t("overview.avgPrice", { amount: money(o.data.avg_price_per_mtok) })}</span>
          )}
          <Link to={logs} search={{ from: rangeStart(range) }} className="text-accent hover:underline">
            {t("overview.viewRequests")}
          </Link>
        </>
      }
    >
      {q.isError ? (
        <ErrorState error={q.error} onRetry={() => void q.refetch()} />
      ) : !q.data ? (
        <Skeleton className="h-[262px]" />
      ) : empty ? (
        <EmptyState icon={BarChart3} title={t("overview.noUsage")} hint={t("overview.noUsageHint")} className="h-[262px] justify-center py-0" />
      ) : (
        <Suspense fallback={<Skeleton className="h-[262px]" />}>
          <UsageChart data={q.data} locale={locale} otherLabel={t("common.other")} height={228} />
        </Suspense>
      )}
    </Panel>
  );
}

function RecentRequests() {
  const { t } = useI18n();
  const money = useMoney();
  const logs = useLogsPath();
  const q = useQuery({
    queryKey: ["logs-recent"],
    queryFn: () => unwrap(api.GET("/logs", { params: { query: { limit: 8 } } })),
    refetchInterval: 30_000,
  });
  return (
    <Panel
      title={t("overview.recent")}
      aside={
        <Link to={logs} className="text-accent hover:underline">
          {t("overview.allRequests")}
        </Link>
      }
      bodyClassName="p-0"
    >
      {q.isError ? (
        <div className="p-4">
          <ErrorState error={q.error} onRetry={() => void q.refetch()} />
        </div>
      ) : !q.data ? (
        <div className="grid gap-2 p-4">
          {Array.from({ length: 6 }, (_, i) => (
            <Skeleton key={i} className="h-6" />
          ))}
        </div>
      ) : q.data.items.length === 0 ? (
        <EmptyState compact icon={ScrollText} title={t("logs.empty")} hint={t("overview.noRequestsHint")} />
      ) : (
        <ul>
          {q.data.items.map((l) => (
            <li key={l.id} className="border-b border-line-soft last:border-0">
              <Link
                to={logs}
                search={{ id: l.id }}
                className="grid grid-cols-[minmax(64px,max-content)_minmax(0,1fr)_auto_64px] items-center gap-3 px-4 py-2 text-sm outline-none hover:bg-surface-2/70 focus-visible:bg-accent-soft/60"
              >
                <RequestTime iso={l.time} />
                <ModelName log={l} className="truncate" />
                <span className="flex items-center gap-1 max-sm:hidden">
                  <HandlingPill log={l} />
                  <RetryPills log={l} />
                </span>
                <span className="text-right tabular-nums">{money(l.charge)}</span>
              </Link>
            </li>
          ))}
        </ul>
      )}
    </Panel>
  );
}

export function OverviewPage() {
  const { t } = useI18n();
  const admin = isOperator(useMe());
  return (
    <>
      <PageHeader title={t("nav.overview")} description={admin ? t("overview.adminHint") : t("overview.memberHint")} actions={<TimeRangePicker />} />
      <div className="grid gap-4">
        <Tiles />
        {admin ? (
          <>
            <div className="grid gap-4 xl:grid-cols-[minmax(0,1.6fr)_minmax(0,1fr)]">
              <SpendPanel />
              <ClientSpendPanel />
            </div>
            <div className="grid gap-4 xl:grid-cols-2">
              <NeedsAttention />
              <UpstreamHealth />
            </div>
            <RecentRequests />
            <QuotaWindows />
          </>
        ) : (
          <>
            <div className="grid gap-4 xl:grid-cols-[minmax(0,1.6fr)_minmax(0,1fr)]">
              <SpendPanel />
              <ClientSpendPanel />
            </div>
            <QuotaWindows />
            <RecentRequests />
          </>
        )}
      </div>
    </>
  );
}
