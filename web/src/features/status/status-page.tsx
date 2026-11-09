import { useQuery, type UseQueryResult } from "@tanstack/react-query";
import { Activity, RotateCw } from "lucide-react";
import { HourlyBar } from "@/components/hourly-bar";
import { EmptyState, ErrorState, PageHeader } from "@/components/page";
import { StatTile } from "@/components/stat-tile";
import { Pill, type Tone } from "@/components/status";
import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import { api, unwrap } from "@/lib/api/client";
import { cn } from "@/lib/cn";
import { formatDuration, formatPercent, formatWhen } from "@/lib/format";
import type { StatusPage as StatusPageData } from "@/lib/api/types";
import { useI18n } from "@/i18n";

type State = "ok" | "degraded" | "down";
const TONE: Record<State, Tone> = { ok: "ok", degraded: "warn", down: "bad" };
const ORDER: Record<State, number> = { down: 0, degraded: 1, ok: 2 };

/** Whether each model the caller may use is answering, over the last 24 hours. */
export function StatusPage() {
  const { t, locale } = useI18n();
  const q = useQuery({
    queryKey: ["status"],
    queryFn: () => unwrap(api.GET("/status")),
    refetchInterval: 30_000,
  });
  return (
    <>
      <PageHeader
        title={t("nav.status")}
        description={t("status.description")}
        actions={
          <>
            {q.data && (
              <span className="text-xs text-ink-3">
                {t("status.updated", { at: formatWhen(q.data.updated_at, locale) })}
              </span>
            )}
            <Button
              size="sm"
              variant="ghost"
              onClick={() => void q.refetch()}
              disabled={q.isFetching}
              aria-label={t("status.refresh")}
            >
              <RotateCw className={cn(q.isFetching && "motion-safe:animate-spin")} />
            </Button>
          </>
        }
      />
      <StatusBoard q={q} />
    </>
  );
}

/** The status summary, counts and per-model rows; also the public status page. */
export function StatusBoard({
  q,
}: {
  q: Pick<UseQueryResult<StatusPageData>, "data" | "isPending" | "isError" | "error" | "refetch">;
}) {
  const { t, locale } = useI18n();
  const items = [...(q.data?.items ?? [])].sort(
    (a, b) => ORDER[a.state] - ORDER[b.state] || a.model.localeCompare(b.model),
  );
  const count = (s: State) => items.filter((i) => i.state === s).length;
  const bad = count("down") + count("degraded");
  const withTraffic = items.filter((i) => i.hourly.some((h) => h !== null));
  const avg = withTraffic.length
    ? withTraffic.reduce((s, i) => s + i.success_rate_24h, 0) / withTraffic.length
    : undefined;

  return (
    <>
      {q.isError ? (
        <ErrorState error={q.error} onRetry={() => void q.refetch()} />
      ) : (
        <>
          <div
            role="status"
            className={cn(
              "mb-4 flex items-center gap-3 rounded-lg border px-4 py-3",
              q.isPending
                ? "border-line bg-surface"
                : bad === 0
                  ? "border-ok/30 bg-ok-soft"
                  : count("down")
                    ? "border-bad/30 bg-bad-soft"
                    : "border-warn/30 bg-warn-soft",
            )}
          >
            {q.isPending ? (
              <Skeleton className="h-5 w-64" />
            ) : (
              <>
                <span
                  className={cn(
                    "size-2.5 rounded-full",
                    bad === 0 ? "bg-ok" : count("down") ? "bg-bad" : "bg-warn",
                  )}
                />
                <span className="text-sm font-medium">
                  {items.length === 0
                    ? t("status.none")
                    : bad === 0
                      ? t("status.allOk")
                      : t("status.someBad", { n: bad })}
                </span>
              </>
            )}
          </div>
          <div className="mb-4 grid grid-cols-2 gap-3 lg:grid-cols-4">
            <StatTile
              label={t("status.ok")}
              loading={q.isPending}
              value={count("ok")}
              note={t("status.models")}
            />
            <StatTile
              label={t("status.degraded")}
              loading={q.isPending}
              value={<span className={count("degraded") ? "text-warn" : undefined}>{count("degraded")}</span>}
              note={t("status.models")}
            />
            <StatTile
              label={t("status.down")}
              loading={q.isPending}
              value={<span className={count("down") ? "text-bad" : undefined}>{count("down")}</span>}
              note={t("status.models")}
            />
            <StatTile
              label={t("status.avg")}
              loading={q.isPending}
              value={avg === undefined ? "—" : formatPercent(avg, locale)}
              note={t("status.avgNote")}
            />
          </div>
          <section className="overflow-hidden rounded-lg border border-line bg-surface">
            <div className="hidden grid-cols-[minmax(160px,1.2fr)_110px_minmax(200px,2fr)_150px] gap-4 border-b border-line bg-surface-2/95 px-4 py-2 text-xs font-medium text-ink-3 md:grid">
              <span>{t("catalog.model")}</span>
              <span>{t("status.state")}</span>
              <span className="flex justify-between">
                <span>{t("status.last24h")}</span>
                <span>{t("status.now")}</span>
              </span>
              <span className="text-right">{t("status.rateTtfb")}</span>
            </div>
            {q.isPending ? (
              <div className="grid gap-2 p-4" aria-busy="true">
                {Array.from({ length: 5 }, (_, i) => (
                  <Skeleton key={i} className="h-6" />
                ))}
              </div>
            ) : items.length === 0 ? (
              <EmptyState compact icon={Activity} title={t("status.none")} hint={t("status.noneHint")} />
            ) : (
              <ul>
                {items.map((s) => (
                  <li
                    key={s.model}
                    className="grid items-center gap-x-4 gap-y-2 border-b border-line-soft px-4 py-3 last:border-0 md:grid-cols-[minmax(160px,1.2fr)_110px_minmax(200px,2fr)_150px]"
                  >
                    <div className="flex items-center justify-between gap-2 md:contents">
                      <span className="truncate font-mono text-[13px] font-medium">{s.model}</span>
                      <span>
                        <Pill tone={TONE[s.state]} dot>
                          {t(`status.${s.state}`)}
                        </Pill>
                      </span>
                    </div>
                    <HourlyBar hourly={s.hourly} />
                    <span className="text-xs tabular-nums text-ink-2 md:text-right">
                      {s.hourly.some((h) => h !== null)
                        ? formatPercent(s.success_rate_24h, locale)
                        : t("status.noTraffic")}
                      {s.ttfb_p50_ms ? (
                        <span className="text-ink-3">
                          {" "}
                          · {t("status.ttfb", { d: formatDuration(s.ttfb_p50_ms) })}
                        </span>
                      ) : null}
                    </span>
                  </li>
                ))}
              </ul>
            )}
          </section>
          <p className="mt-2 px-1 text-xs text-ink-3">{t("status.legend")}</p>
        </>
      )}
    </>
  );
}
