import { ArrowDownRight, ArrowRight, ArrowUpRight } from "lucide-react";
import { useId, type ReactNode } from "react";
import { Skeleton } from "@/components/ui/skeleton";
import { cn } from "@/lib/cn";
import { formatPercent } from "@/lib/format";
import { useI18n } from "@/i18n";

export type SparkTone = "brand" | "ok" | "warn" | "bad" | "violet" | "amber";

const STROKE: Record<SparkTone, string> = {
  brand: "var(--accent)",
  ok: "var(--ok)",
  warn: "var(--warn)",
  bad: "var(--bad)",
  violet: "var(--violet)",
  amber: "var(--amber)",
};

/** A tiny trend line with a soft fill and a dot on the latest point. Decorative; the tile states the numbers. */
function Sparkline({
  points,
  tone = "brand",
  width = 96,
  height = 32,
  className,
}: {
  points: readonly number[];
  tone?: SparkTone;
  width?: number;
  height?: number;
  className?: string;
}) {
  const gradient = useId();
  if (points.length < 2) return <svg width={width} height={height} aria-hidden className={className} />;
  const min = Math.min(...points);
  const max = Math.max(...points);
  const pad = 3;
  const span = max - min || 1;
  const xy = points.map((p, i) => {
    const x = (i / (points.length - 1)) * (width - pad * 2) + pad;
    const y = max === min ? height / 2 : height - pad - ((p - min) / span) * (height - pad * 2);
    return [x, y] as const;
  });
  const line = xy.map(([x, y]) => `${x.toFixed(1)},${y.toFixed(1)}`).join(" ");
  const [lx, ly] = xy[xy.length - 1]!;
  const color = STROKE[tone];
  return (
    <svg width={width} height={height} viewBox={`0 0 ${width} ${height}`} aria-hidden className={cn("shrink-0 overflow-visible", className)}>
      <defs>
        <linearGradient id={gradient} x1="0" x2="0" y1="0" y2="1">
          <stop offset="0%" stopColor={color} stopOpacity="0.18" />
          <stop offset="100%" stopColor={color} stopOpacity="0" />
        </linearGradient>
      </defs>
      <polygon points={`${xy[0]![0]},${height} ${line} ${lx},${height}`} fill={`url(#${gradient})`} />
      <polyline points={line} fill="none" stroke={color} strokeWidth="1.6" strokeLinejoin="round" strokeLinecap="round" />
      <circle cx={lx} cy={ly} r="2.4" fill={color} stroke="var(--surface)" strokeWidth="1.2" />
    </svg>
  );
}

export interface Delta {
  /** Change against the previous period: 0.06 = +6 %. */
  ratio: number;
  /** Which direction is good news: spend going up is not, success rate going up is. */
  goodWhen?: "up" | "down";
  /** What it compares with: "vs yesterday". */
  label?: string;
}

/** Change between two periods; undefined when there is nothing to compare with. */
export function deltaOf(current: number, previous: number | undefined): number | undefined {
  if (previous === undefined || previous === 0 || !Number.isFinite(previous)) return undefined;
  return (current - previous) / Math.abs(previous);
}

function DeltaText({ delta }: { delta: Delta }) {
  const { locale } = useI18n();
  const flat = Math.abs(delta.ratio) < 0.005;
  const up = delta.ratio > 0;
  const good = flat ? undefined : (delta.goodWhen ?? "up") === (up ? "up" : "down");
  const Icon = flat ? ArrowRight : up ? ArrowUpRight : ArrowDownRight;
  const pct = formatPercent(Math.abs(delta.ratio), locale, Math.abs(delta.ratio) < 0.1 ? 1 : 0);
  return (
    <span className={cn("inline-flex min-w-0 items-center gap-0.5 whitespace-nowrap font-medium", good === undefined ? "text-ink-3" : good ? "text-ok" : "text-bad")}>
      <Icon className="size-3.5" aria-hidden />
      <span>
        {flat ? "0%" : `${up ? "+" : "−"}${pct}`}
      </span>
      {delta.label && <span className="ml-1 truncate font-normal text-ink-3">{delta.label}</span>}
    </span>
  );
}

/**
 * One headline number: label, value, how it moved, and its recent trend. With `onClick` the
 * whole tile is a button (typically: open the requests behind the number).
 */
export function StatTile({
  label,
  value,
  delta,
  note,
  spark,
  sparkTone = "brand",
  loading,
  onClick,
  className,
}: {
  label: ReactNode;
  value: ReactNode;
  delta?: Delta;
  /** A second line when there is no delta, or after it: "97.4% succeeded". */
  note?: ReactNode;
  spark?: readonly number[];
  sparkTone?: SparkTone;
  loading?: boolean;
  onClick?: () => void;
  className?: string;
}) {
  const body = (
    <>
      <div className="truncate text-xs text-ink-3">{label}</div>
      <div className="mt-1 flex items-end justify-between gap-3">
        {loading ? (
          <Skeleton className="my-1 h-6 w-24" />
        ) : (
          <div className="shrink-0 whitespace-nowrap text-2xl font-semibold tracking-[-0.01em] tabular-nums">{value}</div>
        )}
        {spark && !loading && (
          <div className="flex min-w-0 justify-end overflow-hidden">
            <Sparkline points={spark} tone={sparkTone} width={88} className="mb-0.5" />
          </div>
        )}
      </div>
      {loading ? (
        <Skeleton className="mt-1.5 h-3 w-20" />
      ) : (
        (delta || note) && (
          <div className="mt-1 flex min-w-0 items-center gap-x-2 text-xs text-ink-2">
            {delta && <DeltaText delta={delta} />}
            {note && <span className="truncate">{note}</span>}
          </div>
        )
      )}
    </>
  );
  const shell = "min-w-0 rounded-lg border border-line bg-surface px-4 py-3 text-left";
  if (onClick)
    return (
      <button
        type="button"
        onClick={onClick}
        className={cn(shell, "block w-full transition-[border-color,box-shadow] duration-100 hover:border-line-strong hover:shadow-card", className)}
      >
        {body}
      </button>
    );
  return <div className={cn(shell, className)}>{body}</div>;
}
