import { useQuery } from "@tanstack/react-query";
import { ClientName, useClientLabel } from "@/components/client-name";
import { ClearFilters, FilterBar, FilterSearch, FilterSelect, FilterToggle } from "@/components/filter-bar";
import { api, unwrap } from "@/lib/api/client";
import type { Cause, Tier, TimeRange } from "@/lib/api/types";
import { countActiveFilters, type LogFilters } from "@/lib/url-filters";
import { rangeStart, TIME_RANGES } from "@/lib/time-range";
import { useI18n } from "@/i18n";
import { useUpstreams } from "@/lib/api/queries";

const TIERS: Tier[] = ["passthrough", "patch", "convert"];
const CAUSES: Cause[] = ["rate_limit", "quota", "transient", "auth_expired", "banned", "content", "request", "unknown"];
const CUSTOM = "custom";

/** Which time range a stored `from` matches (within five minutes), or "custom". */
function rangeOf(from: string | undefined, to: string | undefined): TimeRange | typeof CUSTOM | undefined {
  if (!from) return undefined;
  if (to) return CUSTOM;
  const age = Date.now() - Date.parse(from);
  return TIME_RANGES.find((r) => Math.abs(Date.now() - Date.parse(rangeStart(r)) - age) < 5 * 60_000) ?? CUSTOM;
}

/** The request log's filters as chips. Every value lives in the URL. */
export function LogFilterBar({
  filters,
  onChange,
  admin,
  defaultRange,
}: {
  filters: LogFilters;
  onChange: (patch: Partial<Record<keyof LogFilters, string | boolean | undefined>>) => void;
  admin: boolean;
  /** What the list covers without a time filter, when the API limits it (the session view). */
  defaultRange?: TimeRange;
}) {
  const { t, locale } = useI18n();
  const clientLabel = useClientLabel();
  const keys = useQuery({ queryKey: ["keys"], queryFn: () => unwrap(api.GET("/keys")) });
  const models = useQuery({ queryKey: ["catalog"], queryFn: () => unwrap(api.GET("/catalog")) });
  const upstreams = useUpstreams({ enabled: admin });
  const clients = useQuery({ queryKey: ["clients"], queryFn: () => unwrap(api.GET("/clients")), staleTime: Infinity });
  const range = rangeOf(filters.from, filters.to);
  const fmt = (iso: string) =>
    new Date(iso).toLocaleString(locale, { month: "numeric", day: "numeric", hour: "2-digit", minute: "2-digit", hour12: false });
  const customLabel = filters.from ? (filters.to ? `${fmt(filters.from)} – ${fmt(filters.to)}` : t("logs.since", { when: fmt(filters.from) })) : "";
  const failedOnly = filters.status === "error";
  const active = countActiveFilters(filters) + (filters.from ? 1 : 0);

  return (
    <FilterBar>
      <FilterSearch
        value={filters.q}
        onChange={(v) => onChange({ q: v })}
        placeholder={t("logs.searchPlaceholder")}
        className="max-sm:w-full"
      />
      <FilterSelect<string>
        label={t("logs.filterTime")}
        allLabel={defaultRange ? t("logs.defaultRange", { range: t(`range.last.${defaultRange}`) }) : t("logs.anyTime")}
        value={range === defaultRange ? undefined : range}
        onChange={(v) => {
          if (v === undefined) onChange({ from: undefined, to: undefined });
          else if (v !== CUSTOM) onChange({ from: rangeStart(v as TimeRange), to: undefined });
        }}
        options={[
          ...TIME_RANGES.filter((r) => r !== defaultRange).map((r) => ({ value: r, label: t(`range.last.${r}`) })),
          ...(range === CUSTOM ? [{ value: CUSTOM, label: customLabel }] : []),
        ]}
      />
      <FilterSelect
        label={t("logs.filterKey")}
        allLabel={t("logs.allKeys")}
        value={filters.key_id}
        onChange={(v) => onChange({ key_id: v })}
        options={(keys.data?.items ?? []).map((k) => ({ value: k.id, label: k.name, search: k.name }))}
      />
      <FilterSelect
        label={t("logs.filterClient")}
        allLabel={t("logs.allClients")}
        value={filters.client}
        onChange={(v) => onChange({ client: v })}
        options={(clients.data?.items ?? []).map((c) => ({
          value: c.id,
          label: <ClientName client={c} />,
          search: clientLabel(c),
        }))}
      />
      <FilterSelect
        label={t("logs.filterModel")}
        allLabel={t("logs.allModels")}
        value={filters.model}
        onChange={(v) => onChange({ model: v })}
        options={(models.data?.items ?? []).map((m) => ({ value: m.model, label: m.model }))}
      />
      {admin && (
        <FilterSelect
          label={t("logs.filterUpstream")}
          allLabel={t("logs.allUpstreams")}
          value={filters.upstream_id}
          onChange={(v) => onChange({ upstream_id: v })}
          options={(upstreams.data?.items ?? []).map((u) => ({ value: u.id, label: u.name, search: u.name }))}
        />
      )}
      <FilterSelect<Tier>
        label={t("logs.filterTier")}
        allLabel={t("logs.allTiers")}
        value={filters.tier}
        onChange={(v) => onChange({ tier: v })}
        options={TIERS.map((x) => ({ value: x, label: t(`tier.${x}`) }))}
      />
      <FilterToggle pressed={failedOnly} onChange={(v) => onChange({ status: v ? "error" : undefined, cause: v ? filters.cause : undefined })}>
        {t("logs.onlyFailed")}
      </FilterToggle>
      {failedOnly && (
        <FilterSelect<Cause>
          label={t("logs.filterCause")}
          allLabel={t("logs.allCauses")}
          value={filters.cause}
          onChange={(v) => onChange({ cause: v })}
          options={CAUSES.map((x) => ({ value: x, label: t(`cause.${x}`) }))}
        />
      )}
      <FilterToggle pressed={Boolean(filters.retried)} onChange={(v) => onChange({ retried: v || undefined })}>
        {t("logs.onlyRetried")}
      </FilterToggle>
      {filters.session_id && (
        <FilterToggle pressed onChange={() => onChange({ session_id: undefined })}>
          {t("logs.oneSession")}
        </FilterToggle>
      )}
      {filters.user_id && (
        <FilterToggle pressed onChange={() => onChange({ user_id: undefined })}>
          {t("logs.oneUser")}
        </FilterToggle>
      )}
      {filters.status === "ok" && (
        <FilterToggle pressed onChange={() => onChange({ status: undefined })}>
          {t("logs.onlyOk")}
        </FilterToggle>
      )}
      <ClearFilters
        count={active}
        onClear={() =>
          onChange({
            from: undefined,
            to: undefined,
            key_id: undefined,
            user_id: undefined,
            model: undefined,
            upstream_id: undefined,
            client: undefined,
            status: undefined,
            tier: undefined,
            cause: undefined,
            retried: undefined,
            session_id: undefined,
            q: undefined,
          })
        }
      />
    </FilterBar>
  );
}
