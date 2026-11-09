import { useInfiniteQuery, useQuery } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import type { ColumnDef } from "@tanstack/react-table";
import { ReceiptText } from "lucide-react";
import { useMemo } from "react";
import { DataTable } from "@/components/data-table";
import { DetailList, Drawer } from "@/components/drawer";
import { ClearFilters, FilterBar, FilterSelect } from "@/components/filter-bar";
import { EmptyState, ErrorState, LoadingRows, PageHeader } from "@/components/page";
import { StatTile } from "@/components/stat-tile";
import { Pill } from "@/components/status";
import { Button } from "@/components/ui/button";
import { useMoney } from "@/components/money";
import { formatDateTime, formatWhen } from "@/lib/format";
import { useDrawerParam, useUrlSearch } from "@/lib/use-url-search";
import { useI18n } from "@/i18n";
import { paymentApi, type Order, type OrderKind, type OrderStatus } from "../api";
import { ORDER_TONE, SubjectCell, useNames, usePayMethodLabel } from "../shared";
import { str } from "./parts";

const STATUSES: readonly OrderStatus[] = ["pending", "paid", "canceled"];
const KINDS: readonly OrderKind[] = ["topup", "plan"];
const PROVIDERS = ["stripe", "epay"] as const;

function pick<T extends string>(v: unknown, allowed: readonly T[]): T | undefined {
  const s = str(v);
  return s && (allowed as readonly string[]).includes(s) ? (s as T) : undefined;
}

/** Every payment at a provider: top-ups and plan periods, with their status and what was charged. */
export function OrdersPage() {
  const { t, locale } = useI18n();
  const money = useMoney();
  const names = useNames();
  const method = usePayMethodLabel();
  const [search, set] = useUrlSearch();
  const status = pick(search.status, STATUSES);
  const kind = pick(search.kind, KINDS);
  const provider = pick(search.provider, PROVIDERS);
  const drawer = useDrawerParam();
  const q = useInfiniteQuery({
    queryKey: ["billing", "orders", status, kind, provider],
    queryFn: ({ pageParam }) => paymentApi.orders({ status, kind, provider, cursor: pageParam || undefined, limit: 50 }),
    initialPageParam: "",
    getNextPageParam: (last) => last.next_cursor || undefined,
  });
  const first = q.data?.pages[0];
  const cur = first?.currency ?? "USD";
  const totals = first?.totals;
  const rows = useMemo(() => q.data?.pages.flatMap((p) => p.items), [q.data]);
  const columns = useMemo<ColumnDef<Order>[]>(
    () => [
      {
        id: "time",
        header: t("billing.op.ord.time"),
        meta: { className: "w-[110px]" },
        cell: ({ row }) => (
          <span className="whitespace-nowrap text-xs text-ink-2 tabular-nums" title={formatDateTime(row.original.created_at, locale)}>
            {formatWhen(row.original.created_at, locale)}
          </span>
        ),
      },
      { id: "user", header: t("billing.op.ord.user"), cell: ({ row }) => <SubjectCell name={names.user(row.original.user_id)} /> },
      {
        id: "what",
        header: t("billing.op.ord.what"),
        meta: { className: "w-[170px] max-lg:hidden" },
        cell: ({ row }) => (
          <span className="grid min-w-0">
            <span className="text-sm">{t(`billing.order.kind.${row.original.kind}`)}</span>
            {row.original.kind === "plan" && row.original.note && <span className="truncate text-xs text-ink-3">{row.original.note}</span>}
          </span>
        ),
      },
      { id: "amount", header: t("billing.op.ord.amount"), meta: { numeric: true, className: "w-[120px]" }, cell: ({ row }) => money(row.original.amount) },
      {
        id: "pay",
        header: t("billing.op.ord.pay"),
        meta: { numeric: true, className: "w-[150px]" },
        cell: ({ row }) => (
          <span className="grid justify-items-end">
            <span className="font-medium">{money(row.original.pay)}</span>
            {row.original.discount.micro > 0 && (
              <span className="text-xs text-ink-3">{t("billing.op.ord.discountShort", { amount: money(row.original.discount) })}</span>
            )}
          </span>
        ),
      },
      {
        id: "method",
        header: t("billing.op.ord.method"),
        meta: { className: "w-[150px] max-xl:hidden" },
        cell: ({ row }) => <span className="text-sm">{method({ provider: row.original.provider, method: row.original.method })}</span>,
      },
      {
        id: "status",
        header: t("billing.op.ord.status"),
        meta: { className: "w-[120px]" },
        cell: ({ row }) => (
          <Pill tone={ORDER_TONE[row.original.status]} dot>
            {t(`billing.order.${row.original.status}`)}
          </Pill>
        ),
      },
    ],
    [t, locale, money, names, method],
  );
  const filters = [status, kind, provider].filter(Boolean).length;
  const selected = rows?.find((o) => o.id === drawer.id);

  return (
    <>
      <PageHeader title={t("nav.billing.orders")} description={t("billing.op.ord.description")} />
      <div className="mb-4 grid grid-cols-2 gap-3 lg:grid-cols-3">
        <StatTile label={t("billing.op.ord.statPaid")} loading={q.isPending} value={money({ micro: totals?.paid_micro ?? 0, currency: cur })} note={t("billing.op.ord.last30")} />
        <StatTile label={t("billing.op.ord.statCount")} loading={q.isPending} value={totals?.paid_count ?? 0} note={t("billing.op.ord.last30")} />
        <StatTile label={t("billing.op.ord.statPending")} loading={q.isPending} value={totals?.pending_count ?? 0} note={t("billing.op.ord.pendingNote")} onClick={() => set({ status: "pending" })} />
      </div>
      {q.isError ? (
        <ErrorState error={q.error} onRetry={() => void q.refetch()} />
      ) : (
        <DataTable
          columns={columns}
          data={rows}
          getRowId={(o) => o.id}
          onRowClick={(o) => drawer.open(o.id)}
          selectedId={drawer.id}
          toolbar={
            <FilterBar>
              <FilterSelect
                label={t("billing.op.ord.status")}
                allLabel={t("billing.op.ord.allStatuses")}
                value={status}
                options={STATUSES.map((s) => ({ value: s, label: t(`billing.order.${s}`) }))}
                onChange={(v) => set({ status: v })}
              />
              <FilterSelect
                label={t("billing.op.ord.what")}
                allLabel={t("billing.op.ord.allKinds")}
                value={kind}
                options={KINDS.map((k) => ({ value: k, label: t(`billing.order.kind.${k}`) }))}
                onChange={(v) => set({ kind: v })}
              />
              <FilterSelect
                label={t("billing.op.ord.provider")}
                allLabel={t("billing.op.ord.allProviders")}
                value={provider}
                options={PROVIDERS.map((p) => ({ value: p, label: t(`billing.op.provider.${p}`) }))}
                onChange={(v) => set({ provider: v })}
              />
              <ClearFilters count={filters} onClear={() => set({ status: undefined, kind: undefined, provider: undefined })} />
            </FilterBar>
          }
          empty={<EmptyState icon={ReceiptText} title={filters ? t("billing.op.ord.noMatch") : t("billing.op.ord.empty")} hint={filters ? undefined : t("billing.op.ord.emptyHint")} />}
          footer={
            q.hasNextPage ? (
              <Button size="sm" variant="ghost" onClick={() => void q.fetchNextPage()} disabled={q.isFetchingNextPage}>
                {q.isFetchingNextPage ? t("common.loading") : t("common.loadMore")}
              </Button>
            ) : rows && rows.length > 0 ? (
              t("billing.op.ord.count", { n: rows.length })
            ) : undefined
          }
        />
      )}
      {drawer.id && <OrderDrawer id={drawer.id} known={selected} onClose={drawer.close} />}
    </>
  );
}

