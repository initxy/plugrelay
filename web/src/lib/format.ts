export function formatNumber(n: number, locale: string): string {
  return new Intl.NumberFormat(locale).format(n);
}

export function formatCompact(n: number, locale: string): string {
  return new Intl.NumberFormat(locale, { notation: "compact", maximumFractionDigits: 1 }).format(n);
}

export function formatPercent(ratio: number, locale: string, digits = 1): string {
  return new Intl.NumberFormat(locale, {
    style: "percent",
    maximumFractionDigits: ratio === 1 || ratio === 0 ? 0 : digits,
  }).format(ratio);
}

/** 840 → "840ms", 8400 → "8.4s", 125000 → "2m 5s". */
export function formatDuration(ms: number): string {
  if (ms < 1000) return `${Math.round(ms)}ms`;
  if (ms < 60_000) return `${(ms / 1000).toFixed(ms < 10_000 ? 1 : 0)}s`;
  const secs = Math.round(ms / 1000);
  if (secs >= 3600) {
    const h = Math.floor(secs / 3600);
    const m = Math.round((secs % 3600) / 60);
    return m ? `${h}h ${m}m` : `${h}h`;
  }
  const m = Math.floor(secs / 60);
  const s = secs % 60;
  return s ? `${m}m ${s}s` : `${m}m`;
}

const sameDay = (a: Date, b: Date) =>
  a.getFullYear() === b.getFullYear() && a.getMonth() === b.getMonth() && a.getDate() === b.getDate();

/** Time of day for today, date + time otherwise. */
export function formatWhen(iso: string, locale: string, now = new Date()): string {
  const d = new Date(iso);
  const time = d.toLocaleTimeString(locale, { hour: "2-digit", minute: "2-digit", hour12: false });
  if (sameDay(d, now)) return time;
  const date = d.toLocaleDateString(locale, { month: "short", day: "numeric" });
  return `${date} ${time}`;
}

export function formatClock(iso: string, locale: string): string {
  return new Date(iso).toLocaleTimeString(locale, {
    hour: "2-digit",
    minute: "2-digit",
    second: "2-digit",
    hour12: false,
  });
}

export function formatDateTime(iso: string, locale: string): string {
  return new Date(iso).toLocaleString(locale, {
    year: "numeric",
    month: "2-digit",
    day: "2-digit",
    hour: "2-digit",
    minute: "2-digit",
    hour12: false,
  });
}

/** Relative time like "in 4 min" / "3 h ago". */
export function formatRelative(iso: string, locale: string, now = Date.now()): string {
  const diff = new Date(iso).getTime() - now;
  const abs = Math.abs(diff);
  const rtf = new Intl.RelativeTimeFormat(locale, { numeric: "auto", style: "short" });
  if (abs < 60_000) return rtf.format(Math.round(diff / 1000), "second");
  if (abs < 3_600_000) return rtf.format(Math.round(diff / 60_000), "minute");
  if (abs < 86_400_000) return rtf.format(Math.round(diff / 3_600_000), "hour");
  return rtf.format(Math.round(diff / 86_400_000), "day");
}
