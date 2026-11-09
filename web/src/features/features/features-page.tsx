import { useMutation, useQuery } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { AlertTriangle, ChevronRight, X } from "lucide-react";
import { useState } from "react";
import { toast } from "sonner";
import { featurePage } from "@/app/nav";
import { Drawer } from "@/components/drawer";
import { ErrorState, PageHeader } from "@/components/page";
import { Pill, StatusText, type Tone } from "@/components/status";
import { Button } from "@/components/ui/button";
import { Segmented } from "@/components/ui/segmented";
import { Skeleton } from "@/components/ui/skeleton";
import { Switch } from "@/components/ui/switch";
import { api, unwrap } from "@/lib/api/client";
import { ApiError, errorMessage } from "@/lib/api/errors";
import type { Feature, Setting } from "@/lib/api/types";
import { cn } from "@/lib/cn";
import { useDiscardCheck } from "@/lib/use-unsaved-guard";
import { useDrawerParam, useUrlSearch } from "@/lib/use-url-search";
import { useI18n } from "@/i18n";
import type { MessageKey } from "@/i18n/en";
import { ExtensionDrawer, ExtensionsTab, useExtensionSwitch } from "./extensions";
import { extensionsQuery, featuresQuery, useFeaturesRefresh } from "./queries";
import { SettingsForm } from "@/components/settings-form";


/** Features for selling access; the rest are everyday ones. */
const SELLING = new Set(["billing", "signup", "public_pages"]);


function useModuleHeading() {
  const { t, has } = useI18n();
  return (module: string) => {
    const k = `features.module.${module}`;
    return has(k) ? t(k) : undefined;
  };
}

function featureState(f: Feature): { tone: Tone; label: MessageKey } {
  if (f.always_on) return { tone: "ok", label: "features.state.always" };
  if (!f.enabled) return { tone: "neutral", label: "features.state.off" };
  if (f.modules.some((m) => m.status === "crashed" || m.status === "missing")) return { tone: "bad", label: "features.state.problem" };
  return { tone: "ok", label: "features.state.on" };
}

/** What a switch error means, in words: the server's own text stays available as detail. */
function SwitchError({ name, error, on, onDismiss }: { name: string; error: unknown; on: boolean; onDismiss: () => void }) {
  const { t } = useI18n();
  const conflict = error instanceof ApiError && error.status === 409;
  return (
    <div role="alert" className="flex gap-2 rounded-md border border-bad/30 bg-bad-soft px-2.5 py-2 text-xs">
      <AlertTriangle className="mt-px size-3.5 shrink-0 text-bad" aria-hidden />
      <div className="min-w-0 flex-1">
        <p className="font-medium text-bad">{t(on ? "features.enableFailed" : "features.disableFailed", { name })}</p>
        {conflict && <p className="mt-0.5 text-ink-2">{t("features.conflictHint")}</p>}
        <details className="mt-1 text-ink-3">
          <summary className="cursor-pointer select-none hover:text-ink-2">{t("features.details")}</summary>
          <code className="mt-1 block break-words font-mono text-[11px] leading-relaxed text-ink-2">{errorMessage(error)}</code>
        </details>
      </div>
      <button type="button" onClick={onDismiss} aria-label={t("common.close")} className="-mr-1 -mt-0.5 grid size-5 place-items-center rounded text-ink-3 hover:text-ink">
        <X className="size-3.5" />
      </button>
    </div>
  );
}

