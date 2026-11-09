import { useQuery } from "@tanstack/react-query";
import { Panel } from "@/components/page";
import { useMoney } from "@/components/money";
import { api, unwrap } from "@/lib/api/client";
import type { QuotaWindow } from "@/lib/api/types";
import { cn } from "@/lib/cn";
import { formatRelative, formatWhen } from "@/lib/format";
import { useI18n } from "@/i18n";

const NEAR_LIMIT = 0.8;

function windowRatio(w: QuotaWindow): number {
  return w.limit.micro > 0 ? Math.min(1, w.used.micro / w.limit.micro) : 0;
}

/** Every quota window side by side: used / limit and when it resets. Near the limit turns orange. */
export function QuotaWindows() {
  const { t, locale } = useI18n();
  const money = useMoney();
  const q = useQuery({
    queryKey: ["quota"],
    queryFn: () => unwrap(api.GET("/quota")),
  });
  const windows = q.data?.windows ?? [];
  if (q.isSuccess && windows.length === 0) return null;
  return (
    <Panel title={t("overview.quota")} aside={<span className="text-xs text-ink-3">{t("overview.quotaHint")}</span>}>
      <div className="grid gap-4 sm:grid-cols-2 xl:grid-cols-4">
        {windows.map((w) => {
          const r = windowRatio(w);
          const near = r >= NEAR_LIMIT;
          return (
            <div key={w.id}>
              <div className="flex items-baseline justify-between gap-2 text-[13px]">
                <span className="font-medium">{w.label ?? t(`quota.${w.kind}`)}</span>
                {w.scope && <span className="text-xs text-ink-3">{t(`scope.${w.scope}`)}</span>}
              </div>
              <div className="mt-1.5 h-1.5 rounded-full bg-surface-2">
                <div
                  className={cn("h-full rounded-full", near ? "bg-warn" : "bg-accent")}
                  style={{ width: `${Math.max(2, r * 100)}%` }}
                />
              </div>
              <div className="mt-1.5 flex flex-wrap justify-between gap-x-2 text-xs tabular-nums">
                <span className={near ? "text-warn" : "text-ink-2"}>
                  {money(w.used)} / {money(w.limit)}
                </span>
                <span className="text-ink-3" title={w.resets_at ?? undefined}>
                  {w.resets_at
                    ? t("overview.resets", { at: formatWhen(w.resets_at, locale), rel: formatRelative(w.resets_at, locale) })
                    : t("overview.noReset")}
                </span>
              </div>
            </div>
          );
        })}
      </div>
    </Panel>
  );
}
