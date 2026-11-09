import { useInfiniteQuery } from "@tanstack/react-query";
import type { ColumnDef } from "@tanstack/react-table";
import { Download, ListX, ScrollText } from "lucide-react";
import { useMemo, type ReactNode } from "react";
import { ClientName } from "@/components/client-name";
import { DataTable } from "@/components/data-table";
import { useMoney } from "@/components/money";
import { EmptyState, ErrorState } from "@/components/page";
import { Pill } from "@/components/status";
import { Button } from "@/components/ui/button";
import { Segmented } from "@/components/ui/segmented";
import { Tooltip } from "@/components/ui/tooltip";
import { api, unwrap } from "@/lib/api/client";
import type { LogSummary, Session } from "@/lib/api/types";
import { formatDuration, formatPercent } from "@/lib/format";
import { parseLogFilters, serializeLogFilters, type LogFilters, type LogView } from "@/lib/url-filters";
import { useDrawerParam, useUrlSearch } from "@/lib/use-url-search";
import { useI18n } from "@/i18n";
import { RequestHistogram } from "./histogram";
import { LogDrawer } from "./log-drawer";
import { LogFilterBar } from "./log-filters";
import { HandlingPill, ModelName, RequestTime, RetryPills } from "./request-pills";

const PAGE = 100;
const SESSION_PAGE = 50;
/** What `GET /logs/sessions` covers when no time filter is sent. */
const SESSION_SPAN = "30d";

function useColumns(admin: boolean): ColumnDef<LogSummary>[] {
  const { t, locale } = useI18n();
  const money = useMoney();
  return useMemo(
    () => [
      { id: "time", header: t("logs.time"), enableHiding: false, meta: { className: "w-[1%]" }, cell: ({ row }) => <RequestTime iso={row.original.time} /> },
      {
        id: "client",
        header: t("logs.client"),
        meta: { label: t("logs.client"), className: "w-[1%] max-w-[150px] whitespace-nowrap max-lg:hidden" },
        cell: ({ row }) => <ClientName client={row.original.client} className="max-w-[150px] text-ink-2" />,
      },
      {
        id: "model",
        header: t("logs.model"),
        enableHiding: false,
        meta: { className: "max-w-[280px]" },
        cell: ({ row }) => <ModelName log={row.original} className="block truncate" />,
      },
      {
        id: "handling",
        header: t("logs.handling"),
        meta: { label: t("logs.handling"), className: "w-[1%]" },
        cell: ({ row }) => (
          <span className="flex items-center gap-1">
            <HandlingPill log={row.original} />
            <RetryPills log={row.original} />
          </span>
        ),
      },
      {
        id: "cache",
        header: t("logs.cache"),
        meta: { numeric: true, label: t("logs.cache"), className: "max-lg:hidden" },
        cell: ({ row }) => {
          const r = row.original.cache_hit_rate;
          return r ? <span className="text-ink-2">{formatPercent(r, locale, 0)}</span> : <span className="text-ink-3">—</span>;
        },
      },
      {
        id: "latency",
        header: t("logs.ttfbTotal"),
        meta: { numeric: true, label: t("logs.ttfbTotal"), className: "whitespace-nowrap max-lg:hidden" },
        cell: ({ row }) => {
          const l = row.original;
          return (
            <span className="text-ink-2">
              {l.ttfb_ms !== null && l.ttfb_ms !== undefined ? formatDuration(l.ttfb_ms) : "—"}
              <span className="text-ink-3"> / </span>
              {formatDuration(l.duration_ms)}
            </span>
          );
        },
      },
      {
        id: "key",
        header: t("logs.key"),
        meta: { label: t("logs.key"), className: "max-w-[160px] max-2xl:hidden" },
        cell: ({ row }) => <span className="block truncate text-ink-2">{row.original.key?.name ?? "—"}</span>,
      },
      {
        id: "ip",
        header: t("logs.ip"),
        meta: { label: t("logs.ip"), className: "whitespace-nowrap max-lg:hidden" },
        cell: ({ row }) => <span className="font-mono text-xs text-ink-2">{row.original.client_ip ?? "—"}</span>,
      },
      ...(admin
        ? ([
            {
              id: "upstream",
              header: t("logs.upstream"),
              meta: { label: t("logs.upstream"), className: "max-w-[160px] max-lg:hidden" },
              cell: ({ row }) => <span className="block truncate text-ink-2">{row.original.upstream?.name ?? "—"}</span>,
            },
          ] satisfies ColumnDef<LogSummary>[])
        : []),
      {
        id: "charge",
        header: t("logs.charged"),
        enableHiding: false,
        meta: { numeric: true },
        cell: ({ row }) => money(row.original.charge),
      },
    ],
    [t, locale, money, admin],
  );
}

