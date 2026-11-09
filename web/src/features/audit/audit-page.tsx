import { useInfiniteQuery, useQuery } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import type { ColumnDef } from "@tanstack/react-table";
import { ArrowRight, ArrowUpRight, History, SearchX, X } from "lucide-react";
import { useMemo } from "react";
import { DataTable } from "@/components/data-table";
import { DetailList, Drawer } from "@/components/drawer";
import { ClearFilters, FilterBar, FilterSelect } from "@/components/filter-bar";
import { EmptyState, ErrorState, PageHeader } from "@/components/page";
import { Pill, StatusDot } from "@/components/status";
import { Button } from "@/components/ui/button";
import { api, unwrap } from "@/lib/api/client";
import type { AuditEntry } from "@/lib/api/types";
import { formatDateTime, formatRelative } from "@/lib/format";
import { formatMoney } from "@/lib/money";
import { useDrawerParam, useUrlSearch } from "@/lib/use-url-search";
import { useI18n } from "@/i18n";
import {
  actionText,
  actionTone,
  actorText,
  CATEGORIES,
  diffLeaves,
  fieldText,
  targetLink,
  targetName,
  TARGET_TYPES,
  targetTypeText,
  valueText,
  type Names,
} from "./audit-words";

const PAGE = 50;
const str = (v: unknown) => (typeof v === "string" && v !== "" ? v : undefined);

function useNames(): Names {
  const { pick } = useI18n();
  const features = useQuery({ queryKey: ["features"], queryFn: () => unwrap(api.GET("/features")), retry: false });
  const settings = useQuery({ queryKey: ["settings"], queryFn: () => unwrap(api.GET("/settings")), retry: false });
  return useMemo(
    () => ({ features: features.data?.items ?? [], settings: settings.data?.items ?? [], pick }),
    [features.data, settings.data, pick],
  );
}

/** "a → b" for one change; the old value struck through. */
function FromTo({ from, to, format }: { from: unknown; to: unknown; format?: (v: unknown) => unknown }) {
  const { t, has } = useI18n();
  const hasFrom = from !== undefined;
  const show = (v: unknown) => valueText(format && v !== undefined ? format(v) : v, t, has);
  return (
    <span className="inline-flex min-w-0 flex-wrap items-center gap-1.5">
      {hasFrom && <span className="break-all text-bad line-through decoration-bad/50">{show(from)}</span>}
      {hasFrom && <ArrowRight className="size-3 shrink-0 text-ink-3" aria-label={t("audit.becomes")} />}
      <span className="break-all text-ok">{show(to)}</span>
    </span>
  );
}

/** Prices are stored in micro-units per million tokens; show them as money. */
function priceFormat(c: { field: string; from?: unknown; to?: unknown }, locale: string): ((v: unknown) => unknown) | undefined {
  if (c.field !== "prices") return undefined;
  const cur = [c.to, c.from].map((x) => (x && typeof x === "object" ? (x as { currency?: unknown }).currency : undefined)).find((x) => typeof x === "string");
  if (typeof cur !== "string") return undefined;
  return (v) => (typeof v === "number" ? formatMoney({ micro: v, currency: cur }, locale) : v);
}

function FieldName({ field, names }: { field: string; names: Names }) {
  const { t, has } = useI18n();
  const named = fieldText(field, t, has, names);
  return named ? <>{named}</> : <code className="font-mono text-xs">{field}</code>;
}

