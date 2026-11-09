import * as D from "@radix-ui/react-dialog";
import { useQuery } from "@tanstack/react-query";
import { useNavigate } from "@tanstack/react-router";
import { Command } from "cmdk";
import { CornerDownLeft, Search } from "lucide-react";
import { useDeferredValue, useState } from "react";
import { api, unwrap } from "@/lib/api/client";
import type { SearchHit } from "@/lib/api/types";
import { useI18n } from "@/i18n";
import { iconFor } from "./icons";
import type { NavItem } from "./nav";

/** Where a search hit opens: the owning page with its drawer or row selected via the URL. */
function hitTarget(hit: SearchHit): { to: string; search: Record<string, string> } {
  switch (hit.type) {
    case "request":
      return { to: "/logs", search: { id: hit.id } };
    case "key":
      return { to: "/keys", search: { id: hit.id } };
    case "upstream":
      return { to: "/upstreams", search: { id: hit.id } };
    case "user":
      return { to: "/users", search: { id: hit.id } };
    case "model":
      return { to: "/models", search: { id: hit.id } };
    case "feature":
    case "extension":
      return { to: "/features", search: { id: hit.id } };
  }
}

const HIT_ICON: Record<SearchHit["type"], string> = {
  request: "list",
  key: "key",
  upstream: "server",
  user: "users",
  model: "route",
  feature: "features",
  extension: "puzzle",
};

export function CommandPalette({
  open,
  onOpenChange,
  items,
}: {
  open: boolean;
  onOpenChange: (o: boolean) => void;
  items: NavItem[];
}) {
  const { t } = useI18n();
  const navigate = useNavigate();
  const [q, setQ] = useState("");
  const deferred = useDeferredValue(q.trim());
  const { data, isFetching } = useQuery({
    queryKey: ["search", deferred],
    queryFn: () => unwrap(api.GET("/search", { params: { query: { q: deferred, limit: 20 } } })),
    enabled: open && deferred.length > 0,
    staleTime: 10_000,
  });

  const go = (to: string, search?: Record<string, string>) => {
    onOpenChange(false);
    setQ("");
    void navigate({ to, search });
  };

  const itemClass =
    "flex cursor-default items-center gap-2.5 rounded-md px-2.5 py-2 text-sm data-[selected=true]:bg-accent-soft data-[selected=true]:text-accent-strong [&_svg]:size-4 [&_svg]:text-ink-3 data-[selected=true]:[&_svg]:text-accent";

  return (
    <D.Root open={open} onOpenChange={onOpenChange}>
      <D.Portal>
        <D.Overlay className="fixed inset-0 z-40 bg-scrim animate-[fade-in_100ms]" />
        <D.Content className="fixed left-1/2 top-[12vh] z-50 w-[calc(100vw-24px)] max-w-xl -translate-x-1/2 overflow-hidden rounded-xl border border-line bg-surface shadow-pop animate-[pop-in_140ms_var(--ease-out-quart)]">
          <D.Title className="sr-only">{t("shell.search")}</D.Title>
          <D.Description className="sr-only">{t("palette.hint")}</D.Description>
          <Command shouldFilter={false} label={t("shell.search")}>
            <div className="flex items-center gap-2 border-b border-line px-3">
              <Search className="size-4 text-ink-3" />
              <Command.Input
                value={q}
                onValueChange={setQ}
                placeholder={t("palette.placeholder")}
                className="h-11 flex-1 bg-transparent text-[14px] outline-none placeholder:text-ink-3"
              />
              {isFetching && <span className="text-xs text-ink-3">{t("common.loading")}</span>}
            </div>
            <Command.List className="max-h-[50vh] overflow-y-auto p-1.5">
              <Command.Empty className="px-3 py-6 text-center text-[13px] text-ink-3">
                {t("palette.empty")}
              </Command.Empty>
              {(data?.items.length ?? 0) > 0 && (
                <Command.Group heading={t("palette.results")} className="[&_[cmdk-group-heading]]:px-2.5 [&_[cmdk-group-heading]]:py-1.5 [&_[cmdk-group-heading]]:text-xs [&_[cmdk-group-heading]]:text-ink-3">
                  {data?.items.map((hit) => {
                    const Icon = iconFor(HIT_ICON[hit.type]);
                    const target = hitTarget(hit);
                    return (
                      <Command.Item
                        key={`${hit.type}:${hit.id}`}
                        value={`${hit.type}:${hit.id}`}
                        onSelect={() => go(target.to, target.search)}
                        className={itemClass}
                      >
                        <Icon />
                        <span className="min-w-0 flex-1 truncate">
                          {hit.title}
                          {hit.subtitle && <span className="ml-2 text-ink-3">{hit.subtitle}</span>}
                        </span>
                        <span className="text-xs text-ink-3">{t(`search.${hit.type}`)}</span>
                      </Command.Item>
                    );
                  })}
                </Command.Group>
              )}
              <Command.Group heading={t("palette.pages")} className="[&_[cmdk-group-heading]]:px-2.5 [&_[cmdk-group-heading]]:py-1.5 [&_[cmdk-group-heading]]:text-xs [&_[cmdk-group-heading]]:text-ink-3">
                {items
                  .filter((i) => {
                    return !q || t(i.label).toLowerCase().includes(q.toLowerCase());
                  })
                  .map((i) => {
                    const Icon = iconFor(i.icon);
                    return (
                      <Command.Item key={i.id} value={`page:${i.id}`} onSelect={() => go(i.to)} className={itemClass}>
                        <Icon />
                        <span className="flex-1">{t(i.label)}</span>
                        <CornerDownLeft className="opacity-0 [[data-selected=true]_&]:opacity-100" />
                      </Command.Item>
                    );
                  })}
              </Command.Group>
            </Command.List>
          </Command>
        </D.Content>
      </D.Portal>
    </D.Root>
  );
}
