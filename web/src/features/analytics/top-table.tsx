import { useQuery } from "@tanstack/react-query";
import { useNavigate } from "@tanstack/react-router";
import type { ColumnDef } from "@tanstack/react-table";
import { useMemo, useState } from "react";
import { ClientName } from "@/components/client-name";
import { DataTable } from "@/components/data-table";
import { useMoney } from "@/components/money";
import { ErrorState, Panel } from "@/components/page";
import { Segmented } from "@/components/ui/segmented";
import { isOperator } from "@/app/nav";
import { useLogsPath, useMe } from "@/app/session";
import { useTimeRange } from "@/app/time-range";
import { api, unwrap } from "@/lib/api/client";
import type { UsageTop } from "@/lib/api/types";
import { formatCompact, formatDuration, formatNumber, formatPercent } from "@/lib/format";
import { rangeStart } from "@/lib/time-range";
import { useI18n } from "@/i18n";

type By = UsageTop["by"];
type Row = UsageTop["items"][number];

/** The log filter that lists one row's requests. */
const FILTER: Record<By, string> = { model: "model", key: "key_id", user: "user_id", client: "client" };

function useColumns(by: By, share: (r: Row) => number): ColumnDef<Row>[] {
  const { t, locale } = useI18n();
  const money = useMoney();
  return useMemo(
    () => [
      {
        id: "name",
        header: t("analytics.col.name"),
        enableHiding: false,
        meta: { className: "min-w-[180px]" },
        cell: ({ row }) => {
          const r = row.original;
          return (
            <span className="grid gap-1">
              {by === "client" ? (
                <ClientName client={{ id: r.key, name: r.label }} />
              ) : (
                <span className="truncate" data-raw={by === "model" || undefined}>
                  {r.label}
                </span>
              )}
              <span className="h-1 overflow-hidden rounded-full bg-surface-2">
                <span className="block h-full rounded-full bg-accent/70" style={{ width: `${Math.max(1, share(r) * 100)}%` }} />
              </span>
            </span>
          );
        },
      },
      {
        id: "requests",
        header: t("analytics.col.requests"),
        meta: { numeric: true, label: t("analytics.col.requests") },
        cell: ({ row }) => formatNumber(row.original.requests, locale),
      },
      {
        id: "failed",
        header: t("analytics.col.failed"),
        meta: { numeric: true, label: t("analytics.col.failed"), className: "max-md:hidden" },
        cell: ({ row }) => {
          const r = row.original;
          const rate = r.requests > 0 ? r.errors / r.requests : 0;
          return <span className={rate >= 0.05 ? "text-bad" : "text-ink-2"}>{formatPercent(rate, locale, 1)}</span>;
        },
      },
      {
        id: "tokens",
        header: t("analytics.col.tokens"),
        meta: { numeric: true, label: t("analytics.col.tokens"), className: "max-lg:hidden" },
        cell: ({ row }) => <span className="text-ink-2">{formatCompact(row.original.tokens, locale)}</span>,
      },
      {
        id: "cache",
        header: t("analytics.col.cache"),
        meta: { numeric: true, label: t("analytics.col.cache"), className: "max-lg:hidden" },
        cell: ({ row }) => {
          const r = row.original.cache_hit_rate;
          return r !== null ? <span className="text-ink-2">{formatPercent(r, locale, 0)}</span> : <span className="text-ink-3">—</span>;
        },
      },
      {
        id: "ttfb",
        header: () => <span title={t("analytics.col.ttfbHint")}>{t("analytics.col.ttfb")}</span>,
        meta: { numeric: true, label: t("analytics.col.ttfb"), className: "whitespace-nowrap max-md:hidden" },
        cell: ({ row }) => {
          const v = row.original.ttfb_p95_ms;
          return v !== null ? <span className="text-ink-2">{formatDuration(v)}</span> : <span className="text-ink-3">—</span>;
        },
      },
      {
        id: "spend",
        header: t("analytics.col.spend"),
        enableHiding: false,
        meta: { numeric: true },
        cell: ({ row }) => money(row.original.spend),
      },
    ],
    [t, locale, money, by, share],
  );
}

/** Models, keys, users or clients ranked by spend, with tokens, cache hits and first-token time. */
export function TopPanel() {
  const { t } = useI18n();
  const { range } = useTimeRange();
  const navigate = useNavigate();
  const admin = isOperator(useMe());
  const logs = useLogsPath();
  const options = (admin ? (["model", "key", "user", "client"] as const) : (["model", "key", "client"] as const)).map((v) => ({
    value: v,
    label: t(`analytics.by${v.charAt(0).toUpperCase()}${v.slice(1)}` as "analytics.byModel"),
  }));
  const [by, setBy] = useState<By>("model");
  const q = useQuery({
    queryKey: ["usage-top", range, by],
    queryFn: () => unwrap(api.GET("/usage/top", { params: { query: { range, by, limit: 12 } } })),
    placeholderData: (p) => (p?.by === by ? p : undefined),
  });
  const top = q.data?.items[0]?.spend.micro ?? 0;
  const share = useMemo(() => (r: Row) => (top > 0 ? r.spend.micro / top : 0), [top]);
  const columns = useColumns(by, share);
  return (
    <Panel
      title={t("analytics.top")}
      bodyClassName="p-0"
      aside={<Segmented size="sm" aria-label={t("usage.groupBy")} value={by} onChange={setBy} options={options} />}
    >
      {q.isError ? (
        <div className="p-4">
          <ErrorState error={q.error} onRetry={() => void q.refetch()} />
        </div>
      ) : (
        <DataTable
          className="rounded-none border-0"
          columns={columns}
          data={q.data?.items}
          getRowId={(r) => r.key}
          loadingRows={6}
          maxHeight="none"
          onRowClick={(r) =>
            void navigate({ to: logs, search: { from: rangeStart(range), [FILTER[by]]: r.key } })
          }
        />
      )}
    </Panel>
  );
}
