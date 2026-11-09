import { useMutation, useQueryClient } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { RotateCcw } from "lucide-react";
import { toast } from "sonner";
import { Pill } from "@/components/status";
import { Button } from "@/components/ui/button";
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover";
import { api, unwrap } from "@/lib/api/client";
import { errorMessage } from "@/lib/api/errors";
import type { Cause, Credential } from "@/lib/api/types";
import { cn } from "@/lib/cn";
import { formatClock, formatRelative, formatWhen } from "@/lib/format";
import { useI18n } from "@/i18n";
import type { MessageKey } from "@/i18n/en";
import { credentialFine, credentialResettable, credentialTone } from "./labels";

const CAUSES: readonly Cause[] = ["request", "transient", "rate_limit", "quota", "auth_expired", "banned", "content", "unknown"];
const QUOTA_PROBE_REASON = "quota used up (quota probe)";
const REFRESH_UNSAVED = "refreshed secret could not be saved";

/**
 * The router's reason in plain words, or undefined when it is an internal note with nothing
 * for a person to act on. Cause codes and the quota probe's note have their own text.
 */
export function useReasonText() {
  const { t } = useI18n();
  return (reason: string | undefined): string | undefined => {
    if (!reason) return undefined;
    if ((CAUSES as readonly string[]).includes(reason)) return t(`cred.cause.${reason}` as MessageKey);
    if (reason === QUOTA_PROBE_REASON) return t("cred.reason.quotaProbe");
    if (reason.startsWith(REFRESH_UNSAVED)) return t("cred.reason.refreshUnsaved");
    return undefined;
  };
}

/** A credential's state named by what happened: "被限流，冷却到 14:05", "疑似封号，已停用". */
function useCredentialLabel() {
  const { t, locale } = useI18n();
  return (c: Credential): string => {
    const s = c.state;
    const at = s.until ? formatClock(s.until, locale).slice(0, 5) : undefined;
    // The relay sends the cause code as `reason`; it never fills `cause`.
    const cause = (CAUSES as readonly string[]).includes(s.reason ?? "") ? (s.reason as Cause) : undefined;
    switch (s.status) {
      case "ok":
        return s.attention ? t("cred.attention") : t("cred.ok");
      case "cooldown":
        if (s.model_scope) return at ? t("cred.modelCoolingUntil", { model: s.model_scope, at }) : t("cred.modelCooling", { model: s.model_scope });
        if (cause === "rate_limit") return at ? t("cred.rateLimitedUntil", { at }) : t("cred.rateLimited");
        if (cause === "quota" || s.reason === QUOTA_PROBE_REASON) return at ? t("cred.quotaOutUntil", { at }) : t("cred.quotaOut");
        return at ? t("cred.coolingUntil", { at }) : t("cred.cooling");
      case "circuit_open":
        return at ? t("cred.pausedUntil", { at }) : t("cred.paused");
      case "quarantined":
        return cause === "banned" ? t("cred.suspectedBan") : t("cred.held");
      case "manual":
        return cause === "auth_expired" ? t("cred.authExpired") : t("cred.manual");
      case "refreshing":
        return t("cred.refreshing");
      case "disabled":
        return t("cred.off");
    }
  };
}

/** The live state as a pill. */
export function CredentialPill({ c, className }: { c: Credential; className?: string }) {
  const label = useCredentialLabel();
  return (
    <Pill tone={credentialTone(c)} dot className={className}>
      {label(c)}
    </Pill>
  );
}

const QUOTA_LOW = 0.15;

/** The last quota probe: a bar of what is left, the amounts when reported, and when it resets. */
export function CredentialQuota({ c, compact, className }: { c: Credential; compact?: boolean; className?: string }) {
  const { t, locale } = useI18n();
  const q = c.state.quota;
  if (!q) return null;
  const known = q.remaining >= 0;
  const low = known && q.remaining <= QUOTA_LOW;
  const pct = Math.round(Math.max(0, q.remaining) * 100);
  const amount = (n: number) => n.toLocaleString(locale, { maximumFractionDigits: 2 });
  return (
    <div className={cn("grid gap-1", className)}>
      {known && <QuotaBar remaining={q.remaining} label={t("cred.quotaLeft")} />}
      <div className="flex flex-wrap justify-between gap-x-3 text-xs tabular-nums">
        <span className={low ? "font-medium text-warn" : "text-ink-2"}>
          {!known
            ? t("cred.quotaUnknown")
            : q.limit !== undefined && q.left !== undefined
              ? t("cred.quotaAmount", { pct, left: amount(q.left), limit: amount(q.limit) })
              : t("cred.quotaPct", { pct })}
        </span>
        {!compact && (
          <span className="text-ink-3">
            {q.reset_at ? t("cred.quotaResets", { at: formatWhen(q.reset_at, locale), rel: formatRelative(q.reset_at, locale) }) : t("cred.quotaNoReset")}
          </span>
        )}
      </div>
      {q.error && !compact && <div className="break-words text-xs text-warn">{t("cred.quotaError", { error: q.error })}</div>}
    </div>
  );
}

export function QuotaBar({ remaining, label, className }: { remaining: number; label: string; className?: string }) {
  const low = remaining <= QUOTA_LOW;
  const pct = Math.round(Math.max(0, Math.min(1, remaining)) * 100);
  return (
    <div
      className={cn("h-1.5 overflow-hidden rounded-full bg-surface-3", className)}
      role="meter"
      aria-label={label}
      aria-valuemin={0}
      aria-valuemax={100}
      aria-valuenow={pct}
    >
      <div className={cn("h-full rounded-full", low ? "bg-warn" : "bg-accent")} style={{ width: `${Math.max(2, pct)}%` }} />
    </div>
  );
}

