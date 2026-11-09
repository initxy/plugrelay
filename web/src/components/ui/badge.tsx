import { cva, type VariantProps } from "class-variance-authority";
import type { HTMLAttributes } from "react";
import { cn } from "@/lib/cn";

/** A small rectangular tag (protocol, source, version). For states use `Pill` from components/status. */
const badgeVariants = cva(
  "inline-flex h-5 items-center gap-1 whitespace-nowrap rounded-xs px-1.5 text-2xs font-medium leading-none [&_svg]:size-3",
  {
    variants: {
      tone: {
        neutral: "bg-surface-2 text-ink-2",
        accent: "bg-accent-soft text-accent-strong",
        ok: "bg-ok-soft text-ok",
        warn: "bg-warn-soft text-warn",
        bad: "bg-bad-soft text-bad",
        info: "bg-info-soft text-info",
        violet: "bg-violet-soft text-violet",
        outline: "border border-line-strong text-ink-2",
        dashed: "border border-dashed border-line-strong text-ink-2",
      },
    },
    defaultVariants: { tone: "neutral" },
  },
);

export function Badge({
  className,
  tone,
  ...props
}: HTMLAttributes<HTMLSpanElement> & VariantProps<typeof badgeVariants>) {
  return <span className={cn(badgeVariants({ tone }), className)} {...props} />;
}
