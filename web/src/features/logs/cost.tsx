import { Info } from "lucide-react";
import { useMoney } from "@/components/money";
import type { Attempt, Billing } from "@/lib/api/types";
import { formatNumber } from "@/lib/format";
import { useI18n } from "@/i18n";

/** The charge written out line by line: tokens × price per million = amount, then the total. */
export function CostBreakdown({ billing, attempts }: { billing: Billing; attempts: Attempt[] }) {
  const { t, locale } = useI18n();
  const money = useMoney();
  const uncharged = attempts.filter((a) => a.sent && !a.charged).length;
  const notes = [
    billing.multiplier !== undefined && billing.multiplier !== 1 && t("logs.cost.multiplier", { m: String(billing.multiplier) }),
    uncharged > 0 && t("logs.cost.failedFree", { n: uncharged }),
    billing.note && t(`logs.cost.note.${billing.note}`),
  ].filter((n): n is string => Boolean(n));
  return (
    <div className="grid gap-4">
      <div className="overflow-hidden rounded-lg border border-line">
        {billing.lines.length === 0 ? (
          <p className="px-3.5 py-3 text-sm text-ink-3">{t("logs.cost.noTokens")}</p>
        ) : (
          <table className="w-full text-sm tabular-nums">
            <thead className="sr-only">
              <tr>
                <th>{t("logs.cost.bucket")}</th>
                <th>{t("logs.cost.formula")}</th>
                <th>{t("logs.cost.amount")}</th>
              </tr>
            </thead>
            <tbody>
              {billing.lines.map((l) => (
                <tr key={l.bucket} className="border-b border-dashed border-line last:border-0">
                  <td className="py-2 pl-3.5 pr-2 text-ink-2">{t(`usage.${l.bucket}`)}</td>
                  <td className="px-2 py-2 text-ink-3">
                    {t("logs.cost.line", { tokens: formatNumber(l.tokens, locale), price: money(l.unit_price) })}
                  </td>
                  <td className="py-2 pl-2 pr-3.5 text-right">{money(l.amount)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
        <div className="flex items-baseline justify-between gap-3 border-t border-line bg-surface-2/60 px-3.5 py-2.5">
          <span className="text-sm font-medium">{t("logs.cost.charged")}</span>
          <span className="flex items-baseline gap-3">
            {billing.reserved && billing.refunded && (
              <span className="text-xs text-ink-3">
                {t("logs.cost.held", { reserved: money(billing.reserved), refunded: money(billing.refunded) })}
              </span>
            )}
            <span className="text-base font-semibold tabular-nums">{money(billing.charged)}</span>
          </span>
        </div>
      </div>
      {billing.cost && (
        <div className="flex items-baseline justify-between rounded-lg border border-line px-3.5 py-2.5 text-sm">
          <span className="text-ink-2">{t("logs.cost.operator")}</span>
          <span className="tabular-nums">{money(billing.cost)}</span>
        </div>
      )}
      {notes.length > 0 && (
        <ul className="grid gap-1.5 text-xs text-ink-2">
          {notes.map((n) => (
            <li key={n} className="flex items-start gap-1.5">
              <Info className="mt-px size-3.5 shrink-0 text-ink-3" />
              {n}
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}