/** "Appears when on": the menu pages a feature adds, or a sentence for features that add none. */
function Adds({ f }: { f: Feature }) {
  const { t, has } = useI18n();
  const pages = f.pages.map(featurePage).filter((p) => p !== undefined);
  const admin = pages.filter((p) => p.audience === "operator");
  const user = pages.filter((p) => p.audience === "user");
  const hint = `features.where.${f.id}`;
  if (pages.length === 0 && !has(hint)) return <p className="text-xs text-ink-3">{t("features.addsNothing")}</p>;
  const row = (label: string | undefined, list: typeof pages) =>
    list.length > 0 && (
      <div className="flex flex-wrap items-center gap-1">
        {label && <span className="mr-0.5 text-ink-3">{label}</span>}
        {list.map((p) =>
          f.enabled && p.audience === "operator" ? (
            <Link key={p.to} to={p.to} className="rounded-full focus-visible:outline-offset-1">
              <Pill tone="brand" className="h-[18px] px-1.5 text-2xs hover:underline">
                {t(p.label)}
              </Pill>
            </Link>
          ) : (
            <Pill key={p.to} className="h-[18px] px-1.5 text-2xs">
              {t(p.label)}
            </Pill>
          ),
        )}
      </div>
    );
  return (
    <div className="grid gap-1 text-xs">
      <span className="text-ink-3">{t(f.enabled || f.always_on ? "features.addsOn" : "features.adds")}</span>
      {row(admin.length && user.length ? t("features.forAdmins") : undefined, admin)}
      {row(admin.length && user.length ? t("features.forMembers") : undefined, user)}
      {has(hint) && <span className="text-ink-2">{t(hint)}</span>}
    </div>
  );
}

const valueOf = (s: Setting | undefined) => (s ? (s.value ?? s.default) : undefined);

/** The routing strategy as a choice right on its card. */
function RoutingChoice({ f }: { f: Feature }) {
  const { t, pick } = useI18n();
  const refresh = useFeaturesRefresh();
  const s = f.settings.find((x) => x.module === "selector.routing" && x.key === "strategy");
  const save = useMutation({
    mutationFn: (strategy: string) =>
      unwrap(api.PATCH("/features/{id}", { params: { path: { id: f.id } }, body: { settings: { "selector.routing": { strategy } } } })),
    onSuccess: (_, strategy) => {
      refresh();
      const named = s?.enum_labels?.[strategy];
      toast.success(t("features.strategySaved", { name: named ? pick(named) : strategy }));
    },
    onError: (e) => toast.error(errorMessage(e)),
  });
  if (!s?.enum?.length) return null;
  const current = String(save.isPending ? save.variables : (valueOf(s) ?? ""));
  return (
    <fieldset className="grid gap-0.5" disabled={save.isPending}>
      <legend className="sr-only">{pick(s.label)}</legend>
      {s.enum.map((o) => {
        const named = s.enum_labels?.[o];
        const on = current === o;
        return (
          <label
            key={o}
            className={cn(
              "flex cursor-pointer items-center gap-2 rounded-md px-1.5 py-1 text-sm transition-colors duration-100 hover:bg-surface-2 has-[:focus-visible]:outline-2 has-[:focus-visible]:outline-ring",
              on ? "font-medium text-ink" : "text-ink-2",
            )}
          >
            <input
              type="radio"
              name={`strategy-${f.id}`}
              value={o}
              checked={on}
              onChange={() => save.mutate(o)}
              className="peer sr-only"
            />
            <span
              aria-hidden
              className={cn("grid size-3.5 shrink-0 place-items-center rounded-full border transition-colors", on ? "border-accent border-[4px]" : "border-line-strong")}
            />
            {named ? pick(named) : o}
            {o === s.default && <span className="text-2xs font-normal text-ink-3">{t("features.recommended")}</span>}
          </label>
        );
      })}
    </fieldset>
  );
}

/** Two facts of the request log on its card, since it has no switch. */
function RequestLogSummary({ f }: { f: Feature }) {
  const { t } = useI18n();
  const bodies = valueOf(f.settings.find((s) => s.key === "body_logging"));
  const days = Number(valueOf(f.settings.find((s) => s.key === "retention_days")) ?? 0);
  return (
    <dl className="grid grid-cols-[auto_1fr] gap-x-3 gap-y-0.5 text-xs">
      <dt className="text-ink-3">{t("features.summary.bodies")}</dt>
      <dd>{t(bodies ? "features.summary.bodiesOn" : "features.summary.bodiesOff")}</dd>
      <dt className="text-ink-3">{t("features.summary.retention")}</dt>
      <dd>{days > 0 ? t("features.summary.days", { n: days }) : t("features.summary.forever")}</dd>
    </dl>
  );
}

