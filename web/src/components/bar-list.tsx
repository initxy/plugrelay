import type { ReactNode } from "react";
import { cn } from "@/lib/cn";

export interface BarItem {
  key: string;
  label: ReactNode;
  value: number;
  /** The value as shown at the end of the row. */
  display: ReactNode;
  color?: string;
  onClick?: () => void;
}

/** Ranked rows with a proportional bar each: a name, the bar, the amount. */
export function BarList({ items, className }: { items: BarItem[]; className?: string }) {
  const max = Math.max(0, ...items.map((i) => i.value));
  return (
    <ul className={cn("grid gap-2", className)}>
      {items.map((i) => {
        const body = (
          <>
            <span className="min-w-0 truncate text-sm">{i.label}</span>
            <span className="h-2 overflow-hidden rounded-full bg-surface-2">
              <span
                className="block h-full rounded-full"
                style={{ width: `${max > 0 ? Math.max(2, (i.value / max) * 100) : 0}%`, background: i.color ?? "var(--chart-1)" }}
              />
            </span>
            <span className="text-right text-sm tabular-nums">{i.display}</span>
          </>
        );
        const grid = "grid grid-cols-[minmax(96px,9rem)_minmax(0,1fr)_minmax(64px,auto)] items-center gap-3";
        return (
          <li key={i.key}>
            {i.onClick ? (
              <button type="button" onClick={i.onClick} className={cn(grid, "-mx-1.5 w-[calc(100%+0.75rem)] rounded-md px-1.5 py-0.5 text-left hover:bg-surface-2/70")}>
                {body}
              </button>
            ) : (
              <div className={grid}>{body}</div>
            )}
          </li>
        );
      })}
    </ul>
  );
}
