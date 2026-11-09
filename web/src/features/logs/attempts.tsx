import type { ReactNode } from "react";
import { Pill } from "@/components/status";
import type { Attempt } from "@/lib/api/types";
import { cn } from "@/lib/cn";
import { formatDuration } from "@/lib/format";
import { useI18n } from "@/i18n";
import { protocolName, wordNote, wordStep, wordThing, type Worded } from "./diagnostics";
import { TIER_TONE } from "./request-pills";

const failedAttempt = (a: Attempt) => a.status < 200 || a.status >= 300 || Boolean(a.cause);

/** What happened on one attempt, in a sentence. */
function useVerdict() {
  const { t } = useI18n();
  return (a: Attempt, last: boolean): string => {
    if (!a.sent) return t("logs.verdict.notSent");
    if (!failedAttempt(a)) {
      if (a.ttfb_ms !== null && a.ttfb_ms !== undefined && a.ttfb_ms < a.duration_ms)
        return t("logs.verdict.okStream", { ttfb: formatDuration(a.ttfb_ms), rest: formatDuration(a.duration_ms - a.ttfb_ms) });
      return t("logs.verdict.ok", { d: formatDuration(a.duration_ms) });
    }
    const why = a.cause ? t(`cause.${a.cause}`) : t("logs.verdict.failed");
    const status = a.status || "—";
    return last ? t("logs.verdict.failedLast", { why, status }) : t("logs.verdict.failedNext", { why, status });
  };
}

/**
 * One bar per upstream attempt on a shared time axis. The pale part of a bar is waiting for the
 * first byte; the solid part is the answer streaming. Failed attempts are red.
 */
export function AttemptWaterfall({ attempts, total }: { attempts: Attempt[]; total: number }) {
  const { t } = useI18n();
  const verdict = useVerdict();
  const span = Math.max(total, ...attempts.map((a) => a.start_ms + a.duration_ms), 1);
  const ticks = [0, 0.25, 0.5, 0.75, 1].map((f) => f * span);
  return (
    <div className="grid gap-3" role="list" aria-label={t("logs.tabAttempts")}>
      <div className="relative ml-[148px] h-4 text-2xs tabular-nums text-ink-3 max-sm:ml-0" aria-hidden>
        {ticks.map((ms, i) => (
          <span
            key={i}
            className={cn("absolute whitespace-nowrap", i === 0 ? "" : i === ticks.length - 1 ? "-translate-x-full" : "-translate-x-1/2")}
            style={{ left: `${(ms / span) * 100}%` }}
          >
            {formatDuration(ms)}
          </span>
        ))}
      </div>
      {attempts.map((a, i) => {
        const failed = failedAttempt(a);
        const left = (a.start_ms / span) * 100;
        const width = Math.max(0.8, (a.duration_ms / span) * 100);
        const wait = a.ttfb_ms ? Math.min(100, (a.ttfb_ms / Math.max(a.duration_ms, 1)) * 100) : failed ? 100 : 0;
        return (
          <div key={a.n} role="listitem" className="grid grid-cols-[140px_1fr] items-start gap-2 max-sm:grid-cols-1">
            <div className="min-w-0 text-sm leading-tight">
              <div className="flex items-center gap-1.5 truncate font-medium">
                <span className="grid size-4 shrink-0 place-items-center rounded-full bg-surface-2 text-2xs tabular-nums text-ink-2">{a.n}</span>
                <span className="truncate">{a.upstream.name}</span>
              </div>
              <div className="mt-0.5 truncate pl-[22px] text-xs text-ink-3">{a.credential?.name ?? protocolName(a.protocol)}</div>
            </div>
            <div className="min-w-0">
              <div className="relative h-4 rounded-sm bg-surface-2">
                {ticks.slice(1, -1).map((ms, j) => (
                  <span key={j} className="absolute inset-y-0 w-px bg-line" style={{ left: `${(ms / span) * 100}%` }} />
                ))}
                <div className="absolute inset-y-0 flex overflow-hidden rounded-[3px]" style={{ left: `${left}%`, width: `${width}%` }}>
                  <span className={cn("h-full", failed ? "bg-bad/45" : "bg-accent/35")} style={{ width: `${wait}%` }} />
                  <span className={cn("h-full flex-1", failed ? "bg-bad" : "bg-accent")} />
                </div>
              </div>
              <p className={cn("mt-1 text-xs", failed ? "text-bad" : "text-ink-2")}>{verdict(a, i === attempts.length - 1)}</p>
            </div>
          </div>
        );
      })}
    </div>
  );
}

