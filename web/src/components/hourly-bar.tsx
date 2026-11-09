import { cn } from "@/lib/cn";
import { formatPercent } from "@/lib/format";
import { useI18n } from "@/i18n";

/** 24 hourly success ratios as a strip; gaps are hours without traffic. */
export function HourlyBar({ hourly, className }: { hourly: readonly (number | null)[]; className?: string }) {
  const { t, locale } = useI18n();
  return (
    <div className={cn("flex h-4 items-stretch gap-px", className)} role="img" aria-label={t("upstreams.hourlyLabel")}>
      {hourly.map((v, i) => (
        <span
          key={i}
          title={v === null ? t("upstreams.noTraffic") : formatPercent(v, locale)}
          className={cn(
            "w-1 flex-1 rounded-[1px]",
            v === null ? "bg-line" : v >= 0.99 ? "bg-ok/80" : v >= 0.95 ? "bg-warn/80" : "bg-bad/85",
          )}
        />
      ))}
    </div>
  );
}