/** Errors the upstream answered, verbatim, with a link to each request. */
export function RecentErrors({ c, className }: { c: Credential; className?: string }) {
  const { t, locale } = useI18n();
  const errs = c.state.recent_errors ?? [];
  if (errs.length === 0) return null;
  return (
    <div className={className}>
      <div className="mb-1 text-xs font-medium text-ink-2">{t("cred.recentErrors")}</div>
      <ul className="grid max-h-44 gap-1.5 overflow-y-auto">
        {errs.map((e, i) => (
          <li key={i} className="rounded-md border border-line-soft bg-surface-2/60 px-2 py-1.5 text-xs">
            <div className="flex justify-between gap-2 tabular-nums text-ink-3">
              <span>
                {e.status ?? "—"} {e.cause ? t(`cause.${e.cause}` as MessageKey) : ""}
              </span>
              <span>{formatWhen(e.time, locale)}</span>
            </div>
            <div className="mt-0.5 break-words font-mono text-[11.5px] text-ink">{e.message}</div>
            {e.request_id && (
              <Link to="/logs" search={{ id: e.request_id }} className="text-accent hover:underline">
                {t("cred.viewRequest")}
              </Link>
            )}
          </li>
        ))}
      </ul>
    </div>
  );
}

export function useResetCredential(upstreamId: string) {
  const { t } = useI18n();
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (c: Credential) => unwrap(api.POST("/credentials/{id}/reset", { params: { path: { id: c.id } } })),
    onSuccess: (_, c) => {
      toast.success(t("cred.resetDone", { name: c.label }));
      void qc.invalidateQueries({ queryKey: ["upstreams"] });
      void qc.invalidateQueries({ queryKey: ["upstream", upstreamId] });
    },
    onError: (e) => toast.error(errorMessage(e)),
  });
}

/** One credential's state with its reason, quota, recent errors and a reset button. */
function CredentialChip({ c, upstreamId }: { c: Credential; upstreamId: string }) {
  const { t, locale } = useI18n();
  const reasonText = useReasonText();
  const stateLabel = useCredentialLabel();
  const reset = useResetCredential(upstreamId);
  const s = c.state;
  const reason = s.status === "ok" ? undefined : reasonText(s.reason);
  return (
    <Popover>
      <PopoverTrigger asChild>
        <button type="button" onClick={(e) => e.stopPropagation()} className="rounded-full">
          <Pill tone={credentialTone(c)} dot className="max-w-72 cursor-pointer hover:brightness-95">
            <span className="truncate font-normal opacity-80">{c.label}</span>
            <span className="truncate">{stateLabel(c)}</span>
          </Pill>
        </button>
      </PopoverTrigger>
      <PopoverContent className="w-96" align="start">
        <div onClick={(e) => e.stopPropagation()} className="grid gap-2">
          <div className="flex items-start justify-between gap-2">
            <div className="min-w-0">
              <div className="truncate text-sm font-medium">{c.label}</div>
              <div className="font-mono text-xs text-ink-3">{c.secret_hint}</div>
            </div>
            <CredentialPill c={c} />
          </div>
          {reason && <p className="text-sm text-ink-2">{reason}</p>}
          {s.until && s.status !== "ok" && (
            <p className="text-xs text-ink-3">{t("cred.until", { at: formatWhen(s.until, locale), rel: formatRelative(s.until, locale) })}</p>
          )}
          {s.attention && (
            <p className="text-sm text-warn">
              {reasonText(s.attention.reason) ?? t("cred.attention")} <span className="text-xs text-ink-3">· {formatWhen(s.attention.since, locale)}</span>
            </p>
          )}
          <CredentialQuota c={c} />
          <RecentErrors c={c} />
          {credentialResettable(c) && (
            <div className="flex items-center justify-between gap-2 border-t border-line-soft pt-2">
              <span className="text-xs text-ink-3">{s.status === "quarantined" ? t("cred.resetWarn") : ""}</span>
              <Button size="sm" onClick={() => reset.mutate(c)} disabled={reset.isPending}>
                <RotateCcw />
                {t("cred.reset")}
              </Button>
            </div>
          )}
        </div>
      </PopoverContent>
    </Popover>
  );
}

/** Healthy credentials collapse into "3 个正常"; each one with something going on gets its own pill. */
export function CredentialSummary({ credentials, upstreamId, keyOptional }: { credentials: Credential[]; upstreamId: string; keyOptional: boolean }) {
  const { t } = useI18n();
  const ok = credentials.filter(credentialFine);
  const other = credentials.filter((c) => !credentialFine(c));
  if (credentials.length === 0) return keyOptional ? <Pill tone="neutral">{t("cred.keyless")}</Pill> : <Pill tone="bad">{t("cred.needsKey")}</Pill>;
  return (
    <span className="flex flex-wrap items-center gap-1">
      {ok.length > 0 && (
        <Pill tone="ok" dot>
          {t("cred.okCount", { n: ok.length })}
        </Pill>
      )}
      {other.map((c) => (
        <CredentialChip key={c.id} c={c} upstreamId={upstreamId} />
      ))}
    </span>
  );
}
