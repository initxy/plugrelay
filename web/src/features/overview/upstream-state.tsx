import { useMutation, useQueryClient } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { CircleCheck } from "lucide-react";
import { toast } from "sonner";
import { EmptyState, ErrorState, Panel } from "@/components/page";
import { StatusDot, Pill, type Tone } from "@/components/status";
import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import { api, unwrap } from "@/lib/api/client";
import type { Upstream } from "@/lib/api/types";
import { cn } from "@/lib/cn";
import { formatNumber, formatPercent, formatRelative, formatWhen } from "@/lib/format";
import { useI18n, type Locale } from "@/i18n";
import type { MessageKey } from "@/i18n/en";
import { useUpstreams } from "@/lib/api/queries";
import { needsKey } from "@/features/upstreams/labels";

type T = (key: MessageKey, params?: Record<string, string | number>) => string;

export interface AttentionItem {
  id: string;
  tone: Extract<Tone, "bad" | "violet" | "warn">;
  title: string;
  detail: string;
  upstreamId: string;
  /** The upstream is paused; offer a test that may let it back in. */
  testable?: boolean;
}

const RANK = { bad: 0, violet: 1, warn: 2 } as const;
const CAUSES = new Set(["request", "transient", "rate_limit", "quota", "auth_expired", "banned", "content", "unknown"]);
const LOW_QUOTA = 0.2;
const EXPIRY_SOON_MS = 7 * 86_400_000;

/** Everything about the live upstream and credential state an operator should act on, worst first. */
export function attentionItems(ups: readonly Upstream[], t: T, locale: Locale, now = Date.now()): AttentionItem[] {
  const out: AttentionItem[] = [];
  const at = (iso: string) => formatWhen(iso, locale);
  for (const u of ups) {
    if (!u.enabled) continue;
    if (u.circuit.state !== "closed") {
      out.push({
        id: `${u.id}:circuit`,
        tone: "bad",
        title: u.circuit.until
          ? t("overview.att.pausedUntil", { name: u.name, rel: formatRelative(u.circuit.until, locale, now) })
          : t("overview.att.paused", { name: u.name }),
        detail: t("overview.att.pausedWhy"),
        upstreamId: u.id,
        testable: true,
      });
    }
    if (needsKey(u)) {
      out.push({ id: `${u.id}:key`, tone: "bad", title: t("overview.att.needsKey", { name: u.name }), detail: t("overview.att.needsKeyWhy"), upstreamId: u.id });
    }
    for (const c of u.credentials) {
      if (c.enabled === false) continue;
      const s = c.state;
      const cause = s.reason && CAUSES.has(s.reason) ? s.reason : undefined;
      const base = { upstreamId: u.id };
      if (s.status === "quarantined") {
        out.push({ ...base, id: `${c.id}:q`, tone: "violet", title: t("overview.att.quarantined", { name: c.label }), detail: t("overview.att.quarantinedWhy", { upstream: u.name }) });
      } else if (s.status === "manual") {
        out.push({ ...base, id: `${c.id}:m`, tone: "warn", title: t("overview.att.manual", { name: c.label }), detail: u.name });
      } else if (s.status === "cooldown") {
        out.push({
          ...base,
          id: `${c.id}:c`,
          tone: "warn",
          title: s.until ? t("overview.att.coolingUntil", { name: c.label, at: at(s.until) }) : t("overview.att.cooling", { name: c.label }),
          detail: [u.name, cause ? t(`cause.${cause as "rate_limit"}`) : null, s.model_scope ? t("overview.att.onlyModel", { model: s.model_scope }) : null]
            .filter(Boolean)
            .join(" · "),
        });
      }
      if (s.attention) {
        out.push({ ...base, id: `${c.id}:a`, tone: "warn", title: t("overview.att.look", { name: c.label }), detail: `${u.name} · ${at(s.attention.since)}` });
      }
      const qr = s.quota?.remaining;
      if (qr !== undefined && qr >= 0 && qr < LOW_QUOTA && s.status !== "quarantined") {
        out.push({
          ...base,
          id: `${c.id}:quota`,
          tone: "warn",
          title: t("overview.att.lowQuota", { name: c.label, pct: formatPercent(qr, locale, 0) }),
          detail: s.quota?.reset_at ? `${u.name} · ${t("overview.att.resets", { at: at(s.quota.reset_at) })}` : u.name,
        });
      }
      if (c.expires_at && Date.parse(c.expires_at) - now < EXPIRY_SOON_MS) {
        const gone = Date.parse(c.expires_at) <= now;
        out.push({
          ...base,
          id: `${c.id}:exp`,
          tone: gone ? "bad" : "warn",
          title: gone ? t("overview.att.expired", { name: c.label }) : t("overview.att.expires", { name: c.label, rel: formatRelative(c.expires_at, locale, now) }),
          detail: u.name,
        });
      }
    }
  }
  return out.sort((a, b) => RANK[a.tone] - RANK[b.tone]);
}

function TestButton({ id, name }: { id: string; name: string }) {
  const { t } = useI18n();
  const qc = useQueryClient();
  const m = useMutation({
    mutationFn: () => unwrap(api.POST("/upstreams/{id}/test", { params: { path: { id } }, body: {} })),
    onSuccess: (r) => {
      if (r.ok) toast.success(t("overview.att.testOk", { name }));
      else toast.error(t("overview.att.testFailed", { name }));
      void qc.invalidateQueries({ queryKey: ["upstreams"] });
    },
    onError: () => toast.error(t("overview.att.testFailed", { name })),
  });
  return (
    <Button size="sm" onClick={() => m.mutate()} disabled={m.isPending}>
      {m.isPending ? t("overview.att.testing") : t("overview.att.test")}
    </Button>
  );
}

