import { useMutation, type UseQueryResult } from "@tanstack/react-query";
import { BookOpen, ChevronRight, Puzzle, ShieldCheck } from "lucide-react";
import { useState, type ReactNode } from "react";
import { toast } from "sonner";
import { Drawer } from "@/components/drawer";
import { EmptyState, ErrorState, LoadingRows, Panel } from "@/components/page";
import { Pill, StatusText, type Tone } from "@/components/status";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Dialog, DialogContent } from "@/components/ui/dialog";
import { Switch } from "@/components/ui/switch";
import { api, unwrap } from "@/lib/api/client";
import { errorMessage } from "@/lib/api/errors";
import type { Extension } from "@/lib/api/types";
import { useDiscardCheck } from "@/lib/use-unsaved-guard";
import { useI18n } from "@/i18n";
import type { MessageKey } from "@/i18n/en";
import { useFeaturesRefresh } from "./queries";
import { SettingsForm } from "@/components/settings-form";

/** How to write and install one; the console cannot install extensions itself. */
const EXTENSIONS_DOC = "https://github.com/initxy/plugrelay/blob/main/docs/plugins.md#external-process-plugins";

const STATUS: Record<Extension["status"], Tone> = { running: "ok", stopped: "neutral", crashed: "bad", needs_approval: "violet" };

/** A host API an extension asks for, in words; unknown ones show their name as code. */
function HostApi({ api: name }: { api: string }) {
  const { t, has } = useI18n();
  const family = name.startsWith("host.kv") ? "kv" : name.replace(/^host\./, "");
  const k = `ext.host.${family}`;
  return has(k) ? <>{t(k)}</> : <code className="font-mono text-xs">{name}</code>;
}

function unapproved(e: Extension): boolean {
  const ok = new Set(e.approved_permissions ?? []);
  return e.permissions.some((p) => !ok.has(p.id));
}

/** The extension switch: turning on asks for approval first when it wants host APIs not yet approved. */
export function useExtensionSwitch() {
  const { t } = useI18n();
  const refresh = useFeaturesRefresh();
  const [asking, setAsking] = useState<Extension | null>(null);
  const patch = useMutation({
    mutationFn: ({ e, enabled, approved }: { e: Extension; enabled: boolean; approved?: string[] }) =>
      unwrap(api.PATCH("/extensions/{id}", { params: { path: { id: e.id } }, body: { enabled, approved_permissions: approved } })),
    onSuccess: (e) => {
      refresh();
      toast.success(t(e.enabled ? "features.enabledToast" : "features.disabledToast", { name: e.title }));
    },
    onError: (err, { e, enabled }) =>
      toast.error(t(enabled ? "features.enableFailed" : "features.disableFailed", { name: e.title }), { description: errorMessage(err) }),
  });
  const toggle = (e: Extension, on: boolean) => {
    if (on && unapproved(e)) setAsking(e);
    else patch.mutate({ e, enabled: on });
  };
  const dialog = (
    <Dialog open={asking !== null} onOpenChange={(o) => !o && setAsking(null)}>
      {asking && (
        <DialogContent
          title={t("ext.approveTitle", { name: asking.title })}
          description={t("ext.approveHint")}
          footer={
            <>
              <Button onClick={() => setAsking(null)}>{t("common.cancel")}</Button>
              <Button
                variant="primary"
                onClick={() => {
                  patch.mutate({ e: asking, enabled: true, approved: asking.permissions.map((p) => p.id) });
                  setAsking(null);
                }}
              >
                <ShieldCheck />
                {t("ext.approveEnable")}
              </Button>
            </>
          }
        >
          <h3 className="text-xs font-semibold">{t("ext.permissions")}</h3>
          <ul className="mt-2 grid gap-1.5">
            {asking.permissions.map((p) => (
              <li key={p.id} className="rounded-md border border-line px-3 py-2 text-sm">
                <HostApi api={p.api} />
                {p.reason && <div className="mt-0.5 text-xs text-ink-3">{t("ext.reason", { reason: p.reason })}</div>}
              </li>
            ))}
          </ul>
          {asking.source === "external" && <p className="mt-4 text-xs text-ink-3">{t("ext.externalHint")}</p>}
        </DialogContent>
      )}
    </Dialog>
  );
  const pending = patch.isPending ? patch.variables.e.id : undefined;
  return { toggle, dialog, pending };
}

function StatusLine({ e }: { e: Extension }) {
  const { t } = useI18n();
  return <StatusText tone={STATUS[e.status]}>{t(`ext.status.${e.status}` as MessageKey)}</StatusText>;
}

function ExtensionRow({ e, onOpen, onSwitch, pending }: { e: Extension; onOpen: () => void; onSwitch: (on: boolean) => void; pending: boolean }) {
  const { t } = useI18n();
  const hint: Partial<Record<Extension["status"], MessageKey>> = { crashed: "ext.crashedHint", needs_approval: "ext.needsApprovalHint" };
  const h = hint[e.status];
  return (
    <li className="flex flex-wrap items-start gap-x-4 gap-y-2 border-b border-line-soft px-4 py-3.5 last:border-0">
      <span className="mt-0.5 grid size-8 shrink-0 place-items-center rounded-md border border-line bg-surface-2 text-ink-3">
        <Puzzle className="size-4" strokeWidth={1.75} aria-hidden />
      </span>
      <div className="min-w-0 flex-1">
        <div className="flex flex-wrap items-center gap-2">
          <button type="button" onClick={onOpen} className="text-base font-semibold hover:underline">
            {e.title}
          </button>
          <Badge tone="outline">{t("ext.version", { v: e.version })}</Badge>
          <Badge>{t(e.source === "external" ? "ext.external" : "ext.builtin")}</Badge>
        </div>
        {e.description && <p className="mt-0.5 text-sm text-ink-2">{e.description}</p>}
        <div className="mt-1.5 flex flex-wrap items-center gap-x-3 gap-y-1 text-xs">
          <StatusLine e={e} />
          {h && <span className="text-ink-3">{t(h)}</span>}
        </div>
      </div>
      <div className="flex items-center gap-2">
        <Button size="sm" variant="ghost" onClick={onOpen} aria-label={t("features.openSettings", { name: e.title })}>
          {t("ext.manage")}
          <ChevronRight aria-hidden />
        </Button>
        <Switch aria-label={t("features.switch", { name: e.title })} checked={e.enabled} disabled={pending} onChange={onSwitch} />
      </div>
    </li>
  );
}