function FeatureCard({ f, onOpen }: { f: Feature; onOpen: () => void }) {
  const { t, pick } = useI18n();
  const refresh = useFeaturesRefresh();
  const name = pick(f.name);
  const [failure, setFailure] = useState<{ error: unknown; on: boolean }>();
  const toggle = useMutation({
    mutationFn: (enabled: boolean) => unwrap(api.PATCH("/features/{id}", { params: { path: { id: f.id } }, body: { enabled } })),
    onMutate: () => setFailure(undefined),
    onSuccess: (r) => {
      refresh();
      toast.success(t(r.enabled ? "features.enabledToast" : "features.disabledToast", { name }));
    },
    onError: (error, on) => setFailure({ error, on }),
  });
  const state = featureState(f);
  const checked = toggle.isPending ? toggle.variables : f.enabled;
  const live = f.enabled || f.always_on;
  return (
    <article
      aria-labelledby={`f-${f.id}`}
      className={cn(
        "group flex flex-col gap-2.5 rounded-lg border p-4 transition-[border-color,box-shadow] duration-150",
        live ? "border-line bg-surface shadow-card hover:border-line-strong" : "border-line bg-surface/50 hover:border-line-strong dark:bg-surface/60",
      )}
    >
      <header className="flex items-start gap-3">
        <h3 id={`f-${f.id}`} className="min-w-0 flex-1 text-base font-semibold leading-6">
          {name}
        </h3>
        {!f.always_on && (
          <Switch
            aria-label={t("features.switch", { name })}
            checked={checked}
            disabled={toggle.isPending}
            onChange={(on) => toggle.mutate(on)}
            className="mt-1"
          />
        )}
      </header>
      <p className={cn("text-sm", live ? "text-ink-2" : "text-ink-3")}>{pick(f.description)}</p>
      {f.id === "routing" ? <RoutingChoice f={f} /> : f.id === "request_log" ? <RequestLogSummary f={f} /> : <Adds f={f} />}
      {failure && <SwitchError name={name} error={failure.error} on={failure.on} onDismiss={() => setFailure(undefined)} />}
      <footer className="mt-auto flex items-center justify-between gap-2 border-t border-line-soft pt-2.5 text-xs">
        <StatusText tone={state.tone}>{t(state.label)}</StatusText>
        {f.settings.length > 0 ? (
          <Button size="xs" variant="ghost" onClick={onOpen} className="-mr-1.5 text-ink-2" aria-label={t("features.openSettings", { name })}>
            {t("features.settings")}
            <ChevronRight aria-hidden />
          </Button>
        ) : (
          <span className="text-ink-3">{t("features.noSettings")}</span>
        )}
      </footer>
    </article>
  );
}

function FeatureDrawer({ f, onClose }: { f: Feature | undefined; onClose: () => void }) {
  const { t, pick } = useI18n();
  const refresh = useFeaturesRefresh();
  const heading = useModuleHeading();
  const discard = useDiscardCheck();
  const [dirty, setDirty] = useState(false);
  const close = async () => {
    if (await discard(dirty)) onClose();
  };
  const state = f && featureState(f);
  return (
    <Drawer
      open={f !== undefined}
      onOpenChange={(o) => !o && void close()}
      onRequestClose={() => void close()}
      title={f ? pick(f.name) : ""}
      subtitle={t("features.settingsHint")}
      meta={state && <Pill tone={state.tone === "neutral" ? "neutral" : state.tone} dot>{t(state.label)}</Pill>}
      size="lg"
    >
      {f && (
        <div className="px-5 pt-5">
          <p className="mb-5 text-sm text-ink-2">{pick(f.description)}</p>
          {!f.enabled && !f.always_on && (
            <p className="mb-5 rounded-md border border-line bg-surface-2 px-3 py-2 text-xs text-ink-2">{t("features.offHint")}</p>
          )}
          <SettingsForm
            items={f.settings}
            layer="global"
            saved={t("features.settingsSaved", { id: pick(f.name) })}
            heading={heading}
            onDirtyChange={setDirty}
            save={async (settings) => {
              await unwrap(api.PATCH("/features/{id}", { params: { path: { id: f.id } }, body: { settings } }));
              refresh();
            }}
          />
        </div>
      )}
    </Drawer>
  );
}

