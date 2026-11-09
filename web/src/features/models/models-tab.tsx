import { createColumnHelper } from "@tanstack/react-table";
import { Boxes } from "lucide-react";
import { useMemo } from "react";
import { DataTable } from "@/components/data-table";
import { ClearFilters, FilterBar, FilterSearch, FilterSelect } from "@/components/filter-bar";
import { EmptyState, ErrorState } from "@/components/page";
import { Pill, StatusText, type Tone } from "@/components/status";
import type { Model, PricingRule, Upstream } from "@/lib/api/types";
import { formatCompact, formatRelative } from "@/lib/format";
import { formatPrice } from "@/lib/money";
import { pickEnum, pickString } from "@/lib/url-filters";
import { useDrawerParam, useUrlSearch } from "@/lib/use-url-search";
import { useI18n } from "@/i18n";
import { priceFor } from "./lookup";
import { ModelDrawer } from "./model-drawer";
import { useGroups, useModels, usePricing } from "./queries";
import { RouteBar, routeShares } from "./route-bars";
import { useUpstreams } from "@/lib/api/queries";

const STATES = ["on", "off", "noroutes"] as const;

export function modelState(m: Model): { key: "on" | "manual" | "upstream_gone" | "noroutes"; tone: Tone } {
  if (!m.enabled) return { key: m.disabled_reason === "upstream_gone" ? "upstream_gone" : "manual", tone: "neutral" };
  if (m.routes.length === 0) return { key: "noroutes", tone: "warn" };
  return { key: "on", tone: "ok" };
}

export function useModelStateText() {
  const { t } = useI18n();
  return (m: Model) => t(`models.state.${modelState(m).key}`);
}

/** "$3.75 · $18.75": input and output per million tokens, or a dash. */
function PricePair({ rule }: { rule: PricingRule | undefined }) {
  const { locale } = useI18n();
  if (!rule) return <span className="text-ink-3">—</span>;
  const p = rule.prices;
  return (
    <span className="whitespace-nowrap tabular-nums">
      {formatPrice(p.input, p.currency, locale)}
      <span className="mx-1 text-ink-3">/</span>
      {formatPrice(p.output, p.currency, locale)}
    </span>
  );
}

function RouteList({ m, upstreams, groups, max = 3 }: { m: Model; upstreams: Map<string, Upstream>; groups: Map<string, string>; max?: number }) {
  const { t } = useI18n();
  if (m.routes.length === 0) return <span className="text-xs text-warn">{t("models.noRoutes")}</span>;
  const shares = routeShares(m.routes);
  return (
    <ul className="grid gap-1 py-1.5">
      {shares.slice(0, max).map(({ route: r, tier, share }, i) => {
        const up = upstreams.get(r.upstream_id);
        return (
          <li key={i} className="grid grid-cols-[minmax(96px,170px)_auto_auto] items-center gap-x-3">
            {/* Names as configured: the upstream's and the model id it is called by there. */}
            <span data-raw className="min-w-0 truncate text-[13px]" title={r.upstream_model}>
              {up?.name ?? r.upstream_id}
              {r.upstream_model !== m.name && <span className="ml-1 font-mono text-xs text-ink-3">→ {r.upstream_model}</span>}
            </span>
            <RouteBar tier={tier} share={share} />
            {r.groups?.length ? (
              <Pill tone="info" className="max-w-40 justify-self-start">
                <span className="truncate">{t("route.groupsNamed", { names: r.groups.map((g) => groups.get(g) ?? g).join("、") })}</span>
              </Pill>
            ) : (
              <span />
            )}
          </li>
        );
      })}
      {shares.length > max && <li className="text-xs text-ink-3">{t("models.moreRoutes", { n: shares.length - max })}</li>}
    </ul>
  );
}

const col = createColumnHelper<Model>();

