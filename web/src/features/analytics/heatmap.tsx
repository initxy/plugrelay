import { useQuery } from "@tanstack/react-query";
import { CalendarClock } from "lucide-react";
import { useState } from "react";
import { useMoney } from "@/components/money";
import { EmptyState, ErrorState, Panel } from "@/components/page";
import { Segmented } from "@/components/ui/segmented";
import { Skeleton } from "@/components/ui/skeleton";
import { useTimeRange } from "@/app/time-range";
import { api, unwrap } from "@/lib/api/client";
import type { TimeRange } from "@/lib/api/types";
import { formatNumber } from "@/lib/format";
import { useI18n } from "@/i18n";

type Metric = "requests" | "spend";
const HOURS = Array.from({ length: 24 }, (_, h) => h);
/** Monday first; JavaScript counts from Sunday. */
const DAYS = [1, 2, 3, 4, 5, 6, 0];

/** Hourly points summed into weekday × hour cells of the viewer's local time. */
export function heatGrid(points: readonly { t: string; v: number }[]): number[][] {
  const grid = Array.from({ length: 7 }, () => Array<number>(24).fill(0));
  for (const p of points) {
    const d = new Date(p.t);
    grid[d.getDay()]![d.getHours()]! += p.v;
  }
  return grid;
}

function weekdayName(day: number, locale: string): string {
  // 2024-01-07 was a Sunday.
  return new Date(2024, 0, 7 + day).toLocaleDateString(locale, { weekday: "short" });
}

/** A one-hour range has no week to show; the heatmap reads at least the last 7 days. */
function heatRange(range: TimeRange): TimeRange {
  return range === "1h" || range === "24h" ? "7d" : range;
}

/** When the relay is busy: requests or spend by weekday and hour, darker is more. */
export function HeatmapPanel() {
  const { t, locale } = useI18n();
  const money = useMoney();
  const { range: pageRange } = useTimeRange();
  const range = heatRange(pageRange);
  const [metric, setMetric] = useState<Metric>("requests");
  const q = useQuery({
    queryKey: ["timeseries", range, metric, "none", "hour"],
    queryFn: () =>
      unwrap(api.GET("/usage/timeseries", { params: { query: { range, metric, group_by: "none", bucket: "hour" } } })),
    placeholderData: (p) => p,
  });
  const grid = heatGrid(q.data?.series[0]?.points ?? []);
  const max = Math.max(0, ...grid.flat());
  const fmt = (v: number) => (metric === "spend" ? money({ micro: v, currency: q.data?.currency ?? "USD" }) : formatNumber(v, locale));
  return (
    <Panel
      title={t("analytics.heatmap")}
      aside={
        <Segmented
          size="sm"
          aria-label={t("usage.metric")}
          value={metric}
          onChange={setMetric}
          options={[
            { value: "requests", label: t("usage.metricRequests") },
            { value: "spend", label: t("usage.metricSpend") },
          ]}
        />
      }
    >
      {q.isError ? (
        <ErrorState error={q.error} onRetry={() => void q.refetch()} />
      ) : !q.data ? (
        <Skeleton className="h-[188px]" />
      ) : max === 0 ? (
        <EmptyState compact icon={CalendarClock} title={t("overview.noUsage")} hint={t("overview.noUsageHint")} />
      ) : (
        <div className="grid gap-2">
          <div className="overflow-x-auto">
            <table className="w-full min-w-[520px] table-fixed border-separate border-spacing-[3px] text-xs" aria-label={t("analytics.heatmap")}>
              <thead>
                <tr>
                  <th className="w-10" />
                  {HOURS.map((h) => (
                    <th key={h} scope="col" className="h-4 text-left font-normal tabular-nums text-ink-3">
                      {h % 3 === 0 ? h : ""}
                    </th>
                  ))}
                </tr>
              </thead>
              <tbody>
                {DAYS.map((day) => (
                  <tr key={day}>
                    <th scope="row" className="w-10 whitespace-nowrap pr-1 text-left font-normal text-ink-3">
                      {weekdayName(day, locale)}
                    </th>
                    {HOURS.map((h) => {
                      const v = grid[day]![h]!;
                      const share = max > 0 ? v / max : 0;
                      const title = t("analytics.heatCell", { day: weekdayName(day, locale), hour: String(h).padStart(2, "0"), value: fmt(v) });
                      return (
                        <td
                          key={h}
                          title={title}
                          aria-label={title}
                          className="h-5 rounded-[3px]"
                          style={{
                            background:
                              v > 0
                                ? `color-mix(in oklab, var(--chart-1) ${Math.round(12 + share * 88)}%, var(--surface-2))`
                                : "var(--surface-2)",
                          }}
                        />
                      );
                    })}
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
          <div className="flex flex-wrap items-center justify-between gap-2 text-xs text-ink-3">
            <span>
              {t(`range.last.${range}`)} · {t("analytics.heatmapHint")}
            </span>
            <span className="flex items-center gap-1.5">
              {t("analytics.less")}
              {[0, 0.25, 0.5, 0.75, 1].map((s) => (
                <i
                  key={s}
                  aria-hidden
                  className="size-3 rounded-[3px]"
                  style={{ background: s === 0 ? "var(--surface-2)" : `color-mix(in oklab, var(--chart-1) ${Math.round(12 + s * 88)}%, var(--surface-2))` }}
                />
              ))}
              {t("analytics.more")}
            </span>
          </div>
        </div>
      )}
    </Panel>
  );
}