function CardsSkeleton() {
  return (
    <div className="grid gap-3 sm:grid-cols-2 xl:grid-cols-3" aria-busy="true">
      {Array.from({ length: 6 }, (_, i) => (
        <div key={i} className="grid gap-3 rounded-lg border border-line bg-surface p-4">
          <Skeleton className="h-5 w-32" />
          <Skeleton className="h-10" />
          <Skeleton className="h-4 w-40" />
        </div>
      ))}
    </div>
  );
}

/** Features (one switch and one form each) and, on their own tab, extensions. */
export function FeaturesPage() {
  const { t } = useI18n();
  const features = useQuery(featuresQuery);
  const extensions = useQuery(extensionsQuery);
  const [search, setSearch] = useUrlSearch();
  const drawer = useDrawerParam();
  const ext = extensions.data?.items.find((e) => e.id === drawer.id);
  const tab = search.tab === "extensions" || ext ? "extensions" : "features";
  const feature = features.data?.items.find((f) => f.id === drawer.id);
  const extSwitch = useExtensionSwitch();

  const reloadError = features.data?.reload_error ?? extensions.data?.reload_error;
  const items = features.data?.items ?? [];
  const groups = [
    { id: "common", label: t("features.group.common"), items: items.filter((f) => !SELLING.has(f.id)) },
    { id: "selling", label: t("features.group.selling"), items: items.filter((f) => SELLING.has(f.id)) },
  ].filter((g) => g.items.length > 0);
  const extCount = extensions.data?.items.length ?? 0;

  return (
    <>
      <PageHeader
        title={t("nav.features")}
        description={t("features.description")}
        actions={
          <Segmented<"features" | "extensions">
            aria-label={t("nav.features")}
            value={tab}
            onChange={(v) => setSearch({ tab: v === "extensions" ? "extensions" : undefined, id: undefined })}
            options={[
              { value: "features", label: t("features.tab.features") },
              {
                value: "extensions",
                label: (
                  <>
                    {t("features.tab.extensions")}
                    {extCount > 0 && <span className="tabular-nums text-ink-3">{extCount}</span>}
                  </>
                ),
              },
            ]}
          />
        }
      />
      {reloadError && (
        <div role="alert" className="mb-5 flex gap-2.5 rounded-lg border border-bad/30 bg-bad-soft px-4 py-3 text-sm">
          <AlertTriangle className="mt-0.5 size-4 shrink-0 text-bad" aria-hidden />
          <div className="min-w-0">
            <p className="font-medium text-bad">{t("features.reloadTitle")}</p>
            <p className="mt-0.5 text-ink-2">{t("features.reloadBody")}</p>
            <code className="mt-1.5 block break-words font-mono text-xs text-ink-2">{reloadError}</code>
          </div>
        </div>
      )}
      {tab === "extensions" ? (
        <ExtensionsTab query={extensions} onOpen={drawer.open} onSwitch={extSwitch.toggle} pending={extSwitch.pending} />
      ) : features.isError ? (
        <ErrorState error={features.error} onRetry={() => void features.refetch()} />
      ) : features.isPending ? (
        <CardsSkeleton />
      ) : (
        <div className="grid gap-7">
          {groups.map((g) => (
            <section key={g.id} aria-labelledby={`fg-${g.id}`}>
              <h2 id={`fg-${g.id}`} className="mb-2.5 text-xs font-semibold tracking-[0.04em] text-ink-3">
                {g.label}
              </h2>
              <div className="grid gap-3 sm:grid-cols-2 xl:grid-cols-3">
                {g.items.map((f) => (
                  <FeatureCard key={f.id} f={f} onOpen={() => drawer.open(f.id)} />
                ))}
              </div>
            </section>
          ))}
        </div>
      )}
      <FeatureDrawer key={feature?.id ?? "none"} f={feature} onClose={drawer.close} />
      <ExtensionDrawer key={ext?.id ?? "none-ext"} ext={ext} onClose={drawer.close} onSwitch={extSwitch.toggle} />
      {extSwitch.dialog}
    </>
  );
}
