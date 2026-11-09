import { useQuery } from "@tanstack/react-query";
import { Link, useNavigate } from "@tanstack/react-router";
import { Laptop } from "lucide-react";
import { BarList } from "@/components/bar-list";
import { ClientName, clientColor } from "@/components/client-name";
import { useMoney } from "@/components/money";
import { EmptyState, ErrorState, Panel } from "@/components/page";
import { Skeleton } from "@/components/ui/skeleton";
import { isOperator } from "@/app/nav";
import { useLogsPath, useMe } from "@/app/session";
import { useTimeRange } from "@/app/time-range";
import { api, unwrap } from "@/lib/api/client";
import { rangeStart } from "@/lib/time-range";
import { useI18n } from "@/i18n";

const SHOWN = 4;

/** Spend per client program over the page's time range: the top few, the rest as "other". */
export function ClientSpendPanel({ linkToAnalytics = true }: { linkToAnalytics?: boolean }) {
  const { t } = useI18n();
  const money = useMoney();
  const { range } = useTimeRange();
  const navigate = useNavigate();
  const admin = isOperator(useMe());
  const logs = useLogsPath();
  const q = useQuery({
    queryKey: ["timeseries", range, "spend", "client"],
    queryFn: () => unwrap(api.GET("/usage/timeseries", { params: { query: { range, metric: "spend", group_by: "client" } } })),
    placeholderData: (p) => p,
  });
  const currency = q.data?.currency ?? "USD";
  const totals = (q.data?.series ?? [])
    .map((s) => ({ id: s.key, name: s.label, total: s.points.reduce((a, p) => a + p.v, 0) }))
    .filter((r) => r.total > 0)
    .sort((a, b) => b.total - a.total);
  // Unrecognised clients join the long tail in one "other" row at the end.
  const top = totals.filter((r) => r.id !== "other").slice(0, SHOWN);
  const rest = totals.filter((r) => !top.includes(r)).reduce((a, r) => a + r.total, 0);
  const open = (client?: string) => void navigate({ to: logs, search: { from: rangeStart(range), ...(client ? { client } : {}) } });
  const items = [
    ...top.map((r) => ({
      key: r.id,
      label: <ClientName client={{ id: r.id, name: r.name }} />,
      value: r.total,
      display: money({ micro: r.total, currency }),
      color: clientColor(r.id),
      onClick: () => open(r.id),
    })),
    ...(rest > 0
      ? [{ key: "__rest", label: <ClientName client={{ id: "other" }} />, value: rest, display: money({ micro: rest, currency }), color: "var(--chart-4)" }]
      : []),
  ];
  return (
    <Panel
      title={t("overview.byClient")}
      aside={
        admin && linkToAnalytics ? (
          <Link to="/analytics" className="text-accent hover:underline">
            {t("overview.toAnalytics")} →
          </Link>
        ) : undefined
      }
    >
      {q.isError ? (
        <ErrorState error={q.error} onRetry={() => void q.refetch()} />
      ) : !q.data ? (
        <div className="grid gap-3">
          {Array.from({ length: 4 }, (_, i) => (
            <Skeleton key={i} className="h-4" />
          ))}
        </div>
      ) : items.length === 0 ? (
        <EmptyState compact icon={Laptop} title={t("overview.noClientSpend")} hint={t("overview.byClientHint")} />
      ) : (
        <>
          <BarList items={items} />
          <p className="mt-3 text-xs text-ink-3">{t("overview.byClientHint")}</p>
        </>
      )}
    </Panel>
  );
}
