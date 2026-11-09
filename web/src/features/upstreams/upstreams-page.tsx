import { Link, useNavigate } from "@tanstack/react-router";
import { createColumnHelper } from "@tanstack/react-table";
import { PlugZap, Plus, Server } from "lucide-react";
import { useMemo, useState } from "react";
import { DataTable } from "@/components/data-table";
import { ClearFilters, FilterBar, FilterSearch, FilterToggle } from "@/components/filter-bar";
import { HourlyBar } from "@/components/hourly-bar";
import { EmptyState, ErrorState, PageHeader } from "@/components/page";
import { Pill, StatusDot } from "@/components/status";
import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import { Tooltip } from "@/components/ui/tooltip";
import type { Upstream } from "@/lib/api/types";
import { cn } from "@/lib/cn";
import { formatClock, formatDuration, formatPercent } from "@/lib/format";
import { pickFlag, pickString } from "@/lib/url-filters";
import { useDrawerParam, useUrlSearch } from "@/lib/use-url-search";
import { useI18n } from "@/i18n";
import type { MessageKey } from "@/i18n/en";
import { CredentialSummary, QuotaBar } from "./credential-state";
import { hostOf, manifestTitle, protocolKey, sortProtocols, UPSTREAM_TONE, upstreamState } from "./labels";
import { UpstreamDrawer } from "./upstream-drawer";
import { useLastTest, useTestUpstreams } from "./use-upstream-test";
import { useManifests, useUpstreams } from "@/lib/api/queries";

/** "火山方舟 · 官方模板": the provider in the console's language and where the upstream came from. */
export function useSourceText() {
  const { t, locale } = useI18n();
  const manifests = useManifests();
  return (u: Upstream) => {
    const m = manifests.data?.items.find((x) => x.name === u.source.name);
    const type = m ? manifestTitle(m, locale) : u.type_label;
    const src = u.source.kind === "official" ? t("upstreams.src.official") : u.source.kind === "file" ? t("upstreams.src.file") : t("upstreams.src.site");
    return [type, src].filter(Boolean).join(" · ");
  };
}

function TestOutcome({ id }: { id: string }) {
  const { t } = useI18n();
  const results = useLastTest(id);
  if (!results) return null;
  const failed = results.filter((r) => !r.ok);
  const okOnes = results.filter((r) => r.ok);
  const avg = okOnes.length ? okOnes.reduce((s, r) => s + (r.ttfb_ms ?? r.latency_ms ?? 0), 0) / okOnes.length : 0;
  const text = failed.length ? t("upstreams.testFailedShort", { n: failed.length, of: results.length }) : t("upstreams.testOkShort", { d: formatDuration(avg) });
  const body = <span className={cn("whitespace-nowrap text-xs tabular-nums", failed.length ? "text-bad" : "text-ok")}>{text}</span>;
  if (!failed.length) return body;
  return <Tooltip content={failed.map((f) => `${f.status ?? ""} ${f.error ?? ""}`.trim()).join("\n").slice(0, 400)}>{body}</Tooltip>;
}

/** The lowest probed quota among an upstream's credentials, or nothing when none was probed. */
function QuotaCell({ u }: { u: Upstream }) {
  const { t, locale } = useI18n();
  const probed = u.credentials.map((c) => c.state.quota).filter((q) => q && q.remaining >= 0);
  if (probed.length === 0) return <span className="text-ink-3">—</span>;
  const low = Math.min(...probed.map((q) => q!.remaining));
  return (
    <div className="grid w-20 gap-1">
      <QuotaBar remaining={low} label={t("cred.quotaLeft")} />
      <span className="text-xs tabular-nums text-ink-2">{t("upstreams.quotaLeftShort", { pct: formatPercent(low, locale, 0) })}</span>
    </div>
  );
}

function StateLine({ u }: { u: Upstream }) {
  const { t, locale } = useI18n();
  if (u.circuit.state === "open")
    return (
      <Pill tone="bad" dot>
        {u.circuit.until ? t("upstreams.pausedUntil", { at: formatClock(u.circuit.until, locale).slice(0, 5) }) : t("upstreams.paused")}
      </Pill>
    );
  if (u.circuit.state === "half_open")
    return (
      <Pill tone="warn" dot>
        {t("upstreams.probing")}
      </Pill>
    );
  return null;
}

const col = createColumnHelper<Upstream>();

