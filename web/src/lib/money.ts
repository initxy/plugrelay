import type { Money } from "./api/types";

const MICRO = 1_000_000;

/**
 * Decimal places that keep small amounts readable without padding large ones:
 * ¥1,204.50 · ¥3.20 · ¥0.197 · ¥0.0042.
 */
function fractionDigits(abs: number): number {
  if (abs === 0 || abs >= 1) return 2;
  if (abs >= 0.01) return 3;
  return 4;
}

export function formatMoney(m: Money, locale: string, opts: { compact?: boolean } = {}): string {
  const value = m.micro / MICRO;
  const abs = Math.abs(value);
  if (opts.compact && abs >= 10_000) {
    return new Intl.NumberFormat(locale, {
      style: "currency",
      currency: m.currency,
      currencyDisplay: "narrowSymbol",
      notation: "compact",
      maximumFractionDigits: 1,
    }).format(value);
  }
  const digits = fractionDigits(abs);
  return new Intl.NumberFormat(locale, {
    style: "currency",
    currency: m.currency,
    currencyDisplay: "narrowSymbol",
    minimumFractionDigits: value === 0 ? 0 : Math.min(2, digits),
    maximumFractionDigits: digits,
  }).format(value);
}

/** A per-million-token price, e.g. "$1.25". The unit label is added by the caller. */
export function formatPrice(microPerMillion: number, currency: string, locale: string): string {
  return formatMoney({ micro: microPerMillion, currency }, locale);
}

/** tokens × price-per-million → micro-units, rounded half away from zero. */
export function lineAmount(tokens: number, microPerMillion: number): number {
  const v = (tokens * microPerMillion) / MICRO;
  return Math.sign(v) * Math.round(Math.abs(v));
}

/**
 * Parse what a person typed ("12.5", "¥12.50", "1,000", "12,5" in a comma-decimal locale) into
 * micro-units; null when it is not a number. With both separators the last one is the decimal
 * point; a lone "." is always decimal because input fields are prefilled that way.
 */
export function parseMoneyInput(text: string, locale = "en"): number | null {
  let cleaned = text.replace(/[^\d.,-]/g, "");
  const comma = cleaned.lastIndexOf(",");
  const dot = cleaned.lastIndexOf(".");
  const commaDecimal = (1.5).toLocaleString(locale).includes(",");
  if (comma > dot && (dot >= 0 || commaDecimal)) cleaned = cleaned.replace(/\./g, "").replace(",", ".");
  cleaned = cleaned.replace(/,/g, "");
  if (cleaned === "" || cleaned === "-" || cleaned === ".") return null;
  const n = Number(cleaned);
  if (!Number.isFinite(n)) return null;
  return Math.round(n * MICRO);
}

/** micro-units → plain decimal string for an input field. */
export function moneyInputValue(micro: number | undefined): string {
  if (micro === undefined) return "";
  return String(micro / MICRO);
}

export function currencySymbol(currency: string, locale: string): string {
  const part = new Intl.NumberFormat(locale, { style: "currency", currency, currencyDisplay: "narrowSymbol" })
    .formatToParts(0)
    .find((p) => p.type === "currency");
  return part?.value ?? currency;
}
