import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { ArrowUpRight, FileText, Globe, Lock, Network, ShieldCheck, type LucideIcon } from "lucide-react";
import { useMemo, useState } from "react";
import { toast } from "sonner";
import { meQuery, useMe } from "@/app/session";
import { useConfirm } from "@/components/confirm";
import { ErrorState, PageHeader, Panel } from "@/components/page";
import { Pill } from "@/components/status";
import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import { api, unwrap } from "@/lib/api/client";
import { ApiError, errorMessage } from "@/lib/api/errors";
import type { SettingItem } from "@/lib/api/types";
import { cn } from "@/lib/cn";
import { useUnsavedGuard } from "@/lib/use-unsaved-guard";
import { useUrlSearch } from "@/lib/use-url-search";
import { useI18n } from "@/i18n";
import { DEPENDS, groupsOf, SECTIONS, sectionOf, type SectionId } from "./layout";
import { SettingControl } from "./setting-control";

type Values = Record<string, unknown>;
const ICONS: Record<SectionId, LucideIcon> = {
  site: Globe,
  security: ShieldCheck,
  network: Network,
  logs: FileText,
  other: FileText,
};

function SettingRow({
  item,
  value,
  edited,
  error,
  blockedBy,
  onChange,
}: {
  item: SettingItem;
  value: unknown;
  edited: boolean;
  error?: string;
  blockedBy?: string;
  onChange: (v: unknown) => void;
}) {
  const { t, pick } = useI18n();
  const helpId = `s-${item.key}-help`;
  const help = item.help.en ? pick(item.help) : "";
  return (
    <div
      className={cn(
        "grid gap-x-8 gap-y-2.5 px-4 py-4 md:grid-cols-[minmax(0,5fr)_minmax(0,6fr)]",
        blockedBy && "opacity-60",
      )}
    >
      <div className="min-w-0">
        <div className="flex flex-wrap items-center gap-1.5">
          <label htmlFor={`s-${item.key}`} className="text-sm font-medium">
            {pick(item.title)}
          </label>
          {edited && (
            <span className="size-1.5 rounded-full bg-warn" role="img" aria-label={t("unsaved.badge")} />
          )}
          {item.apply === "restart" && !item.pending_restart && <Pill tone="warn">{t("apply.restart")}</Pill>}
          {item.pending_restart && (
            <Pill tone="warn" dot>
              {t("settings.pendingRestart")}
            </Pill>
          )}
          {item.apply === "file" && <Pill icon={<Lock aria-hidden />}>{t("settings.readOnly")}</Pill>}
        </div>
        {help && (
          <p id={helpId} className="mt-1 max-w-[60ch] text-xs leading-[18px] text-ink-3">
            {help}
          </p>
        )}
      </div>
      <div className="min-w-0 self-center">
        <SettingControl
          item={item}
          value={value}
          onChange={onChange}
          invalid={Boolean(error)}
          disabled={Boolean(blockedBy)}
          describedBy={help ? helpId : undefined}
        />
        {blockedBy && <p className="mt-1.5 text-xs text-ink-3">{blockedBy}</p>}
        {error && (
          <p role="alert" className="mt-1.5 text-xs text-bad">
            {error}
          </p>
        )}
      </div>
    </div>
  );
}

/** Where a setting that is not a site setting lives, as a card that links there. */
function ElsewhereCard({
  title,
  body,
  to,
  search,
  action,
}: {
  title: string;
  body: string;
  to: string;
  search?: Record<string, string>;
  action: string;
}) {
  return (
    <Panel bodyClassName="flex flex-wrap items-center gap-4 p-4">
      <div className="min-w-0 flex-1">
        <p className="text-sm font-medium">{title}</p>
        <p className="mt-0.5 text-xs text-ink-3">{body}</p>
      </div>
      <Button asChild size="sm">
        <Link to={to} search={search}>
          {action}
          <ArrowUpRight aria-hidden />
        </Link>
      </Button>
    </Panel>
  );
}

function LoadingSection() {
  return (
    <div className="grid gap-4" aria-busy="true">
      {[3, 2].map((n, i) => (
        <div key={i} className="rounded-lg border border-line bg-surface">
          <Skeleton className="m-4 h-4 w-24" />
          {Array.from({ length: n }, (_, j) => (
            <div key={j} className="grid gap-4 border-t border-line-soft px-4 py-4 md:grid-cols-2">
              <Skeleton className="h-9" />
              <Skeleton className="h-8" />
            </div>
          ))}
        </div>
      ))}
    </div>
  );
}