/** Live credential and upstream problems, each linking to its upstream. */
export function NeedsAttention() {
  const { t, locale } = useI18n();
  const q = useUpstreams({ refetchInterval: 30_000 });
  const items = q.data ? attentionItems(q.data.items, t, locale) : [];
  const names = new Map(q.data?.items.map((u) => [u.id, u.name]));
  return (
    <Panel
      title={
        <span className="flex items-center gap-2">
          {t("overview.attention")}
          {items.length > 0 && <Pill tone={items.some((i) => i.tone === "bad") ? "bad" : "warn"}>{items.length}</Pill>}
        </span>
      }
      bodyClassName="p-0"
    >
      {q.isError ? (
        <div className="p-4">
          <ErrorState error={q.error} onRetry={() => void q.refetch()} />
        </div>
      ) : !q.data ? (
        <div className="grid gap-3 p-4">
          {[0, 1, 2].map((i) => (
            <Skeleton key={i} className="h-9" />
          ))}
        </div>
      ) : items.length === 0 ? (
        <EmptyState compact icon={CircleCheck} title={t("overview.att.none")} hint={t("overview.att.noneHint")} />
      ) : (
        <ul className="max-h-[316px] overflow-y-auto">
          {items.map((it) => (
            <li key={it.id} className="flex items-center gap-3 border-b border-line-soft px-4 py-2.5 last:border-0">
              <StatusDot tone={it.tone} />
              <div className="min-w-0 flex-1">
                <p className="line-clamp-2 text-sm">{it.title}</p>
                <p className="truncate text-xs text-ink-3">{it.detail}</p>
              </div>
              {it.testable && <TestButton id={it.upstreamId} name={names.get(it.upstreamId) ?? ""} />}
              <Button size="sm" variant="ghost" asChild>
                <Link to="/upstreams" search={{ id: it.upstreamId }}>
                  {t("overview.att.open")}
                </Link>
              </Button>
            </li>
          ))}
        </ul>
      )}
    </Panel>
  );
}

function healthTone(u: Upstream): Tone {
  if (!u.enabled) return "neutral";
  if (u.circuit.state !== "closed") return "bad";
  const r = u.stats.success_rate_24h;
  if (r === null || r === undefined) return "neutral";
  return r >= 0.99 ? "ok" : r >= 0.95 ? "warn" : "bad";
}

function tickClass(v: number | null): string {
  if (v === null) return "bg-line";
  return v >= 0.99 ? "bg-ok/80" : v >= 0.95 ? "bg-warn" : "bg-bad";
}

/** Every upstream's last 24 hours as hourly ticks, with its success rate and request count. */
export function UpstreamHealth() {
  const { t, locale } = useI18n();
  const q = useUpstreams({ refetchInterval: 30_000 });
  return (
    <Panel
      title={t("overview.health")}
      aside={
        <Link to="/upstreams" className="text-xs text-accent hover:underline">
          {t("overview.allUpstreams")}
        </Link>
      }
      bodyClassName="px-4 py-1.5"
    >
      {q.isError ? (
        <ErrorState error={q.error} onRetry={() => void q.refetch()} />
      ) : !q.data ? (
        <div className="grid gap-3 py-2.5">
          {[0, 1, 2, 3].map((i) => (
            <Skeleton key={i} className="h-5" />
          ))}
        </div>
      ) : q.data.items.length === 0 ? (
        <EmptyState compact title={t("overview.noUpstreams")} />
      ) : (
        <ul>
          {q.data.items.map((u) => {
            const rate = u.stats.success_rate_24h;
            return (
              <li key={u.id}>
                <Link
                  to="/upstreams"
                  search={{ id: u.id }}
                  className="-mx-2 grid grid-cols-[10px_minmax(96px,150px)_1fr_auto] items-center gap-3 rounded-md px-2 py-2 text-sm outline-none hover:bg-surface-2/70 focus-visible:ring-2 focus-visible:ring-accent max-sm:grid-cols-[10px_1fr_auto]"
                >
                  <StatusDot tone={healthTone(u)} />
                  <span className={cn("truncate font-medium", !u.enabled && "text-ink-3")}>{u.name}</span>
                  <span className="flex h-3.5 items-stretch gap-[2px] max-sm:hidden" role="img" aria-label={t("overview.healthTicks", { name: u.name })}>
                    {u.stats.hourly.map((v, i) => (
                      <i
                        key={i}
                        title={v === null ? t("overview.noTraffic") : formatPercent(v, locale)}
                        className={cn("min-w-0 flex-1 rounded-[2px]", tickClass(v))}
                      />
                    ))}
                  </span>
                  <span className="flex items-baseline gap-2 whitespace-nowrap text-right tabular-nums">
                    <span className="text-xs text-ink-3">{t("overview.reqCount", { n: formatNumber(u.stats.requests_24h, locale) })}</span>
                    <span className="w-12">{rate === null || rate === undefined ? "—" : formatPercent(rate, locale)}</span>
                  </span>
                </Link>
              </li>
            );
          })}
        </ul>
      )}
    </Panel>
  );
}