function WordedList({ items }: { items: Worded[] }) {
  return (
    <ul className="grid gap-1">
      {items.map((w, i) => (
        <li key={i} className="flex flex-wrap items-baseline gap-x-1.5">
          <span>{w.text}</span>
          {w.raw && <code className="rounded-xs bg-surface-2 px-1 font-mono text-xs text-ink-2">{w.raw}</code>}
        </li>
      ))}
    </ul>
  );
}

function Row({ label, children }: { label: ReactNode; children: ReactNode }) {
  return (
    <>
      <dt className="text-ink-3">{label}</dt>
      <dd className="min-w-0">{children}</dd>
    </>
  );
}

/** Everything about one attempt: handling, model, what the conversion did, what the upstream said. */
export function AttemptDetail({ a, clientProtocol }: { a: Attempt; clientProtocol?: string }) {
  const { t } = useI18n();
  const d = a.diagnostic;
  const lists = d
    ? ([
        ["steps", (d.steps ?? []).map((s) => wordStep(s, t))],
        ["dropped", (d.dropped ?? []).map((s) => wordThing(s, t))],
        ["missing", (d.missing ?? []).map((s) => wordThing(s, t))],
        ["notes", (d.notes ?? []).map((s) => wordNote(s, t))],
      ] as const)
    : [];
  return (
    <section className="rounded-lg border border-line" aria-label={t("logs.attemptN", { n: a.n, upstream: a.upstream.name })}>
      <header className="flex flex-wrap items-center gap-2 border-b border-line-soft px-3.5 py-2">
        <h4 className="text-sm font-semibold">{t("logs.attemptN", { n: a.n, upstream: a.upstream.name })}</h4>
        <Pill tone={TIER_TONE[a.tier]}>{t(`tier.${a.tier}`)}</Pill>
        {d && <Pill tone={d.fidelity === "lossless" ? "ok" : d.fidelity === "impossible" ? "bad" : "warn"}>{t(`fidelity.${d.fidelity}`)}</Pill>}
      </header>
      <dl className="grid grid-cols-[minmax(96px,max-content)_1fr] gap-x-4 gap-y-2 px-3.5 py-3 text-sm">
        <Row label={t("logs.handling")}>
          {t(`tier.${a.tier}.long`, { from: protocolName(clientProtocol), to: protocolName(a.protocol) })}
        </Row>
        {a.credential && <Row label={t("logs.credential")}>{a.credential.name}</Row>}
        <Row label={t("logs.upstreamModel")}>
          <span className="font-mono text-xs">{a.upstream_model ?? "—"}</span>
        </Row>
        {lists.map(([k, items]) =>
          items.length ? (
            <Row key={k} label={t(`logs.diag.${k}`)}>
              <WordedList items={items} />
            </Row>
          ) : null,
        )}
        {a.error && (
          <Row label={t("logs.upstreamSaid")}>
            <pre className="whitespace-pre-wrap break-words rounded-md bg-bad-soft px-2.5 py-2 font-mono text-xs text-bad">{a.error}</pre>
          </Row>
        )}
      </dl>
      {!d && a.tier !== "convert" && <p className="border-t border-line-soft px-3.5 py-2 text-xs text-ink-3">{t("logs.noConversion")}</p>}
    </section>
  );
}
