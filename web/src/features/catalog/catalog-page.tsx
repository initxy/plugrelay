import { useQuery } from "@tanstack/react-query";
import { createColumnHelper } from "@tanstack/react-table";
import { Brain, Eye, PackageOpen, SearchX, Wrench } from "lucide-react";
import { useMemo } from "react";
import { DataTable } from "@/components/data-table";
import { ClearFilters, FilterBar, FilterSearch, FilterSelect, FilterToggle } from "@/components/filter-bar";
import { EmptyState, ErrorState, PageHeader } from "@/components/page";
import { Pill } from "@/components/status";
import { Tooltip } from "@/components/ui/tooltip";
import { useMe } from "@/app/session";
import { api, unwrap } from "@/lib/api/client";
import type { components } from "@/lib/api/schema.gen";
import { formatCompact } from "@/lib/format";
import { formatPrice } from "@/lib/money";
import { useUrlSearch } from "@/lib/use-url-search";
import { useI18n } from "@/i18n";

type Entry = components["schemas"]["CatalogEntry"];
const BUCKETS = ["input", "cache_read", "cache_write_5m", "cache_write_1h", "output"] as const;
const col = createColumnHelper<Entry>();
const str = (v: unknown) => (typeof v === "string" && v !== "" ? v : undefined);

function Capability({ on, icon: Icon, label }: { on?: boolean; icon: typeof Brain; label: string }) {
  if (!on) return null;
  return (
    <Tooltip content={label}>
      <span
        className="grid size-5 place-items-center rounded-xs bg-surface-2 text-ink-2"
        aria-label={label}
        role="img"
      >
        <Icon className="size-3" />
      </span>
    </Tooltip>
  );
}

/** Models the caller's keys can use and what each costs per million tokens, multiplier included. */
export function CatalogPage() {
  const { t } = useI18n();
  const me = useMe();
  const catalog = useQuery({ queryKey: ["catalog"], queryFn: () => unwrap(api.GET("/catalog")) });
  return (
    <>
      <PageHeader title={t("nav.catalog")} description={t("catalog.description")} />
      {catalog.isError ? (
        <ErrorState error={catalog.error} onRetry={() => void catalog.refetch()} />
      ) : (
        <CatalogTable items={catalog.data?.items} currency={me.site.currency} />
      )}
    </>
  );
}

/** The models-and-prices table with its filters (in the URL); also the public models page. */
export function CatalogTable({
  items: loaded,
  currency: siteCurrency,
}: {
  items: Entry[] | undefined;
  currency: string;
}) {
  const { t, locale } = useI18n();
  const [search, set] = useUrlSearch();
  const q = str(search.q);
  const maker = str(search.maker);
  const reasoning = search.reasoning === true || search.reasoning === "true";
  const items = useMemo(() => loaded ?? [], [loaded]);
  const makers = [
    ...new Set(items.map((m) => m.info?.owned_by).filter((x): x is string => Boolean(x))),
  ].sort();
  const currency = items[0]?.price.currency ?? siteCurrency;

  const columns = useMemo(
    () => [
      col.accessor("model", {
        header: t("catalog.model"),
        cell: (c) => {
          const m = c.row.original;
          return (
            <div className="min-w-0 py-1">
              <div className="flex flex-wrap items-center gap-1.5">
                <span className="font-mono text-[13px] font-medium">{m.model}</span>
                {m.kind === "preset" && <Pill tone="violet">{t("catalog.preset")}</Pill>}
              </div>
              <div className="truncate text-xs text-ink-3">
                {m.kind === "preset" && m.preset_models?.length
                  ? t("catalog.presetOf", { chain: m.preset_models.join(" → ") })
                  : [m.info?.display_name, m.info?.owned_by].filter(Boolean).join(" · ")}
              </div>
            </div>
          );
        },
        meta: { className: "min-w-56" },
      }),
      col.display({
        id: "can",
        header: t("catalog.can"),
        meta: { label: t("catalog.can") },
        cell: (c) => {
          const i = c.row.original.info;
          return (
            <div className="flex gap-1">
              <Capability on={i?.reasoning} icon={Brain} label={t("catalog.reasoning")} />
              <Capability on={i?.vision} icon={Eye} label={t("catalog.vision")} />
              <Capability on={i?.tools} icon={Wrench} label={t("catalog.tools")} />
            </div>
          );
        },
      }),
      col.accessor((m) => m.info?.context_window ?? 0, {
        id: "context",
        header: t("catalog.context"),
        cell: (c) =>
          c.getValue() ? <span className="whitespace-nowrap">{formatCompact(c.getValue(), locale)}</span> : <span className="text-ink-3">—</span>,
        meta: { numeric: true },
      }),
      ...BUCKETS.map((b) =>
        col.accessor((m) => m.price[b], {
          id: b,
          header: t(`usage.${b}`),
          cell: (c) =>
            c.row.original.kind === "preset" ? null : c.getValue() ? (
              <span className={b === "input" || b === "output" ? "font-medium text-ink" : "text-ink-2"}>
                {formatPrice(c.getValue(), c.row.original.price.currency, locale)}
              </span>
            ) : (
              <span className="text-ink-3">—</span>
            ),
          meta: { numeric: true },
        }),
      ),
    ],
    [t, locale],
  );

  const shown = items.filter(
    (m) =>
      (!maker || m.info?.owned_by === maker) &&
      (!reasoning || m.info?.reasoning) &&
      (!q || `${m.model} ${m.info?.display_name ?? ""}`.toLowerCase().includes(q.toLowerCase())),
  );
  const filters = [q, maker, reasoning || undefined].filter(Boolean).length;

  return (
    <div className="grid gap-2.5">
      <DataTable
        toolbar={
          <FilterBar>
            <FilterSearch
              value={q}
              onChange={(v) => set({ q: v }, { replace: true })}
              placeholder={t("catalog.search")}
            />
            {makers.length > 1 && (
              <FilterSelect
                label={t("catalog.maker")}
                allLabel={t("catalog.allMakers")}
                value={maker}
                onChange={(v) => set({ maker: v })}
                options={makers.map((m) => ({ value: m, label: m }))}
              />
            )}
            <FilterToggle pressed={reasoning} onChange={(v) => set({ reasoning: v })}>
              {t("catalog.onlyReasoning")}
            </FilterToggle>
            <ClearFilters
              count={filters}
              onClear={() => set({ q: undefined, maker: undefined, reasoning: undefined })}
            />
          </FilterBar>
        }
        id="catalog"
        columns={columns}
        data={loaded === undefined ? undefined : shown}
        getRowId={(m) => m.model}
        columnPicker
        maxHeight="none"
        empty={
          items.length === 0 ? (
            <EmptyState icon={PackageOpen} title={t("catalog.empty")} hint={t("catalog.emptyHint")} />
          ) : (
            <EmptyState compact icon={SearchX} title={t("catalog.noMatch")} />
          )
        }
        footer={items.length > 0 ? t("catalog.unit", { currency }) : undefined}
      />
    </div>
  );
}
