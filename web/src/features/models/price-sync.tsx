import { useMutation, useQueryClient } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { ArrowRight, Check, CloudDownload, X } from "lucide-react";
import { toast } from "sonner";
import { WithRawValue, RAW_SLOT } from "@/components/raw-value";
import { Pill } from "@/components/status";
import { Button } from "@/components/ui/button";
import { Switch } from "@/components/ui/switch";
import { api, unwrap } from "@/lib/api/client";
import { errorMessage } from "@/lib/api/errors";
import type { PriceSuggestion as Suggestion, PriceSyncStatus as SyncStatus } from "@/lib/api/types";
import { formatCompact, formatDateTime, formatRelative } from "@/lib/format";
import { formatPrice } from "@/lib/money";
import { useI18n } from "@/i18n";

const path = (model: string) => ({ params: { path: { model } } });

/** Accept, reject and follow, shared by the suggestion rows and the price drawer. */
export function usePriceSync() {
  const { t } = useI18n();
  const qc = useQueryClient();
  const done = (msg: string) => () => {
    void qc.invalidateQueries({ queryKey: ["pricing-rules"] });
    toast.success(msg);
  };
  const onError = (e: unknown) => toast.error(errorMessage(e));
  return {
    accept: useMutation({ mutationFn: (m: string) => unwrap(api.POST("/price-sync/{model}/accept", path(m))), onSuccess: done(t("pricing.confirmed")), onError }),
    reject: useMutation({ mutationFn: (m: string) => unwrap(api.POST("/price-sync/{model}/reject", path(m))), onSuccess: done(t("pricing.rejected")), onError }),
    follow: useMutation({
      mutationFn: ({ model, follow }: { model: string; follow: boolean }) => unwrap(api.PATCH("/price-sync/{model}", { ...path(model), body: { follow } })),
      onSuccess: (_, v) => done(v.follow ? t("pricing.followOn") : t("pricing.followOff"))(),
      onError,
    }),
  };
}

/** One line about the last price check, or how to turn it on. */
export function SyncBanner({ sync }: { sync: SyncStatus }) {
  const { t, locale } = useI18n();
  let body;
  if (!sync.enabled) {
    body = (
      <>
        <span className="text-ink-2">{t("pricing.sync.off")}</span>
        <Link to="/features" search={{ id: "price_sync" }} className="ml-auto inline-flex items-center gap-1 font-medium text-accent hover:underline">
          {t("pricing.sync.turnOn")}
          <ArrowRight className="size-3.5" />
        </Link>
      </>
    );
  } else if (!sync.ran_at) {
    body = <span className="text-ink-2">{t("pricing.sync.first")}</span>;
  } else if (sync.error_code) {
    body = (
      <span className="text-warn">
        {t(`pricing.sync.err.${sync.error_code}`, { when: formatRelative(sync.ran_at, locale) })}
        {sync.error_code !== "no_rate" && sync.error && (
          <span data-raw className="ml-2 font-mono text-xs text-ink-3">
            {sync.error}
          </span>
        )}
      </span>
    );
  } else {
    body = <span className="text-ink-2">{t("pricing.sync.ran", { when: formatRelative(sync.ran_at, locale), n: sync.matched })}</span>;
  }
  return (
    <div className="mb-3 flex flex-wrap items-center gap-2 rounded-lg border border-line bg-surface-2 px-3.5 py-2 text-sm">
      <CloudDownload className="size-4 shrink-0 text-ink-3" />
      {body}
    </div>
  );
}

function PricePair({ input, output, currency }: { input: number; output: number; currency: string }) {
  const { locale } = useI18n();
  return (
    <span className="tabular-nums">
      {formatPrice(input, currency, locale)}
      <span className="text-ink-3"> / </span>
      {formatPrice(output, currency, locale)}
    </span>
  );
}

/** The models.dev prices that differ from what models are charged, each with accept / reject / follow. */
export function SuggestionList({ items }: { items: Suggestion[] }) {
  const { t, locale } = useI18n();
  const ps = usePriceSync();
  if (!items.length) return null;
  const busy = ps.accept.isPending || ps.reject.isPending || ps.follow.isPending;
  return (
    <section className="mb-4 rounded-lg border border-warn/40 bg-surface" aria-label={t("pricing.suggestions")}>
      <div className="flex items-center justify-between gap-2 border-b border-line-soft px-4 py-2">
        <h2 className="flex items-center gap-2 text-sm font-semibold">
          {t("pricing.suggestions")}
          <Pill tone="warn">{items.length}</Pill>
        </h2>
        <span className="text-xs text-ink-3">{t("pricing.suggestionsHint")}</span>
      </div>
      <ul className="divide-y divide-line-soft">
        {items.map((s) => {
          const hints = [
            s.hints.context_window ? t("pricing.hint.context", { n: formatCompact(s.hints.context_window, locale) }) : null,
            s.hints.reasoning ? t("pricing.hint.reasoning") : null,
            s.hints.vision ? t("pricing.hint.vision") : null,
          ].filter(Boolean);
          return (
            <li key={s.model} className="grid gap-x-4 gap-y-2 px-4 py-3 lg:grid-cols-[minmax(0,1.3fr)_minmax(0,1.6fr)_auto] lg:items-center">
              <div className="min-w-0">
                <div className="truncate font-mono text-[13px] font-medium" data-raw>
                  {s.model}
                </div>
                <div className="mt-0.5 truncate text-xs text-ink-3">
                  <WithRawValue text={t("pricing.from", { ref: RAW_SLOT, when: formatDateTime(s.fetched_at, locale) })} value={s.ref} />
                </div>
              </div>
              <div className="flex min-w-0 flex-wrap items-center gap-x-2 gap-y-1 text-sm">
                <span className="text-xs text-ink-3">{t("pricing.inOut")}</span>
                {s.current ? (
                  <span className="text-ink-3 line-through decoration-ink-3/60">
                    <PricePair input={s.current.input} output={s.current.output} currency={s.current.currency} />
                  </span>
                ) : (
                  <span className="text-ink-3">{t("pricing.noPriceYet")}</span>
                )}
                <ArrowRight className="size-3.5 text-ink-3" />
                <span className="font-medium">
                  <PricePair input={s.prices.input} output={s.prices.output} currency={s.prices.currency} />
                </span>
                {s.prices.cache_read > 0 && (
                  <span className="text-xs text-ink-3">{t("pricing.cacheRead", { price: formatPrice(s.prices.cache_read, s.prices.currency, locale) })}</span>
                )}
                {hints.length > 0 && <span className="w-full text-xs text-ink-3">{hints.join(" · ")}</span>}
              </div>
              <div className="flex flex-wrap items-center gap-2 lg:justify-end">
                <label className="mr-1 flex items-center gap-1.5 text-xs text-ink-2">
                  <Switch checked={s.follow} onChange={(follow) => ps.follow.mutate({ model: s.model, follow })} disabled={busy} aria-label={t("pricing.follow")} />
                  {t("pricing.follow")}
                </label>
                <Button size="xs" variant="ghost" onClick={() => ps.reject.mutate(s.model)} disabled={busy}>
                  <X />
                  {t("pricing.reject")}
                </Button>
                <Button size="xs" variant="primary" onClick={() => ps.accept.mutate(s.model)} disabled={busy}>
                  <Check />
                  {t("pricing.accept")}
                </Button>
              </div>
            </li>
          );
        })}
      </ul>
    </section>
  );
}