/** The change column: one simple change inline, otherwise a count. */
function ChangeSummary({ e, names }: { e: AuditEntry; names: Names }) {
  const { t, has } = useI18n();
  const changes = e.changes ?? [];
  if (e.target.type === "plugin" && e.target.name) return <code className="font-mono text-xs text-ink-3">{e.target.name}</code>;
  if (changes.length === 0) return <span className="text-ink-3">—</span>;
  const c = changes[0]!;
  const simple = (v: unknown) => v === undefined || v === null || typeof v !== "object" || Array.isArray(v);
  if (changes.length === 1 && simple(c.from) && simple(c.to)) {
    const named = fieldText(c.field, t, has, names);
    return (
      <span className="flex min-w-0 items-center gap-1.5 text-xs">
        {named && <span className="shrink-0 text-ink-3">{named}</span>}
        <span className="min-w-0 truncate">
          <FromTo from={c.from} to={c.to} />
        </span>
      </span>
    );
  }
  const n = changes.reduce((sum, x) => sum + (x.from === undefined && x.to === undefined ? 1 : diffLeaves(x.from, x.to).length), 0);
  if (n === 0) return <span className="text-xs text-ink-3">{t("audit.nothingVisible")}</span>;
  return <span className="text-xs text-ink-2">{t("audit.changeCount", { n })}</span>;
}

function ChangeList({ e, names }: { e: AuditEntry; names: Names }) {
  const { t, locale } = useI18n();
  const changes = e.changes ?? [];
  if (changes.length === 0) return <p className="text-sm text-ink-3">{t("audit.noChanges")}</p>;
  return (
    <ul className="divide-y divide-line-soft rounded-lg border border-line">
      {changes.map((c, i) => {
        const leaves = diffLeaves(c.from, c.to);
        const format = priceFormat(c, locale);
        const nested = leaves.length > 1 || (leaves.length === 1 && leaves[0]!.path !== "");
        return (
          <li key={i} className="grid gap-1.5 px-3 py-2.5 text-sm">
            <span className="font-medium">
              <FieldName field={c.field} names={names} />
            </span>
            {nested ? (
              <ul className="grid gap-1">
                {leaves.map((l) => (
                  <li key={l.path} className="grid grid-cols-[minmax(0,2fr)_minmax(0,3fr)] gap-3 text-xs">
                    <code className="truncate font-mono text-ink-3" title={l.path}>
                      {l.path}
                    </code>
                    <FromTo from={l.from} to={l.to} format={format} />
                  </li>
                ))}
              </ul>
            ) : leaves.length === 0 && c.to === undefined && c.from === undefined ? (
              <span className="text-xs text-ink-3">{t("audit.removed")}</span>
            ) : leaves.length === 0 ? (
              <span className="text-xs text-ink-3">{t("audit.nothingVisible")}</span>
            ) : (
              <span className="text-xs">
                <FromTo from={c.from} to={c.to} />
              </span>
            )}
          </li>
        );
      })}
    </ul>
  );
}

function EntryDrawer({ e, names, onClose, onHistory }: { e: AuditEntry | undefined; names: Names; onClose: () => void; onHistory: (e: AuditEntry) => void }) {
  const { t, has, locale } = useI18n();
  const link = e && targetLink(e);
  const name = e && targetName(e, names);
  return (
    <Drawer
      open={e !== undefined}
      onOpenChange={(o) => !o && onClose()}
      title={e ? actionText(e.action, t, has) : ""}
      subtitle={e && formatRelative(e.time, locale)}
      meta={e && <StatusDot tone={actionTone(e.action)} />}
      size="md"
    >
      {e && (
        <div className="grid gap-6 p-5">
          <DetailList
            items={[
              { label: t("audit.when"), value: formatDateTime(e.time, locale) },
              {
                label: t("audit.actor"),
                value: (
                  <span>
                    {actorText(e, t)}
                    {e.ip && <span className="ml-2 text-xs text-ink-3">{t("audit.fromIp", { ip: e.ip })}</span>}
                  </span>
                ),
              },
              {
                label: t("audit.target"),
                value: (
                  <span className="flex flex-wrap items-center gap-2">
                    <Pill>{targetTypeText(e.target.type, t, has)}</Pill>
                    {name && <span className="font-medium">{name}</span>}
                    {link && (
                      <Link to={link.to} search={link.search} className="inline-flex items-center gap-0.5 text-xs text-accent hover:underline">
                        {t("audit.open")}
                        <ArrowUpRight className="size-3" aria-hidden />
                      </Link>
                    )}
                  </span>
                ),
              },
            ]}
          />
          <section className="grid gap-2">
            <h3 className="text-xs font-semibold text-ink-3">{t("audit.changes")}</h3>
            <ChangeList e={e} names={names} />
          </section>
          {e.target.type !== "settings" && (
            <div>
              <Button size="sm" onClick={() => onHistory(e)}>
                <History aria-hidden />
                {t("audit.history")}
              </Button>
            </div>
          )}
          <details className="text-xs text-ink-3">
            <summary className="cursor-pointer select-none hover:text-ink-2">{t("audit.technical")}</summary>
            <dl className="mt-2 grid grid-cols-[max-content_1fr] gap-x-4 gap-y-1">
              <dt>{t("audit.tech.action")}</dt>
              <dd>
                <code className="font-mono">{e.action}</code>
              </dd>
              <dt>{t("audit.tech.target")}</dt>
              <dd className="break-all">
                <code className="font-mono">
                  {e.target.type}:{e.target.id}
                </code>
              </dd>
              {e.target.type === "plugin" && e.target.name && (
                <>
                  <dt>{t("audit.tech.call")}</dt>
                  <dd>
                    <code className="font-mono">{e.target.name}</code>
                  </dd>
                </>
              )}
              <dt>{t("audit.tech.entry")}</dt>
              <dd className="break-all">
                <code className="font-mono">{e.id}</code>
              </dd>
            </dl>
          </details>
        </div>
      )}
    </Drawer>
  );
}

