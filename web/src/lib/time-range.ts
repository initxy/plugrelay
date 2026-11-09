import type { TimeRange } from "./api/types";

export const TIME_RANGES: readonly TimeRange[] = ["1h", "24h", "7d", "30d", "90d"];

const MS: Record<TimeRange, number> = {
  "1h": 3_600_000,
  "24h": 86_400_000,
  "7d": 7 * 86_400_000,
  "30d": 30 * 86_400_000,
  "90d": 90 * 86_400_000,
};

export function rangeStart(range: TimeRange, now = Date.now()): string {
  return new Date(now - MS[range]).toISOString();
}

export function isTimeRange(v: unknown): v is TimeRange {
  return typeof v === "string" && (TIME_RANGES as readonly string[]).includes(v);
}
