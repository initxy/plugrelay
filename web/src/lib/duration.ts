/** Units a person picks a duration in. Days have no Go spelling and are written as hours. */
export type DurationUnit = "s" | "m" | "h" | "d";

const SECONDS: Record<DurationUnit, number> = { s: 1, m: 60, h: 3600, d: 86400 };
const GO_PART = /(\d+(?:\.\d+)?)(ms|s|m|h)/g;

/** Seconds in a Go duration ("1h30m", "15m", "90s", "0"); null when it is not one. */
export function parseDuration(text: string): number | null {
  const s = text.trim();
  if (s === "0") return 0;
  if (!/^(\d+(\.\d+)?(ms|s|m|h))+$/.test(s)) return null;
  let total = 0;
  for (const [, n, unit] of s.matchAll(GO_PART)) {
    const v = Number(n);
    total += unit === "ms" ? v / 1000 : v * SECONDS[unit as "s" | "m" | "h"];
  }
  return total;
}

/** The largest unit that shows `seconds` as a whole number: 5400 → 90 m, 7200 → 2 h, 172800 → 2 d. */
export function splitDuration(seconds: number): { amount: number; unit: DurationUnit } {
  for (const unit of ["d", "h", "m"] as const) {
    if (seconds >= SECONDS[unit] && seconds % SECONDS[unit] === 0) return { amount: seconds / SECONDS[unit], unit };
  }
  return { amount: seconds, unit: "s" };
}

/** An amount in a unit as a Go duration string: (2, "d") → "48h", (15, "m") → "15m". */
export function toGoDuration(amount: number, unit: DurationUnit): string {
  if (unit === "d") return `${amount * 24}h`;
  return `${amount}${unit}`;
}

/** "6 hours", "90 minutes": seconds in the largest whole unit, with the unit's name from `t`. */
export function formatDurationText(seconds: number, t: (key: `duration.${DurationUnit}`) => string): string {
  const { amount, unit } = splitDuration(seconds);
  return `${amount} ${t(`duration.${unit}`)}`;
}
