import { Bar, BarChart, CartesianGrid, ResponsiveContainer, Tooltip, XAxis, YAxis } from "recharts";
import type { Timeseries } from "@/lib/api/types";
import { formatCompact } from "@/lib/format";
import { formatMoney } from "@/lib/money";

const COLORS = ["var(--chart-1)", "var(--chart-2)", "var(--chart-3)", "var(--chart-4)", "var(--chart-5)"];
const MAX_SERIES = 4;

/** Stacked bars of one metric over time, top series plus "other". Loaded lazily with Recharts. */
export default function UsageChart({
  data,
  locale,
  otherLabel,
  height = 240,
  colors,
}: {
  data: Timeseries;
  locale: string;
  otherLabel: string;
  height?: number;
  /** Fixed colors by series key; the rest take the chart palette. */
  colors?: Record<string, string>;
}) {
  const top = data.series.slice(0, MAX_SERIES);
  const rest = data.series.slice(MAX_SERIES);
  const keys = [...top.map((s) => s.key), ...(rest.length ? ["__other"] : [])];
  const labels: Record<string, string> = Object.fromEntries([...top.map((s) => [s.key, s.label]), ["__other", otherLabel]]);
  const len = data.series[0]?.points.length ?? 0;
  const rows = Array.from({ length: len }, (_, i) => {
    const row: Record<string, number | string> = { t: data.series[0]!.points[i]!.t };
    for (const s of top) row[s.key] = s.points[i]?.v ?? 0;
    if (rest.length) row.__other = rest.reduce((sum, s) => sum + (s.points[i]?.v ?? 0), 0);
    return row;
  });
  const color = (k: string, i: number) => colors?.[k] ?? COLORS[i % COLORS.length];
  const isMoney = Boolean(data.currency);
  const fmt = (v: number) => (isMoney ? formatMoney({ micro: v, currency: data.currency! }, locale, { compact: true }) : formatCompact(v, locale));
  const tick = (iso: string) => {
    const d = new Date(iso);
    if (data.bucket_seconds >= 86_400) return d.toLocaleDateString(locale, { month: "short", day: "numeric" });
    if (data.bucket_seconds >= 6 * 3600)
      return d.toLocaleString(locale, { month: "numeric", day: "numeric", hour: "2-digit", minute: "2-digit", hour12: false });
    return d.toLocaleTimeString(locale, { hour: "2-digit", minute: "2-digit", hour12: false });
  };

  return (
    <div>
      <div style={{ height }}>
        <ResponsiveContainer width="100%" height="100%">
          <BarChart data={rows} margin={{ top: 4, right: 4, bottom: 0, left: 4 }} barCategoryGap="18%">
            <CartesianGrid vertical={false} stroke="var(--line)" />
            <XAxis dataKey="t" tickFormatter={tick} tick={{ fill: "var(--ink-3)", fontSize: 11 }} axisLine={false} tickLine={false} minTickGap={24} />
            <YAxis tickFormatter={fmt} tick={{ fill: "var(--ink-3)", fontSize: 11 }} axisLine={false} tickLine={false} width={56} />
            <Tooltip
              cursor={{ fill: "var(--surface-2)" }}
              contentStyle={{ background: "var(--surface)", border: "1px solid var(--line)", borderRadius: 6, fontSize: 12 }}
              labelStyle={{ color: "var(--ink-2)" }}
              labelFormatter={(l) => new Date(String(l)).toLocaleString(locale, { hour12: false })}
              formatter={(v, name) => [fmt(Number(v)), labels[String(name)] ?? String(name)]}
            />
            {keys.map((k, i) => (
              <Bar key={k} dataKey={k} stackId="s" fill={color(k, i)} radius={i === keys.length - 1 ? [2, 2, 0, 0] : 0} isAnimationActive={false} />
            ))}
          </BarChart>
        </ResponsiveContainer>
      </div>
      <ul className="mt-2 flex flex-wrap gap-x-4 gap-y-1 text-xs text-ink-2">
        {keys.map((k, i) => (
          <li key={k} className="flex items-center gap-1.5">
            <span className="size-2 rounded-[2px]" style={{ background: color(k, i) }} />
            {labels[k]}
          </li>
        ))}
      </ul>
    </div>
  );
}