function OrderDrawer({ id, known, onClose }: { id: string; known?: Order; onClose: () => void }) {
  const { t, locale } = useI18n();
  const money = useMoney();
  const names = useNames();
  const method = usePayMethodLabel();
  const q = useQuery({ queryKey: ["billing", "order", id], queryFn: () => paymentApi.order(id), initialData: known });
  const o = q.data;
  return (
    <Drawer
      open
      onOpenChange={(v) => !v && onClose()}
      title={o ? t("billing.op.ord.drawerTitle", { kind: t(`billing.order.kind.${o.kind}`), amount: money(o.amount) }) : t("billing.op.ord.order")}
      meta={
        o && (
          <Pill tone={ORDER_TONE[o.status]} dot>
            {t(`billing.order.${o.status}`)}
          </Pill>
        )
      }
      subtitle={o ? formatDateTime(o.created_at, locale) : undefined}
    >
      <div className="p-5">
        {q.isError ? (
          <ErrorState error={q.error} onRetry={() => void q.refetch()} />
        ) : !o ? (
          <LoadingRows rows={6} />
        ) : (
          <DetailList
            items={[
              { label: t("billing.op.ord.user"), value: <SubjectCell name={names.user(o.user_id)} /> },
              { label: t("billing.op.ord.account"), value: <SubjectCell name={names.subject(o.subject)} /> },
              { label: t("billing.op.ord.what"), value: o.kind === "plan" ? t("billing.op.ord.planOf", { name: o.note ?? "" }) : t("billing.order.kind.topup") },
              { label: t("billing.op.ord.amount"), value: money(o.amount) },
              { label: t("billing.op.ord.pay"), value: <span className="font-medium">{money(o.pay)}</span> },
              ...(o.coupon_code
                ? [
                    {
                      label: t("billing.op.ord.coupon"),
                      value: (
                        <span>
                          <Link to="/billing/$page" params={{ page: "coupons" }} search={{ id: o.coupon_id }} className="font-mono text-accent hover:underline" data-raw>
                            {o.coupon_code}
                          </Link>
                          <span className="ml-2 text-ink-2">{t("billing.op.ord.discountShort", { amount: money(o.discount) })}</span>
                        </span>
                      ),
                    },
                  ]
                : []),
              { label: t("billing.op.ord.method"), value: method({ provider: o.provider, method: o.method }) },
              { label: t("billing.op.ord.created"), value: formatDateTime(o.created_at, locale) },
              { label: t("billing.op.ord.paidAt"), value: o.paid_at ? formatDateTime(o.paid_at, locale) : <span className="text-ink-3">{t("billing.op.ord.notPaid")}</span> },
              ...(o.kind === "plan" && o.ref
                ? [
                    {
                      label: t("billing.op.ord.subscription"),
                      value: (
                        <Link to="/billing/$page" params={{ page: "plans" }} className="text-accent hover:underline">
                          {t("billing.op.ord.openPlans")}
                        </Link>
                      ),
                    },
                  ]
                : []),
              { label: t("billing.op.ord.orderId"), value: <span className="font-mono text-xs" data-raw>{o.id}</span> },
              ...(o.session_id ? [{ label: t("billing.op.ord.session"), value: <span className="font-mono text-xs break-all" data-raw>{o.session_id}</span> }] : []),
            ]}
          />
        )}
        {o?.status === "pending" && <p className="mt-4 text-xs text-ink-3">{t("billing.op.ord.pendingHelp")}</p>}
      </div>
    </Drawer>
  );
}