export function UpstreamsPage() {
  const { t, locale } = useI18n();
  const navigate = useNavigate();
  const [search, setSearch] = useUrlSearch();
  const drawer = useDrawerParam();
  const q = pickString(search.q);
  const attention = pickFlag(search.attention) ?? false;
  const list = useUpstreams({ refetchInterval: 15_000 });
  const test = useTestUpstreams();
  const source = useSourceText();
  const [selected, setSelected] = useState<Set<string>>(new Set());

  const all = useMemo(() => list.data?.items ?? [], [list.data]);
  const items = useMemo(() => {
    const f = q?.toLowerCase();
    return all.filter((u) => {
      if (attention && !["paused", "probing", "attention"].includes(upstreamState(u))) return false;
      if (!f) return true;
      return [u.name, u.id, u.type_label ?? "", ...(u.tags ?? [])].some((s) => s.toLowerCase().includes(f));
    });
  }, [all, q, attention]);

  const counts = useMemo(() => {
    const states = all.map(upstreamState);
    return { paused: states.filter((s) => s === "paused").length, attention: states.filter((s) => s === "attention" || s === "probing").length };
  }, [all]);

  const toggle = (id: string) =>
    setSelected((s) => {
      const n = new Set(s);
      if (n.has(id)) n.delete(id);
      else n.add(id);
      return n;
    });

  const columns = useMemo(
    () => [
      col.display({
        id: "select",
        enableHiding: false,
        meta: { className: "w-9 !pr-0" },
        header: () => (
          <Checkbox
            aria-label={t("common.selectAll")}
            checked={items.length > 0 && items.every((u) => selected.has(u.id)) ? true : selected.size ? "indeterminate" : false}
            onChange={(c) => setSelected(c ? new Set(items.map((u) => u.id)) : new Set())}
          />
        ),
        cell: ({ row }) => (
          <span onClick={(e) => e.stopPropagation()} className="flex">
            <Checkbox aria-label={t("common.select", { name: row.original.name })} checked={selected.has(row.original.id)} onChange={() => toggle(row.original.id)} />
          </span>
        ),
      }),
      col.accessor("name", {
        header: t("upstreams.col.name"),
        enableHiding: false,
        meta: { className: "min-w-48" },
        cell: ({ row: { original: u } }) => {
          const st = upstreamState(u);
          return (
            <div className={cn("flex min-w-0 max-w-64 items-center gap-2.5 py-1.5", !u.enabled && "opacity-60")}>
              <StatusDot tone={UPSTREAM_TONE[st]} pulse={st === "paused"} label={t(`upstreams.state.${st}` as MessageKey)} />
              <div className="min-w-0">
                <div className="flex items-center gap-1.5">
                  <span className="truncate font-medium">{u.name}</span>
                  {!u.enabled && <Pill tone="neutral">{t("upstreams.state.off")}</Pill>}
                </div>
                <div className="truncate text-xs text-ink-3">
                  {source(u)}
                </div>
              </div>
            </div>
          );
        },
      }),
      col.display({
        id: "endpoints",
        header: t("upstreams.col.endpoints"),
        meta: { className: "hidden xl:table-cell", label: t("upstreams.col.endpoints") },
        cell: ({ row: { original: u } }) => {
          const eps = sortProtocols(u.endpoints);
          const labels = [...new Set(eps.map((e) => t(protocolKey(e.protocol))))];
          return (
            <Tooltip content={eps.map((e) => `${t(protocolKey(e.protocol))} · ${hostOf(e.base_url)}`).join("\n")}>
              <span className="flex max-w-40 items-center gap-1.5 text-[13px] text-ink-2">
                <span className="truncate">{labels.slice(0, 2).join(" · ")}</span>
                {labels.length > 2 && <span className="shrink-0 text-xs text-ink-3">+{labels.length - 2}</span>}
              </span>
            </Tooltip>
          );
        },
      }),
      col.display({
        id: "health",
        header: t("upstreams.col.health"),
        meta: { className: "hidden w-40 md:table-cell", label: t("upstreams.col.health") },
        cell: ({ row: { original: u } }) => (
          <div className="w-36">
            <div className="flex items-baseline justify-between gap-2 text-xs">
              <span
                className={cn(
                  "font-medium tabular-nums",
                  u.stats.success_rate_24h !== null && u.stats.success_rate_24h < 0.95 ? "text-bad" : u.stats.success_rate_24h !== null && u.stats.success_rate_24h < 0.99 ? "text-warn" : "text-ink",
                )}
              >
                {u.stats.success_rate_24h === null ? "—" : formatPercent(u.stats.success_rate_24h, locale)}
              </span>
              <span className="tabular-nums text-ink-3">{t("upstreams.reqCount", { n: u.stats.requests_24h })}</span>
            </div>
            <HourlyBar hourly={u.stats.hourly} className="mt-1" />
          </div>
        ),
      }),
      col.display({
        id: "credentials",
        header: t("upstreams.col.credentials"),
        enableHiding: false,
        cell: ({ row: { original: u } }) => (
          <div className="flex flex-wrap items-center gap-1 py-1.5">
            <StateLine u={u} />
            {u.circuit.state === "open" && u.credentials.every((c) => c.state.status === "circuit_open") ? null : (
              <CredentialSummary credentials={u.circuit.state === "open" ? u.credentials.filter((c) => c.state.status !== "circuit_open") : u.credentials} upstreamId={u.id} keyOptional={u.key_optional} />
            )}
          </div>
        ),
      }),
      col.display({
        id: "quota",
        header: t("upstreams.col.quota"),
        meta: { className: "hidden xl:table-cell", label: t("upstreams.col.quota") },
        cell: ({ row: { original: u } }) => <QuotaCell u={u} />,
      }),
      col.display({
        id: "actions",
        enableHiding: false,
        meta: { className: "w-px text-right" },
        cell: ({ row: { original: u } }) => (
          <div className="flex items-center justify-end gap-2" onClick={(e) => e.stopPropagation()}>
            <TestOutcome id={u.id} />
            <Button size="sm" onClick={() => test.mutate({ ids: [u.id] })} disabled={test.isPending}>
              {t("upstreams.test")}
            </Button>
          </div>
        ),
      }),
    ],
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [t, locale, items, selected, test.isPending],
  );

  const filtered = Boolean(q) || attention;
  return (
    <>
      <PageHeader
        title={t("nav.upstreams")}
        description={t("upstreams.description")}
        meta={
          list.data && (
            <>
              {counts.paused > 0 && <Pill tone="bad" dot>{t("upstreams.countPaused", { n: counts.paused })}</Pill>}
              {counts.attention > 0 && <Pill tone="warn" dot>{t("upstreams.countAttention", { n: counts.attention })}</Pill>}
            </>
          )
        }
        actions={
          <>
            <Button onClick={() => test.mutate({ ids: selected.size ? [...selected] : items.map((u) => u.id) })} disabled={test.isPending || items.length === 0}>
              <PlugZap />
              {test.isPending ? t("upstreams.testing") : selected.size ? t("upstreams.testSelected", { n: selected.size }) : t("upstreams.testAll")}
            </Button>
            <Button variant="primary" asChild>
              <Link to="/upstreams/new">
                <Plus />
                {t("upstreams.add")}
              </Link>
            </Button>
          </>
        }
      />
      {list.isError ? (
        <ErrorState error={list.error} onRetry={() => void list.refetch()} />
      ) : (
        <DataTable
          id="upstreams"
          columns={columns}
          data={list.data ? items : undefined}
          getRowId={(u) => u.id}
          onRowClick={(u) => drawer.open(u.id)}
          selectedId={drawer.id}
          loadingRows={5}
          columnPicker
          toolbar={
            <FilterBar>
              <FilterSearch value={q} onChange={(v) => setSearch({ q: v }, { replace: true })} placeholder={t("upstreams.filter")} />
              <FilterToggle pressed={attention} onChange={(v) => setSearch({ attention: v || undefined })}>
                {t("upstreams.onlyAttention")}
              </FilterToggle>
              <ClearFilters count={(q ? 1 : 0) + (attention ? 1 : 0)} onClear={() => setSearch({ q: undefined, attention: undefined })} />
            </FilterBar>
          }
          empty={
            filtered ? (
              <EmptyState compact title={t("upstreams.noMatch")} hint={t("upstreams.noMatchHint")} />
            ) : (
              <EmptyState
                icon={Server}
                title={t("upstreams.empty")}
                hint={t("upstreams.emptyHint")}
                action={
                  <Button variant="primary" onClick={() => void navigate({ to: "/upstreams/new" })}>
                    <Plus />
                    {t("upstreams.add")}
                  </Button>
                }
              />
            )
          }
          footer={list.data && t("upstreams.footer", { n: items.length })}
        />
      )}
      {drawer.id && <UpstreamDrawer id={drawer.id} onClose={drawer.close} />}
    </>
  );
}