function useSessionColumns(): ColumnDef<Session>[] {
  const { t, locale } = useI18n();
  const money = useMoney();
  return useMemo(
    () => [
      {
        id: "last",
        header: t("logs.ses.last"),
        enableHiding: false,
        meta: { className: "w-[1%] whitespace-nowrap" },
        cell: ({ row }) => {
          const s = row.original;
          const span = Date.parse(s.last) - Date.parse(s.first);
          return (
            <span className="flex items-baseline gap-2">
              <RequestTime iso={s.last} />
              {span >= 1000 && <span className="text-xs text-ink-3">{t("logs.ses.lasted", { d: formatDuration(span) })}</span>}
            </span>
          );
        },
      },
      {
        id: "client",
        header: t("logs.client"),
        meta: { label: t("logs.client"), className: "w-[1%] whitespace-nowrap max-md:hidden" },
        cell: ({ row }) => <ClientName client={row.original.client} className="text-ink-2" />,
      },
      {
        id: "who",
        header: t("logs.ses.who"),
        meta: { label: t("logs.ses.who"), className: "max-w-[200px] max-lg:hidden" },
        cell: ({ row }) => {
          const s = row.original;
          const key = s.key?.name ?? "—";
          return <span className="block truncate text-ink-2">{s.user ? t("logs.keyOfUser", { key, user: s.user.name }) : key}</span>;
        },
      },
      {
        id: "models",
        header: t("logs.ses.models"),
        enableHiding: false,
        meta: { className: "max-w-[260px]" },
        cell: ({ row }) => {
          const [first, ...rest] = row.original.models;
          return (
            <span className="flex min-w-0 items-center gap-1.5">
              <span data-raw className="truncate">
                {first ?? "—"}
              </span>
              {rest.length > 0 && (
                <Tooltip content={<span data-raw>{rest.join(", ")}</span>}>
                  <span className="shrink-0 rounded-full bg-surface-2 px-1.5 text-xs tabular-nums text-ink-3">+{rest.length}</span>
                </Tooltip>
              )}
            </span>
          );
        },
      },
      {
        id: "requests",
        header: t("logs.ses.requests"),
        meta: { numeric: true, label: t("logs.ses.requests"), className: "whitespace-nowrap" },
        cell: ({ row }) => (
          <span className="inline-flex items-center gap-1.5">
            {row.original.failed > 0 && <Pill tone="bad">{t("logs.ses.failedN", { n: row.original.failed })}</Pill>}
            <span className="tabular-nums">{row.original.requests}</span>
          </span>
        ),
      },
      {
        id: "cache",
        header: t("logs.cache"),
        meta: { numeric: true, label: t("logs.cache"), className: "max-lg:hidden" },
        cell: ({ row }) => {
          const r = row.original.cache_hit_rate;
          return r ? <span className="text-ink-2">{formatPercent(r, locale, 0)}</span> : <span className="text-ink-3">—</span>;
        },
      },
      {
        id: "spend",
        header: t("logs.ses.spend"),
        enableHiding: false,
        meta: { numeric: true },
        cell: ({ row }) => money(row.original.charge),
      },
    ],
    [t, locale, money],
  );
}

/** The log's current view, from the URL. */
function useLogView(): [LogView, (v: LogView) => void] {
  const [search, set] = useUrlSearch();
  const view: LogView = search.view === "sessions" ? "sessions" : "requests";
  return [view, (v) => set({ view: v === "sessions" ? "sessions" : undefined, id: undefined })];
}

/** "Each request / By session" and the CSV download of the filtered log, for a page header. */
export function LogActions() {
  const { t, locale } = useI18n();
  const [search] = useUrlSearch();
  const [view, setView] = useLogView();
  const query = new URLSearchParams({ ...serializeLogFilters(parseLogFilters(search)), lang: locale });
  return (
    <div className="flex flex-wrap items-center gap-2">
      <Segmented
        aria-label={t("logs.view")}
        value={view}
        onChange={setView}
        options={[
          { value: "requests", label: t("logs.viewRequests") },
          { value: "sessions", label: t("logs.viewSessions") },
        ]}
      />
      <Tooltip content={t("logs.exportHint")}>
        <Button asChild>
          <a href={`/api/v1/logs/export?${query.toString()}`} download>
            <Download />
            {t("logs.export")}
          </a>
        </Button>
      </Tooltip>
    </div>
  );
}