/** Every change made through the admin API, newest first; filters live in the URL. */
export function AuditPage() {
  const { t, has, locale } = useI18n();
  const [search, set] = useUrlSearch();
  const drawer = useDrawerParam();
  const names = useNames();
  const cat = str(search.cat);
  const actor = str(search.actor);
  const type = str(search.type);
  const target = str(search.target);
  const actions = CATEGORIES.find((c) => c.id === cat)?.actions;
  const q = useInfiniteQuery({
    queryKey: ["audit", actions, actor, type, target],
    initialPageParam: undefined as string | undefined,
    queryFn: ({ pageParam }) =>
      unwrap(
        api.GET("/audit", {
          params: {
            query: {
              cursor: pageParam,
              limit: PAGE,
              action: actions,
              actor_id: actor,
              target_type: type,
              target_id: type ? target : undefined,
            },
          },
        }),
      ),
    getNextPageParam: (p) => p.next_cursor ?? undefined,
  });
  const users = useQuery({ queryKey: ["users", "audit-actors"], queryFn: () => unwrap(api.GET("/users")), retry: false });
  const items = useMemo(() => q.data?.pages.flatMap((p) => p.items) ?? [], [q.data]);

  const actorOptions = useMemo(() => {
    const seen = new Map<string, string>();
    for (const u of users.data?.items ?? []) seen.set(u.id, u.display_name || u.username);
    for (const e of items) if (!seen.has(e.actor.id)) seen.set(e.actor.id, actorText(e, t));
    if (actor && !seen.has(actor)) seen.set(actor, actor);
    return [...seen].map(([value, label]) => ({ value, label, search: label }));
  }, [users.data, items, actor, t]);

  const columns = useMemo<ColumnDef<AuditEntry>[]>(
    () => [
      {
        id: "time",
        header: t("audit.time"),
        meta: { className: "w-[132px]" },
        cell: ({ row }) => (
          <span className="whitespace-nowrap text-xs text-ink-2 tabular-nums" title={formatDateTime(row.original.time, locale)}>
            {formatRelative(row.original.time, locale)}
          </span>
        ),
      },
      {
        id: "actor",
        header: t("audit.actor"),
        meta: { className: "w-[180px]" },
        cell: ({ row }) => (
          <span className="block min-w-0 truncate">
            {actorText(row.original, t)}
            {row.original.ip && <span className="ml-1.5 text-xs text-ink-3">{row.original.ip}</span>}
          </span>
        ),
      },
      {
        id: "action",
        header: t("audit.action"),
        cell: ({ row }) => {
          const e = row.original;
          const name = targetName(e, names);
          return (
            <span className="flex min-w-0 items-center gap-2">
              <StatusDot tone={actionTone(e.action)} />
              <span className="whitespace-nowrap">{actionText(e.action, t, has)}</span>
              <span className="min-w-0 truncate text-ink-3">
                {targetTypeText(e.target.type, t, has)}
                {name && <span className="ml-1 font-medium text-ink">{name}</span>}
              </span>
            </span>
          );
        },
      },
      {
        id: "changes",
        header: t("audit.changes"),
        meta: { className: "hidden w-[34%] max-w-0 lg:table-cell" },
        cell: ({ row }) => <ChangeSummary e={row.original} names={names} />,
      },
    ],
    [t, has, locale, names],
  );

  const active = [cat, actor, type].filter(Boolean).length;
  const targetEntry = target ? items.find((e) => e.target.id === target) : undefined;
  const open = items.find((e) => e.id === drawer.id);
  const filtered = active > 0 || target;

  return (
    <>
      <PageHeader title={t("nav.audit")} description={t("audit.description")} />
      <div className="grid gap-3">
        <FilterBar>
          <FilterSelect
            label={t("audit.filter.category")}
            allLabel={t("audit.allCategories")}
            value={cat}
            onChange={(v) => set({ cat: v })}
            options={CATEGORIES.map((c) => ({ value: c.id, label: t(c.label) }))}
          />
          <FilterSelect label={t("audit.filter.actor")} allLabel={t("audit.allActors")} value={actor} onChange={(v) => set({ actor: v })} options={actorOptions} />
          <FilterSelect
            label={t("audit.filter.type")}
            allLabel={t("audit.allTypes")}
            value={type}
            onChange={(v) => set({ type: v, target: undefined })}
            options={TARGET_TYPES.map((x) => ({ value: x, label: targetTypeText(x, t, has) }))}
          />
          {target && type && (
            <span className="inline-flex h-7 items-center gap-1 rounded-full border border-accent/40 bg-accent-soft pl-2.5 pr-1 text-xs font-medium text-accent-strong">
              {t("audit.onlyThis", { name: (targetEntry && targetName(targetEntry, names)) ?? targetTypeText(type, t, has) })}
              <button type="button" onClick={() => set({ target: undefined })} aria-label={t("filter.clear")} className="grid size-5 place-items-center rounded-full hover:bg-accent/15">
                <X className="size-3" />
              </button>
            </span>
          )}
          <ClearFilters count={active} onClear={() => set({ cat: undefined, actor: undefined, type: undefined, target: undefined })} />
        </FilterBar>
        {q.isError ? (
          <ErrorState error={q.error} onRetry={() => void q.refetch()} />
        ) : (
          <DataTable
            id="audit"
            columns={columns}
            data={q.isPending ? undefined : items}
            getRowId={(e) => e.id}
            onRowClick={(e) => drawer.open(e.id)}
            selectedId={drawer.id}
            maxHeight="calc(100dvh - 15rem)"
            empty={
              filtered ? (
                <EmptyState compact icon={SearchX} title={t("audit.emptyFiltered")} hint={t("audit.emptyFilteredHint")} />
              ) : (
                <EmptyState compact icon={History} title={t("audit.empty")} hint={t("audit.emptyHint")} />
              )
            }
            footer={
              items.length > 0 && (
                <div className="flex items-center justify-between gap-2 py-1">
                  <span>{t("audit.shown", { n: items.length })}</span>
                  {q.hasNextPage ? (
                    <Button size="sm" onClick={() => void q.fetchNextPage()} disabled={q.isFetchingNextPage}>
                      {q.isFetchingNextPage ? t("common.loading") : t("audit.loadMore")}
                    </Button>
                  ) : (
                    <span>{t("audit.end")}</span>
                  )}
                </div>
              )
            }
          />
        )}
      </div>
      <EntryDrawer
        e={open}
        names={names}
        onClose={drawer.close}
        onHistory={(e) => set({ type: e.target.type, target: e.target.id, id: undefined, cat: undefined, actor: undefined })}
      />
    </>
  );
}
