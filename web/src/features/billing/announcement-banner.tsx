import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { ChevronDown, Megaphone, X } from "lucide-react";
import { useState } from "react";
import { useFeatureOn } from "@/app/session";
import { Markdown } from "@/components/markdown";
import { Button } from "@/components/ui/button";
import { cn } from "@/lib/cn";
import { useI18n } from "@/i18n";
import { announcementsApi, type Announcement } from "./api";

const mineKey = ["billing", "announcements", "mine"];

/** The live announcements for the caller above every page, newest first; each one can be dismissed. */
export function AnnouncementBanner() {
  const on = useFeatureOn("billing");
  const q = useQuery({ queryKey: mineKey, queryFn: () => announcementsApi.mine(), enabled: on, staleTime: 60_000, refetchInterval: 5 * 60_000 });
  const items = q.data?.items ?? [];
  if (!on || items.length === 0) return null;
  return <Banner items={items} />;
}

function Banner({ items }: { items: Announcement[] }) {
  const { t } = useI18n();
  const qc = useQueryClient();
  const [open, setOpen] = useState(false);
  const dismiss = useMutation({
    mutationFn: (id: string) => announcementsApi.dismiss(id),
    onMutate: (id) => {
      qc.setQueryData<{ items: Announcement[] }>(mineKey, (d) => d && { items: d.items.filter((a) => a.id !== id) });
    },
    onError: () => void qc.invalidateQueries({ queryKey: mineKey }),
  });
  const shown = open ? items : items.slice(0, 1);
  return (
    <section aria-label={t("billing.my.announcements")} className="mb-4 overflow-hidden rounded-lg border border-info/25 bg-info-soft">
      <ul className="divide-y divide-info/15">
        {shown.map((a) => (
          <li key={a.id} className="flex items-start gap-3 px-3.5 py-2.5">
            <Megaphone aria-hidden className="mt-0.5 size-4 shrink-0 text-info" />
            <div className="min-w-0 flex-1">
              <p className="text-sm font-semibold text-ink">{a.title}</p>
              {a.body && <Markdown source={a.body} className="mt-0.5 text-ink-2" />}
            </div>
            <Button
              size="icon-sm"
              variant="ghost"
              aria-label={t("billing.my.dismiss", { title: a.title })}
              onClick={() => dismiss.mutate(a.id)}
            >
              <X aria-hidden />
            </Button>
          </li>
        ))}
      </ul>
      {items.length > 1 && (
        <button
          type="button"
          onClick={() => setOpen((v) => !v)}
          aria-expanded={open}
          className="flex w-full items-center justify-center gap-1 border-t border-info/15 py-1 text-xs text-info hover:bg-info/5"
        >
          {open ? t("billing.my.showLess") : t("billing.my.showMore", { n: items.length - 1 })}
          <ChevronDown aria-hidden className={cn("size-3.5 transition-transform", open && "rotate-180")} />
        </button>
      )}
    </section>
  );
}