function SessionTable({ filters, filterBar, tableHeight }: { filters: LogFilters; filterBar: ReactNode; tableHeight?: string }) {
  const { t } = useI18n();
  const [, set] = useUrlSearch();
  const query = serializeLogFilters(filters);
  const q = useInfiniteQuery({
    queryKey: ["log-sessions", query],
    initialPageParam: undefined as string | undefined,
    queryFn: ({ pageParam }) =>
      unwrap(api.GET("/logs/sessions", { params: { query: { ...filters, cursor: pageParam, limit: SESSION_PAGE } } })),
    getNextPageParam: (last) => last.next_cursor ?? undefined,
  });
  const rows = useMemo(() => q.data?.pages.flatMap((p) => p.items), [q.data]);
  const columns = useSessionColumns();
  const more = () => {
    if (q.hasNextPage && !q.isFetchingNextPage) void q.fetchNextPage();
  };
  if (q.isError)
    return (
      <div className="grid gap-3">
        {filterBar}
        <ErrorState error={q.error} onRetry={() => void q.refetch()} />
      </div>
    );
  return (
    <DataTable
      toolbar={filterBar}
      beforeTable={<RequestHistogram filters={filters} defaultRange={SESSION_SPAN} onPick={(from, to) => set({ from, to })} />}
      id="logs.sessions"
      columns={columns}
      data={rows}
      getRowId={(r) => r.id}
      onRowClick={(r) => set({ view: undefined, session_id: r.id })}
      rowClassName={(r) => (r.failed > 0 ? "[&>td:first-child]:shadow-[inset_2px_0_0_var(--bad)]" : undefined)}
      onEndReached={more}
      loadingRows={12}
      maxHeight={tableHeight}
      columnPicker
      empty={<EmptyState compact icon={ScrollText} title={t("logs.ses.empty")} hint={t("logs.ses.emptyHint")} />}
      footer={
        rows && rows.length > 0 ? (
          <span className="flex items-center justify-between gap-2">
            <span className="tabular-nums">{t("logs.ses.loaded", { n: rows.length })}</span>
            {q.hasNextPage ? (
              <Button size="sm" variant="ghost" onClick={more} disabled={q.isFetchingNextPage}>
                {q.isFetchingNextPage ? t("common.loading") : t("logs.ses.loadMore")}
              </Button>
            ) : (
              <span>{t("logs.end")}</span>
            )}
          </span>
        ) : undefined
      }
    />
  );
}

/**
 * The request log: filters (in the URL), a histogram of the filtered requests, the list (one row
 * per request, or per session with `view=sessions`), and the request drawer (`?id=`). Used by the
 * log page and by members' usage page.
 */
export function RequestLog({ admin, tableHeight }: { admin: boolean; tableHeight?: string }) {
  const { t } = useI18n();
  const [search, set] = useUrlSearch();
  const filters: LogFilters = parseLogFilters(search);
  const query = serializeLogFilters(filters);
  const drawer = useDrawerParam();
  const [view] = useLogView();

  const q = useInfiniteQuery({
    queryKey: ["logs", query],
    initialPageParam: undefined as string | undefined,
    queryFn: ({ pageParam }) =>
      unwrap(api.GET("/logs", { params: { query: { ...filters, cursor: pageParam, limit: PAGE } } })),
    getNextPageParam: (last) => last.next_cursor ?? undefined,
    enabled: view === "requests",
  });
  const rows = useMemo(() => q.data?.pages.flatMap((p) => p.items), [q.data]);
  const columns = useColumns(admin);
  const filtered = Object.keys(query).length > 0;
  const filterBar = (
    <LogFilterBar filters={filters} onChange={(patch) => set(patch)} admin={admin} defaultRange={view === "sessions" ? SESSION_SPAN : undefined} />
  );
  const more = () => {
    if (q.hasNextPage && !q.isFetchingNextPage) void q.fetchNextPage();
  };

  if (view === "sessions") return <SessionTable filters={filters} filterBar={filterBar} tableHeight={tableHeight} />;
  return (
    <>
      {q.isError ? (
        <div className="grid gap-3">
          {filterBar}
          <ErrorState error={q.error} onRetry={() => void q.refetch()} />
        </div>
      ) : (
        <DataTable
          toolbar={filterBar}
          beforeTable={<RequestHistogram filters={filters} onPick={(from, to) => set({ from, to })} />}
          id={admin ? "logs.admin" : "logs.member"}
          columns={columns}
          defaultHidden={["ip"]}
          data={rows}
          getRowId={(r) => r.id}
          onRowClick={(r) => drawer.open(r.id)}
          selectedId={drawer.id}
          rowClassName={(r) => (r.status < 200 || r.status >= 300 ? "[&>td:first-child]:shadow-[inset_2px_0_0_var(--bad)]" : undefined)}
          onEndReached={more}
          loadingRows={12}
          maxHeight={tableHeight}
          columnPicker
          empty={
            filtered ? (
              <EmptyState compact icon={ListX} title={t("logs.emptyFiltered")} hint={t("logs.emptyFilteredHint")} />
            ) : (
              <EmptyState compact icon={ScrollText} title={t("logs.empty")} hint={t("logs.emptyHint")} />
            )
          }
          footer={
            rows && rows.length > 0 ? (
              <span className="flex items-center justify-between gap-2">
                <span className="tabular-nums">{t("logs.loaded", { n: rows.length })}</span>
                {q.hasNextPage ? (
                  <Button size="sm" variant="ghost" onClick={more} disabled={q.isFetchingNextPage}>
                    {q.isFetchingNextPage ? t("common.loading") : t("logs.loadMore")}
                  </Button>
                ) : (
                  <span>{t("logs.end")}</span>
                )}
              </span>
            ) : undefined
          }
        />
      )}
      <LogDrawer id={drawer.id} onOpenChange={drawer.onOpenChange} />
    </>
  );
}
