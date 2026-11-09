import type { Route } from "@/lib/api/types";
import { cn } from "@/lib/cn";
import { formatPercent } from "@/lib/format";
import { useI18n } from "@/i18n";

interface RouteShare<R extends Route = Route> {
  route: R;
  /** 0 = tried first; higher tiers are fallbacks in order. */
  tier: number;
  /** Share of new sessions within its tier, 0..1. */
  share: number;
}

/**
 * Routes in the order the router tries them: higher priority first, and within one priority
 * new sessions split by weight.
 */
export function routeShares<R extends Route>(routes: readonly R[]): RouteShare<R>[] {
  const prios = [...new Set(routes.map((r) => r.priority))].sort((a, b) => b - a);
  return [...routes]
    .map((route) => {
      const tier = prios.indexOf(route.priority);
      const peers = routes.filter((r) => r.priority === route.priority);
      const sum = peers.reduce((s, r) => s + Math.max(0, r.weight), 0);
      const share = sum > 0 ? Math.max(0, route.weight) / sum : 1 / peers.length;
      return { route, tier, share };
    })
    .sort((a, b) => a.tier - b.tier || b.share - a.share);
}

const TIER_BAR = ["bg-accent", "bg-slate", "bg-ink-3/60"];

/** "首选 · 75%" with a bar: how likely a new session lands on this route. */
export function RouteBar({ tier, share, className }: { tier: number; share: number; className?: string }) {
  const { t, locale } = useI18n();
  const label = tier === 0 ? t("route.first") : t("route.fallback", { n: tier });
  return (
    <div className={cn("flex min-w-0 items-center gap-2", className)}>
      <div className="h-1.5 w-16 shrink-0 overflow-hidden rounded-full bg-surface-3" aria-hidden>
        <div className={cn("h-full rounded-full", TIER_BAR[Math.min(tier, 2)])} style={{ width: `${Math.max(4, share * 100)}%` }} />
      </div>
      <span className="whitespace-nowrap text-xs tabular-nums text-ink-2">
        {label} · {formatPercent(share, locale, 0)}
      </span>
    </div>
  );
}