/** Site settings, by section. Each change applies when saved unless it says it needs a restart. */
export function SettingsPage() {
  const { t } = useI18n();
  const me = useMe();
  const qc = useQueryClient();
  const confirm = useConfirm();
  const q = useQuery({ queryKey: ["settings"], queryFn: () => unwrap(api.GET("/settings")) });
  const [search, setSearch] = useUrlSearch();
  const [edits, setEdits] = useState<Values>({});
  const [errors, setErrors] = useState<Record<string, string>>({});
  const items = useMemo(() => q.data?.items ?? [], [q.data]);
  const dirtyKeys = Object.keys(edits);
  useUnsavedGuard(dirtyKeys.length > 0);

  const present = useMemo(() => new Set(items.map(sectionOf)), [items]);
  const sections = SECTIONS.filter((s) => s.id !== "other" || present.has("other"));
  const current: SectionId = sections.some((s) => s.id === search.section)
    ? (search.section as SectionId)
    : "site";
  const section = SECTIONS.find((s) => s.id === current)!;
  const valueOf = (key: string) => (key in edits ? edits[key] : items.find((i) => i.key === key)?.value);

  const save = useMutation({
    mutationFn: (confirmCurrency: boolean = false) =>
      unwrap(
        api.PATCH("/settings", {
          body: { values: edits, ...(confirmCurrency ? { confirm_currency_change: true } : {}) },
        }),
      ),
    onSuccess: (r) => {
      qc.setQueryData(["settings"], { items: r.items });
      const siteChanged = dirtyKeys.some((k) => k.startsWith("site."));
      setEdits({});
      setErrors({});
      if (siteChanged) void qc.invalidateQueries({ queryKey: meQuery.queryKey });
      if (r.restart_required) toast.warning(t("settings.savedRestart"));
      else toast.success(t("settings.saved"));
    },
    onError: async (e) => {
      // Prepaid balances are amounts in the old currency and are not converted.
      if (e instanceof ApiError && e.code === "balances_exist") {
        if (
          await confirm({
            title: t("settings.currencyTitle"),
            body: t("settings.currencyBody"),
            confirm: t("settings.currencyConfirm"),
            danger: true,
          })
        )
          save.mutate(true);
        return;
      }
      if (e instanceof ApiError) {
        const fields = Object.fromEntries(
          Object.entries(e.fields).map(([k, v]) => [k.replace(/^values\./, ""), v]),
        );
        setErrors(fields);
        const first = items.find((i) => i.key in fields);
        if (first && sectionOf(first) !== current)
          setSearch({ section: sectionOf(first) }, { replace: true });
      }
      toast.error(errorMessage(e));
    },
  });

  const change = (item: SettingItem, v: unknown) => {
    setEdits((prev) => {
      const next = { ...prev };
      const same =
        JSON.stringify(v ?? null) === JSON.stringify(item.value ?? null) || (item.type === "secret" && !v);
      if (same) delete next[item.key];
      else next[item.key] = v;
      return next;
    });
    setErrors((prev) => {
      if (!(item.key in prev)) return prev;
      const next = { ...prev };
      delete next[item.key];
      return next;
    });
  };

  const editedIn = (id: SectionId) => items.some((i) => i.key in edits && sectionOf(i) === id);
  const groups = groupsOf(current, items);
  const ssoOn = me.pages.includes("sso.providers");
  const mcpOn = me.pages.includes("mcp.servers");

  return (
    <>
      <PageHeader title={t("nav.settings")} description={t("settings.description")} />
      <div className="grid gap-6 lg:grid-cols-[200px_minmax(0,1fr)]">
        <nav aria-label={t("settings.sections")} className="lg:sticky lg:top-6 lg:self-start">
          <ul className="-mx-1 flex gap-1 overflow-x-auto px-1 pb-1 lg:mx-0 lg:grid lg:overflow-visible lg:px-0 lg:pb-0">
            {sections.map((s) => {
              const Icon = ICONS[s.id];
              const on = s.id === current;
              return (
                <li key={s.id}>
                  <button
                    type="button"
                    aria-current={on ? "page" : undefined}
                    onClick={() => setSearch({ section: s.id === "site" ? undefined : s.id })}
                    className={cn(
                      "flex h-8 w-full items-center gap-2 whitespace-nowrap rounded-md px-2.5 text-sm transition-colors duration-100",
                      on
                        ? "bg-accent-soft font-medium text-accent-strong"
                        : "text-ink-2 hover:bg-surface-2 hover:text-ink",
                    )}
                  >
                    <Icon className="size-4 shrink-0" strokeWidth={1.75} aria-hidden />
                    {t(s.id === "other" ? "settings.nav.other" : s.label)}
                    {editedIn(s.id) && (
                      <span
                        className="ml-auto size-1.5 rounded-full bg-warn"
                        role="img"
                        aria-label={t("unsaved.badge")}
                      />
                    )}
                  </button>
                </li>
              );
            })}
            {(ssoOn || mcpOn) && (
              <li aria-hidden className="mx-2.5 my-1 hidden border-t border-line lg:block" />
            )}
            {ssoOn && (
              <li>
                <Link
                  to="/sso"
                  className="flex h-8 items-center gap-2 whitespace-nowrap rounded-md px-2.5 text-sm text-ink-2 hover:bg-surface-2 hover:text-ink"
                >
                  {t("nav.sso")}
                  <ArrowUpRight className="ml-auto size-3.5 text-ink-3" aria-hidden />
                </Link>
              </li>
            )}
            {mcpOn && (
              <li>
                <Link
                  to="/mcp"
                  className="flex h-8 items-center gap-2 whitespace-nowrap rounded-md px-2.5 text-sm text-ink-2 hover:bg-surface-2 hover:text-ink"
                >
                  {t("nav.mcp")}
                  <ArrowUpRight className="ml-auto size-3.5 text-ink-3" aria-hidden />
                </Link>
              </li>
            )}
          </ul>
        </nav>
        <div className="min-w-0">
          <div className="mb-4">
            <h2 className="text-lg font-semibold">
              {t(current === "other" ? "settings.nav.other" : section.label)}
            </h2>
            <p className="mt-0.5 text-sm text-ink-2">{t(section.description)}</p>
          </div>
          {q.isError ? (
            <ErrorState error={q.error} onRetry={() => void q.refetch()} />
          ) : q.isPending ? (
            <LoadingSection />
          ) : (
            <div className="grid gap-4 pb-4">
              {current === "logs" && (
                <ElsewhereCard
                  title={t("settings.logs.cardTitle")}
                  body={t("settings.logs.cardBody")}
                  to="/features"
                  search={{ id: "request_log" }}
                  action={t("settings.logs.open")}
                />
              )}
              {current === "security" && (
                <ElsewhereCard
                  title={t("settings.sso.cardTitle")}
                  body={t(ssoOn ? "settings.sso.cardOn" : "settings.sso.cardOff")}
                  to={ssoOn ? "/sso" : "/features"}
                  search={ssoOn ? undefined : { id: "sso" }}
                  action={t(ssoOn ? "settings.sso.manage" : "settings.sso.turnOn")}
                />
              )}
              {groups.map((g) => (
                <section
                  key={g.title}
                  aria-labelledby={`g-${g.title}`}
                  className="rounded-lg border border-line bg-surface shadow-card"
                >
                  <h3
                    id={`g-${g.title}`}
                    className="border-b border-line-soft px-4 py-2.5 text-xs font-semibold text-ink-2"
                  >
                    {t(g.title)}
                  </h3>
                  <div className="divide-y divide-line-soft">
                    {g.items.map((item) => {
                      const dep = DEPENDS[item.key];
                      const parent = dep && items.find((i) => i.key === dep.on);
                      const blocked = dep && parent && !dep.active(valueOf(dep.on));
                      return (
                        <SettingRow
                          key={item.key}
                          item={item}
                          value={valueOf(item.key)}
                          edited={item.key in edits}
                          error={errors[item.key]}
                          blockedBy={blocked ? t(dep.hint) : undefined}
                          onChange={(v) => change(item, v)}
                        />
                      );
                    })}
                  </div>
                </section>
              ))}
            </div>
          )}
          {dirtyKeys.length > 0 && (
            <div className="sticky bottom-3 z-20 mt-2 flex flex-wrap items-center gap-2 rounded-lg border border-line bg-surface/95 px-4 py-2.5 shadow-pop backdrop-blur">
              <span className="mr-auto flex items-center gap-2 text-sm text-warn">
                <span className="size-1.5 rounded-full bg-warn" aria-hidden />
                {t("settings.unsaved", { n: dirtyKeys.length })}
              </span>
              <Button
                onClick={() => {
                  setEdits({});
                  setErrors({});
                }}
              >
                {t("common.revert")}
              </Button>
              <Button variant="primary" onClick={() => save.mutate(false)} disabled={save.isPending}>
                {save.isPending ? t("common.saving") : t("common.save")}
              </Button>
            </div>
          )}
        </div>
      </div>
    </>
  );
}
