import { useQuery } from "@tanstack/react-query";
import { useState } from "react";
import { Skeleton } from "@/components/ui/skeleton";
import { api, unwrap } from "@/lib/api/client";
import type { TimeRange } from "@/lib/api/types";
import { cn } from "@/lib/cn";
import { formatNumber } from "@/lib/format";
import { rangeStart } from "@/lib/time-range";
import type { LogFilters } from "@/lib/url-filters";
import { useI18n } from "@/i18n";

const DEFAULT_SPAN_MS = 7 * 86_400_000;

function bucketLabel(seconds: number, t: ReturnType<typeof useI18n>["t"]): string {
  if (seconds % 86_400 === 0) return t("logs.hist.days", { n: seconds / 86_400 });
  if (seconds % 3600 === 0) return t("logs.hist.hours", { n: seconds / 3600 });
  return t("logs.hist.minutes", { n: Math.round(seconds / 60) });
}

/**
 * Requests per time bucket for the current filters, failures stacked in red on top. Clicking a
 * bar narrows the log to that bucket.
 */
export function RequestHistogram({
  filters,
  defaultRange,
  onPick,
}: {
  filters: LogFilters;
  /** What the bars cover without a time filter; the last week when not given. */
  defaultRange?: TimeRange;
  onPick: (from: string, to: string) => void;
}) {
  const { t, locale } = useI18n();
  // The minute is enough to keep the query key stable.
  const [fallbackFrom] = useState(() => {
    const minute = Math.floor(Date.now() / 60_000) * 60_000;
    return defaultRange ? rangeStart(defaultRange, minute) : new Date(minute - DEFAULT_SPAN_MS).toISOString();
  });
  // Every log filter except `cause`, which the histogram doesn't take.
  const { cause: _cause, ...rest } = filters;
  const query = { ...rest, from: filters.from ?? fallbackFrom };
  const q = useQuery({
    queryKey: ["logs-histogram", query],
    queryFn: () => unwrap(api.GET("/logs/histogram", { params: { query } })),
    placeholderData: (p) => p,
  });
  const h = q.data;
  const max = Math.max(1, ...(h?.points.map((p) => p.requests) ?? [1]));
  const total = h?.points.reduce((a, p) => a + p.requests, 0) ?? 0;
  const failed = h?.points.reduce((a, p) => a + p.failed, 0) ?? 0;
  const fmtTick = (iso: string) => {
    const d = new Date(iso);
    if ((h?.bucket_seconds ?? 0) >= 86_400) return d.toLocaleDateString(locale, { month: "numeric", day: "numeric" });
    if (h && Date.parse(h.to) - Date.parse(h.from) > 86_400_000)
      return d.toLocaleString(locale, { month: "numeric", day: "numeric", hour: "2-digit", minute: "2-digit", hour12: false });
    return d.toLocaleTimeString(locale, { hour: "2-digit", minute: "2-digit", hour12: false });
  };
  const ticks = h ? [0, Math.floor(h.points.length / 2), h.points.length - 1].filter((i, j, a) => i >= 0 && i < h.points.length && a.indexOf(i) === j) : [];

  return (
    <div className="rounded-lg border border-line bg-surface px-4 pb-2 pt-3">
      <div className="mb-2 flex flex-wrap items-baseline justify-between gap-x-4 gap-y-1 text-xs text-ink-3">
        <span>
          {h ? (
            <>
              <span className="font-medium text-ink-2">{t("logs.hist.total", { n: formatNumber(total, locale) })}</span>
              {failed > 0 && <span className="text-bad"> · {t("logs.hist.failed", { n: formatNumber(failed, locale) })}</span>}
              <span>
                {" · "}
                {filters.from ? t("logs.hist.perBar", { size: bucketLabel(h.bucket_seconds, t) }) : t("logs.hist.lastWeek", { size: bucketLabel(h.bucket_seconds, t) })}
              </span>
            </>
          ) : (
            <Skeleton className="h-3 w-48" />
          )}
        </span>
        <span className="flex items-center gap-3" aria-hidden>
          <span className="flex items-center gap-1">
            <i className="size-2 rounded-[2px] bg-accent/75" />
            {t("logs.hist.okLegend")}
          </span>
          <span className="flex items-center gap-1">
            <i className="size-2 rounded-[2px] bg-bad" />
            {t("logs.hist.failedLegend")}
          </span>
        </span>
      </div>
      {q.isError ? (
        <p className="py-4 text-center text-xs text-bad">{t("logs.hist.error")}</p>
      ) : !h ? (
        <Skeleton className="h-14" />
      ) : (
        <>
          <div className="flex h-14 items-end gap-[2px]" role="list" aria-label={t("logs.hist.label")}>
            {h.points.map((p) => {
              const end = new Date(Date.parse(p.t) + h.bucket_seconds * 1000).toISOString();
              const okH = ((p.requests - p.failed) / max) * 100;
              const badH = (p.failed / max) * 100;
              const label = t("logs.hist.bar", { when: fmtTick(p.t), n: p.requests, failed: p.failed });
              return (
                <button
                  key={p.t}
                  type="button"
                  role="listitem"
                  title={label}
                  aria-label={label}
                  disabled={p.requests === 0}
                  onClick={() => onPick(p.t, end)}
                  className={cn(
                    "group flex h-full min-w-0 flex-1 flex-col justify-end rounded-t-[2px] outline-none focus-visible:ring-2 focus-visible:ring-accent",
                    p.requests > 0 && "cursor-pointer",
                  )}
                >
                  {p.failed > 0 && <span className="w-full rounded-t-[2px] bg-bad group-hover:opacity-80" style={{ height: `${badH}%` }} />}
                  <span
                    className={cn(
                      "w-full bg-accent/70 group-hover:bg-accent",
                      p.failed === 0 && "rounded-t-[2px]",
                      p.requests === 0 && "h-px bg-line group-hover:bg-line",
                    )}
                    style={p.requests > 0 ? { height: `${Math.max(okH, p.requests - p.failed > 0 ? 3 : 0)}%` } : undefined}
                  />
                </button>
              );
            })}
          </div>
          <div className="relative mt-1 h-4 text-2xs tabular-nums text-ink-3" aria-hidden>
            {ticks.map((i) => (
              <span
                key={i}
                className={cn("absolute", i === 0 ? "left-0" : i === h.points.length - 1 ? "right-0" : "-translate-x-1/2")}
                style={i !== 0 && i !== h.points.length - 1 ? { left: `${((i + 0.5) / h.points.length) * 100}%` } : undefined}
              >
                {fmtTick(h.points[i]!.t)}
              </span>
            ))}
          </div>
        </>
      )}
    </div>
  );
}
