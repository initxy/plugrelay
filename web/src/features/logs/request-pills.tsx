import { Pill, type Tone } from "@/components/status";
import type { LogSummary, Tier } from "@/lib/api/types";
import { formatClock } from "@/lib/format";
import { useI18n } from "@/i18n";

export const TIER_TONE: Record<Tier, Tone> = { passthrough: "neutral", patch: "info", convert: "violet" };

/** How the request was handled, or how it failed: "原样转发", "失败 · 503 上游出错". */
export function HandlingPill({ log }: { log: Pick<LogSummary, "status" | "cause" | "tier"> }) {
  const { t } = useI18n();
  if (log.status < 200 || log.status >= 300)
    return (
      <Pill tone="bad">
        {t("logs.failedPill", { status: log.status || "—" })}
        {log.cause ? ` ${t(`cause.${log.cause}`)}` : ""}
      </Pill>
    );
  if (!log.tier) return null;
  return (
    <Pill tone={TIER_TONE[log.tier]} title={t(`tier.${log.tier}.hint`)}>
      {t(`tier.${log.tier}`)}
    </Pill>
  );
}

/** Relay-side retries, client resends and playground requests, next to the handling pill. */
export function RetryPills({ log }: { log: Pick<LogSummary, "attempts" | "resent" | "client"> }) {
  const { t } = useI18n();
  return (
    <>
      {log.client?.id === "playground" && (
        <Pill tone="info" title={t("logs.playgroundHint")}>
          {t("logs.playgroundPill")}
        </Pill>
      )}
      {log.attempts > 1 && <Pill tone="warn">{t("logs.retryPill", { n: log.attempts - 1 })}</Pill>}
      {log.resent && (
        <Pill tone="neutral" title={t("logs.resentHint")}>
          {t("logs.resentPill")}
        </Pill>
      )}
    </>
  );
}

/** "auto-coding → claude-sonnet-4-5" when a preset picked the model, else just the model. */
export function ModelName({ log, className }: { log: Pick<LogSummary, "model" | "served_model">; className?: string }) {
  const { t } = useI18n();
  const swapped = log.served_model && log.served_model !== log.model;
  return (
    <span className={className} title={swapped ? t("logs.presetHint") : undefined}>
      {swapped ? (
        <>
          <span className="text-ink-3">{log.model} → </span>
          <span className="font-medium">{log.served_model}</span>
        </>
      ) : (
        <span className="font-medium">{log.model}</span>
      )}
    </span>
  );
}

/** Clock time of a request, with the day in front when it isn't today. */
export function RequestTime({ iso }: { iso: string }) {
  const { locale } = useI18n();
  const d = new Date(iso);
  const today = d.toDateString() === new Date().toDateString();
  return (
    <span className="whitespace-nowrap tabular-nums text-ink-2" title={d.toISOString()}>
      {!today && <span className="mr-1.5 text-ink-3">{d.toLocaleDateString(locale, { month: "numeric", day: "numeric" })}</span>}
      {formatClock(iso, locale)}
    </span>
  );
}