export function ExtensionsTab({
  query,
  onOpen,
  onSwitch,
  pending,
}: {
  query: UseQueryResult<{ items: Extension[] }>;
  onOpen: (id: string) => void;
  onSwitch: (e: Extension, on: boolean) => void;
  pending?: string;
}) {
  const { t } = useI18n();
  if (query.isError) return <ErrorState error={query.error} onRetry={() => void query.refetch()} />;
  if (query.isPending) return <LoadingRows rows={3} />;
  const docs = (
    <Button asChild size="sm">
      <a href={EXTENSIONS_DOC} target="_blank" rel="noreferrer">
        <BookOpen aria-hidden />
        {t("ext.docs")}
      </a>
    </Button>
  );
  if (query.data.items.length === 0) {
    return (
      <div className="rounded-lg border border-dashed border-line-strong bg-surface">
        <EmptyState icon={Puzzle} title={t("ext.emptyTitle")} hint={t("ext.emptyHint")} action={docs} />
      </div>
    );
  }
  return (
    <div className="grid gap-3">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <p className="max-w-[72ch] text-sm text-ink-2">{t("ext.description")}</p>
        {docs}
      </div>
      <ul className="rounded-lg border border-line bg-surface shadow-card">
        {query.data.items.map((e) => (
          <ExtensionRow key={e.id} e={e} onOpen={() => onOpen(e.id)} onSwitch={(on) => onSwitch(e, on)} pending={pending === e.id} />
        ))}
      </ul>
    </div>
  );
}

function Section({ title, children }: { title: ReactNode; children: ReactNode }) {
  return (
    <section className="grid gap-2">
      <h3 className="text-xs font-semibold text-ink-3">{title}</h3>
      {children}
    </section>
  );
}

export function ExtensionDrawer({ ext, onClose, onSwitch }: { ext: Extension | undefined; onClose: () => void; onSwitch: (e: Extension, on: boolean) => void }) {
  const { t } = useI18n();
  const refresh = useFeaturesRefresh();
  const discard = useDiscardCheck();
  const [dirty, setDirty] = useState(false);
  const close = async () => {
    if (await discard(dirty)) onClose();
  };
  const approved = new Set(ext?.approved_permissions ?? []);
  return (
    <Drawer
      open={ext !== undefined}
      onOpenChange={(o) => !o && void close()}
      onRequestClose={() => void close()}
      title={ext?.title ?? ""}
      subtitle={ext && t("ext.version", { v: ext.version })}
      meta={ext && <Pill tone={STATUS[ext.status]} dot>{t(`ext.status.${ext.status}` as MessageKey)}</Pill>}
      actions={ext && <Switch aria-label={t("features.switch", { name: ext.title })} checked={ext.enabled} onChange={(on) => onSwitch(ext, on)} className="mr-2" />}
      size="lg"
    >
      {ext && (
        <div className="grid gap-6 px-5 pt-5">
          {ext.description && <p className="text-sm text-ink-2">{ext.description}</p>}
          {ext.status_detail && (
            <Panel bodyClassName="p-3">
              <p className="text-xs font-medium text-ink-2">{t(ext.status === "crashed" ? "ext.crashedHint" : "ext.statusDetail")}</p>
              <code className="mt-1 block break-words font-mono text-xs text-ink-2">{ext.status_detail}</code>
            </Panel>
          )}
          <Section title={t("ext.permissions")}>
            {ext.permissions.length === 0 ? (
              <p className="text-sm text-ink-3">{t("ext.noPermissions")}</p>
            ) : (
              <ul className="grid gap-1.5">
                {ext.permissions.map((p) => (
                  <li key={p.id} className="flex items-start justify-between gap-3 rounded-md border border-line px-3 py-2 text-sm">
                    <div className="min-w-0">
                      <HostApi api={p.api} />
                      {p.reason && <div className="mt-0.5 text-xs text-ink-3">{t("ext.reason", { reason: p.reason })}</div>}
                    </div>
                    <Pill tone={approved.has(p.id) ? "ok" : "violet"}>{t(approved.has(p.id) ? "ext.approved" : "ext.notApproved")}</Pill>
                  </li>
                ))}
              </ul>
            )}
            {ext.source === "external" && <p className="text-xs text-ink-3">{t("ext.externalHint")}</p>}
          </Section>
          <Section title={t("features.settings")}>
            {ext.settings.length === 0 ? (
              <p className="text-sm text-ink-3">{t("features.noSettings")}</p>
            ) : (
              <SettingsForm
                items={ext.settings}
                layer="global"
                saved={t("features.settingsSaved", { id: ext.title })}
                onDirtyChange={setDirty}
                save={async (settings) => {
                  await unwrap(api.PATCH("/extensions/{id}", { params: { path: { id: ext.id } }, body: { settings: settings[ext.id] ?? {} } }));
                  refresh();
                }}
              />
            )}
          </Section>
        </div>
      )}
    </Drawer>
  );
}
