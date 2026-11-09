import { useRef, type KeyboardEvent } from "react";
import { cn } from "@/lib/cn";
import type { Option } from "./select";

/** A small set of mutually exclusive choices shown inline. Arrow keys move the choice, like native radios. */
export function Segmented<V extends string>({
  value,
  onChange,
  options,
  size = "md",
  className,
  "aria-label": ariaLabel,
}: {
  value: V;
  onChange: (v: V) => void;
  options: readonly Option<V>[];
  size?: "sm" | "md";
  className?: string;
  "aria-label"?: string;
}) {
  const ref = useRef<HTMLDivElement>(null);
  const onKeyDown = (e: KeyboardEvent) => {
    const step = e.key === "ArrowRight" || e.key === "ArrowDown" ? 1 : e.key === "ArrowLeft" || e.key === "ArrowUp" ? -1 : 0;
    if (!step) return;
    e.preventDefault();
    const i = options.findIndex((o) => o.value === value);
    const next = options[(i + step + options.length) % options.length]!;
    onChange(next.value);
    ref.current?.querySelector<HTMLButtonElement>(`[data-value="${CSS.escape(next.value)}"]`)?.focus();
  };
  return (
    <div
      ref={ref}
      role="radiogroup"
      aria-label={ariaLabel}
      onKeyDown={onKeyDown}
      className={cn("inline-flex w-fit shrink-0 self-start rounded-md border border-line bg-surface p-0.5 shadow-xs", className)}
    >
      {options.map((o) => {
        const on = o.value === value;
        return (
          <button
            key={o.value}
            type="button"
            role="radio"
            aria-checked={on}
            data-value={o.value}
            tabIndex={on ? 0 : -1}
            onClick={() => onChange(o.value)}
            className={cn(
              "inline-flex items-center gap-1.5 whitespace-nowrap rounded-[5px] px-2.5 text-ink-2 transition-colors duration-100 hover:text-ink focus-visible:outline-offset-0 [&_svg]:size-3.5",
              size === "sm" ? "h-6 text-xs" : "h-7 text-xs",
              on && "bg-accent-soft font-semibold text-accent-strong hover:text-accent-strong",
            )}
          >
            {o.label}
          </button>
        );
      })}
    </div>
  );
}
