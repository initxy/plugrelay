import type { HTMLAttributes, ReactNode } from "react";
import { cn } from "@/lib/cn";

/**
 * Semantic tones, used the same way everywhere:
 * ok = working · warn = degraded or needs a look · bad = failing · info = neutral news ·
 * violet = held for a person (quarantine, approval) · brand = selected or new · neutral = off or unknown.
 */
export type Tone = "neutral" | "brand" | "ok" | "warn" | "bad" | "info" | "violet";

const DOT: Record<Tone, string> = {
  neutral: "bg-ink-3",
  brand: "bg-accent",
  ok: "bg-ok",
  warn: "bg-warn",
  bad: "bg-bad",
  info: "bg-info",
  violet: "bg-violet",
};

const PILL: Record<Tone, string> = {
  neutral: "bg-surface-2 text-ink-2",
  brand: "bg-accent-soft text-accent-strong",
  ok: "bg-ok-soft text-ok",
  warn: "bg-warn-soft text-warn",
  bad: "bg-bad-soft text-bad",
  info: "bg-info-soft text-info",
  violet: "bg-violet-soft text-violet",
};

/**
 * A colored dot. `pulse` marks something live (in flight, being retried). With `label` the dot is
 * announced; without it, it is decoration next to text that already says the state.
 */
export function StatusDot({ tone, pulse, label, className }: { tone: Tone; pulse?: boolean; label?: string; className?: string }) {
  return (
    <span
      role={label ? "img" : undefined}
      aria-label={label}
      aria-hidden={label ? undefined : true}
      className={cn("relative inline-flex size-2 shrink-0", className)}
    >
      {pulse && <span className={cn("absolute inset-0 rounded-full opacity-60 motion-safe:animate-ping", DOT[tone])} />}
      <span className={cn("relative inline-block size-2 rounded-full", DOT[tone])} />
    </span>
  );
}

/** A rounded state label: "运行中", "冷却中", "原样转发". `dot` puts a status dot in front. */
export function Pill({
  tone = "neutral",
  dot,
  icon,
  className,
  children,
  ...rest
}: HTMLAttributes<HTMLSpanElement> & { tone?: Tone; dot?: boolean; icon?: ReactNode }) {
  return (
    <span
      className={cn(
        "inline-flex h-5 items-center gap-1.5 whitespace-nowrap rounded-full px-2 text-xs font-medium leading-none [&_svg]:size-3",
        PILL[tone],
        className,
      )}
      {...rest}
    >
      {dot && <span aria-hidden className={cn("size-1.5 rounded-full", DOT[tone])} />}
      {icon}
      {children}
    </span>
  );
}

/** A dot followed by its text, for table cells: "● 正常". */
export function StatusText({ tone, pulse, children, className }: { tone: Tone; pulse?: boolean; children: ReactNode; className?: string }) {
  return (
    <span className={cn("inline-flex items-center gap-2 whitespace-nowrap", className)}>
      <StatusDot tone={tone} pulse={pulse} />
      <span className={tone === "neutral" ? "text-ink-2" : undefined}>{children}</span>
    </span>
  );
}
