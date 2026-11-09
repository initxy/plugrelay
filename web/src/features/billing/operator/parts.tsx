import { Link } from "@tanstack/react-router";
import { PowerOff } from "lucide-react";
import { Button } from "@/components/ui/button";
import { formatDateTime } from "@/lib/format";
import { useI18n } from "@/i18n";

/** An ISO time as a `datetime-local` value in the browser's zone; "" for none. */
export function toLocalInput(iso: string | undefined): string {
  if (!iso) return "";
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return "";
  const pad = (n: number) => String(n).padStart(2, "0");
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`;
}

/** A `datetime-local` value as an RFC 3339 time; undefined when empty or not a time. */
export function fromLocalInput(v: string): string | undefined {
  if (!v) return undefined;
  const d = new Date(v);
  return Number.isNaN(d.getTime()) ? undefined : d.toISOString().replace(/\.\d{3}Z$/, "Z");
}

/** A search param as a string (the router may parse numbers). */
export function str(v: unknown): string | undefined {
  return typeof v === "string" && v !== "" ? v : typeof v === "number" ? String(v) : undefined;
}

/** "from … until …", "until …", "from …" or "no end". */
export function useWindowText() {
  const { t, locale } = useI18n();
  return (start?: string, end?: string) => {
    if (start && end) return t("billing.op.window.both", { from: formatDateTime(start, locale), until: formatDateTime(end, locale) });
    if (end) return t("billing.op.window.until", { until: formatDateTime(end, locale) });
    if (start) return t("billing.op.window.from", { from: formatDateTime(start, locale) });
    return t("billing.op.window.open");
  };
}

/** A strip saying a part of billing is switched off, with the way to switch it on. */
export function OffBanner({ title, hint }: { title: string; hint: string }) {
  const { t } = useI18n();
  return (
    <div className="mb-4 flex flex-wrap items-center gap-x-3 gap-y-2 rounded-lg border border-warn/40 bg-warn-soft px-4 py-3">
      <PowerOff className="size-4 shrink-0 text-warn" aria-hidden />
      <div className="min-w-0 flex-1">
        <p className="text-sm font-medium">{title}</p>
        <p className="text-xs text-ink-2">{hint}</p>
      </div>
      <Button size="sm" asChild>
        <Link to="/features" search={{ id: "billing" }}>
          {t("billing.op.turnOn")}
        </Link>
      </Button>
    </div>
  );
}

/** Saves text as a file in the browser. */
export function download(text: string, name: string, type = "text/plain") {
  const url = URL.createObjectURL(new Blob([text], { type: `${type};charset=utf-8` }));
  const a = document.createElement("a");
  a.href = url;
  a.download = name;
  document.body.appendChild(a);
  a.click();
  a.remove();
  setTimeout(() => URL.revokeObjectURL(url), 1000);
}

/**
 * One CSV line; fields with commas, quotes or line breaks are quoted, and
 * text a spreadsheet would run as a formula (= + - @ tab CR first) gets a
 * leading quote. Numbers stay numbers.
 */
export function csvRow(cells: (string | number)[]): string {
  return cells
    .map((c) => {
      let s = String(c);
      if (typeof c === "string" && /^[=+\-@\t\r]/.test(s) && !Number.isFinite(Number(s))) s = `'${s}`;
      return /[",\r\n]/.test(s) ? `"${s.replace(/"/g, '""')}"` : s;
    })
    .join(",");
}