export function ModelsTab() {
  const { t, locale } = useI18n();
  const [search, setSearch] = useUrlSearch();
  const drawer = useDrawerParam();
  const q = pickString(search.q);
  const upstream = pickString(search.upstream);
  const state = pickEnum(search.state, STATES);
  const models = useModels();
  const pricing = usePricing();
  const ups = useUpstreams();
  const stateText = useModelStateText();
  const upstreams = useMemo(() => new Map((ups.data?.items ?? []).map((u) => [u.id, u])), [ups.data]);
  const groups = useGroups();
  const groupNames = useMemo(() => new Map((groups.data?.items ?? []).map((g) => [g.id, g.name])), [groups.data]);
  const rules = useMemo(() => pricing.data?.items ?? [], [pricing.data]);

  const items = useMemo(() => {
    const f = q?.toLowerCase();
    return (models.data?.items ?? []).filter((m) => {
      if (f && !m.name.toLowerCase().includes(f) && !(m.info?.display_name ?? "").toLowerCase().includes(f)) return false;
      if (upstream && !m.routes.some((r) => r.upstream_id === upstream)) return false;
      const s = modelState(m).key;
      if (state === "on" && s !== "on") return false;
      if (state === "off" && m.enabled) return false;
      if (state === "noroutes" && s !== "noroutes") return false;
      return true;
    });
  }, [models.data, q, upstream, state]);

  const columns = useMemo(
    () => [
      col.accessor("name", {
        header: t("models.col.model"),
        enableHiding: false,
        cell: ({ row: { original: m } }) => (
          <div className="min-w-0 py-1.5">
            <div className="truncate font-mono text-[13px] font-medium">{m.name}</div>
            <div className="truncate text-xs text-ink-3">
              {[m.info?.display_name !== m.name ? m.info?.display_name : undefined, m.info?.context_window ? t("models.context", { n: formatCompact(m.info.context_window, locale) }) : undefined]
                .filter(Boolean)
                .join(" · ")}
            </div>
          </div>
        ),
      }),
      col.display({
        id: "routes",
        header: t("models.col.routes"),
        enableHiding: false,
        cell: ({ row: { original: m } }) => <RouteList m={m} upstreams={upstreams} groups={groupNames} />,
      }),
      col.display({
        id: "price",
        header: t("models.col.price"),
        meta: { className: "hidden lg:table-cell", label: t("models.col.price") },
        cell: ({ row: { original: m } }) => <PricePair rule={priceFor(rules, m.name)} />,
      }),
      col.display({
        id: "state",
        header: t("models.col.state"),
        cell: ({ row: { original: m } }) => <StatusText tone={modelState(m).tone}>{stateText(m)}</StatusText>,
      }),
      col.accessor("synced_at", {
        header: t("models.col.synced"),
        meta: { className: "hidden xl:table-cell text-ink-2", label: t("models.col.synced") },
        cell: ({ getValue }) => {
          const v = getValue();
          return v ? <span title={v}>{formatRelative(v, locale)}</span> : <span className="text-ink-3">{t("models.byHand")}</span>;
        },
      }),
    ],
    [t, locale, upstreams, groupNames, rules, stateText],
  );

  const filtered = Boolean(q || upstream || state);
  return (
    <>
      {models.isError ? (
        <ErrorState error={models.error} onRetry={() => void models.refetch()} />
      ) : (
        <DataTable
          id="models"
          columns={columns}
          data={models.data ? items : undefined}
          getRowId={(m) => m.id}
          onRowClick={(m) => drawer.open(m.id)}
          selectedId={drawer.id}
          columnPicker
          toolbar={
            <FilterBar>
              <FilterSearch value={q} onChange={(v) => setSearch({ q: v }, { replace: true })} placeholder={t("models.filter")} />
              <FilterSelect
                label={t("models.col.upstream")}
                allLabel={t("models.allUpstreams")}
                value={upstream}
                onChange={(v) => setSearch({ upstream: v })}
                options={(ups.data?.items ?? []).map((u) => ({ value: u.id, label: u.name, search: u.name }))}
              />
              <FilterSelect
                label={t("models.col.state")}
                allLabel={t("models.anyState")}
                value={state}
                onChange={(v) => setSearch({ state: v })}
                options={STATES.map((s) => ({ value: s, label: t(`models.filterState.${s}`) }))}
              />
              <ClearFilters count={[q, upstream, state].filter(Boolean).length} onClear={() => setSearch({ q: undefined, upstream: undefined, state: undefined })} />
            </FilterBar>
          }
          empty={
            filtered ? (
              <EmptyState compact title={t("models.noMatch")} />
            ) : (
              <EmptyState icon={Boxes} title={t("models.empty")} hint={t("models.emptyHint")} />
            )
          }
          footer={models.data && t("models.footer", { n: items.length })}
        />
      )}
      {drawer.id && <ModelDrawer id={drawer.id} onClose={drawer.close} />}
    </>
  );
}

