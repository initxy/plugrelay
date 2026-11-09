import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { MoreHorizontal, PlugZap, Trash2 } from "lucide-react";
import { useState } from "react";
import { toast } from "sonner";
import { useConfirm } from "@/components/confirm";
import { Drawer } from "@/components/drawer";
import { ErrorState, LoadingRows } from "@/components/page";
import { Pill } from "@/components/status";
import { Button } from "@/components/ui/button";
import { Menu, MenuContent, MenuItem, MenuTrigger } from "@/components/ui/menu";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { api, unwrap } from "@/lib/api/client";
import { errorMessage } from "@/lib/api/errors";
import type { Upstream } from "@/lib/api/types";
import { pickEnum } from "@/lib/url-filters";
import { useForm } from "@/lib/use-form";
import { useDiscardCheck, useUnsavedGuard } from "@/lib/use-unsaved-guard";
import { useUrlSearch } from "@/lib/use-url-search";
import { useI18n } from "@/i18n";
import type { MessageKey } from "@/i18n/en";
import { CredentialsTab } from "./credentials-tab";
import { UPSTREAM_TONE, upstreamState } from "./labels";
import { missingVariables } from "./manifest-variables";
import { useSourceText } from "./upstreams-page";
import { useTestUpstreams } from "./use-upstream-test";
import { useManifests } from "@/lib/api/queries";
import { countChanges, diffDraft, toDraft } from "./upstream-draft";
import { AdvancedTab, ModelsTab, OverviewTab, TABS, type Tab } from "./upstream-tabs";

function useUpstream(id: string) {
  return useQuery({
    queryKey: ["upstream", id],
    queryFn: () => unwrap(api.GET("/upstreams/{id}", { params: { path: { id } } })),
    refetchInterval: 15_000,
  });
}

/** The upstream drawer (`?id=`): overview, credentials, models and advanced settings as tabs (`&tab=`). */
export function UpstreamDrawer({ id, onClose }: { id: string; onClose: () => void }) {
  const { t } = useI18n();
  const q = useUpstream(id);
  if (!q.data)
    return (
      <Drawer open onOpenChange={(o) => !o && onClose()} title={q.isError ? t("upstreams.notFound") : t("common.loading")} size="lg">
        <div className="p-5">{q.isError ? <ErrorState error={q.error} onRetry={() => void q.refetch()} /> : <LoadingRows rows={8} />}</div>
      </Drawer>
    );
  return <DrawerBody key={q.data.id} upstream={q.data} onClose={onClose} />;
}

function DrawerBody({ upstream: u, onClose }: { upstream: Upstream; onClose: () => void }) {
  const { t } = useI18n();
  const qc = useQueryClient();
  const confirm = useConfirm();
  const discard = useDiscardCheck();
  const [search, setSearch] = useUrlSearch();
  const tab: Tab = pickEnum(search.tab, TABS) ?? "overview";
  const source = useSourceText();
  const [base, setBase] = useState(() => toDraft(u));
  const form = useForm(base);
  const d = form.values;
  const test = useTestUpstreams();
  const manifests = useManifests({ enabled: Boolean(u.source.name) });
  const declared = manifests.data?.items.find((m) => m.name === u.source.name)?.variables ?? [];

  // A fresh server copy replaces the draft only while nothing is edited.
  const fresh = toDraft(u);
  if (!form.dirty && JSON.stringify(fresh) !== JSON.stringify(base)) {
    setBase(fresh);
    form.reset(fresh);
  }

  const save = useMutation({
    mutationFn: () => {
      const missing = missingVariables(declared, d.config, t("form.required"));
      if (Object.keys(missing).length) {
        form.setErrors(missing);
        return Promise.reject(new Error(t("form.fixErrors")));
      }
      return unwrap(api.PATCH("/upstreams/{id}", { params: { path: { id: u.id } }, body: diffDraft(base, d) }));
    },
    onSuccess: (next) => {
      qc.setQueryData(["upstream", u.id], next);
      void qc.invalidateQueries({ queryKey: ["upstreams"] });
      const nd = toDraft(next);
      setBase(nd);
      form.reset(nd);
      toast.success(t("upstreams.saved", { name: next.name }));
    },
    onError: (e) => {
      form.absorb(e);
      toast.error(errorMessage(e));
    },
  });

  const remove = useMutation({
    mutationFn: () => unwrap(api.DELETE("/upstreams/{id}", { params: { path: { id: u.id } } })),
    onSuccess: () => {
      toast.success(t("upstreams.deleted", { name: u.name }));
      void qc.invalidateQueries({ queryKey: ["upstreams"] });
      void qc.invalidateQueries({ queryKey: ["models"] });
      onClose();
    },
    onError: (e) => toast.error(errorMessage(e)),
  });

  useUnsavedGuard(form.dirty);
  const close = async () => {
    if (await discard(form.dirty)) onClose();
  };
  const changes = countChanges(base, d);
  const st = upstreamState(u);

  return (
    <Drawer
      open
      onOpenChange={(o) => !o && void close()}
      onRequestClose={() => void close()}
      size="lg"
      title={u.name}
      meta={
        <Pill tone={UPSTREAM_TONE[st]} dot>
          {t(`upstreams.state.${st}` as MessageKey)}
        </Pill>
      }
      subtitle={source(u)}
      actions={
        <>
          <Button size="sm" onClick={() => test.mutate({ ids: [u.id] })} disabled={test.isPending}>
            <PlugZap />
            {test.isPending ? t("upstreams.testing") : t("upstreams.test")}
          </Button>
          <Menu>
            <MenuTrigger asChild>
              <Button size="icon-sm" variant="ghost" aria-label={t("common.more")}>
                <MoreHorizontal />
              </Button>
            </MenuTrigger>
            <MenuContent>
              <MenuItem
                danger
                onSelect={async () => {
                  if (
                    await confirm({
                      title: t("upstreams.deleteTitle", { name: u.name }),
                      body: t("upstreams.deleteBody"),
                      confirm: t("common.delete"),
                      danger: true,
                    })
                  )
                    remove.mutate();
                }}
              >
                <Trash2 />
                {t("upstreams.delete")}
              </MenuItem>
            </MenuContent>
          </Menu>
        </>
      }
      tabs={
        <Tabs value={tab} onValueChange={(v) => setSearch({ tab: v === "overview" ? undefined : v }, { replace: true })}>
          <TabsList>
            <TabsTrigger value="overview">{t("upstreams.tab.overview")}</TabsTrigger>
            <TabsTrigger value="credentials">
              {t("upstreams.tab.credentials")}
              <span className="ml-1.5 tabular-nums text-ink-3">{u.credentials.length}</span>
            </TabsTrigger>
            <TabsTrigger value="models">{t("upstreams.tab.models")}</TabsTrigger>
            <TabsTrigger value="advanced">{t("upstreams.tab.advanced")}</TabsTrigger>
          </TabsList>
        </Tabs>
      }
      footer={
        tab === "credentials" && !form.dirty ? undefined : (
          <>
            {changes > 0 && <span className="mr-auto text-xs text-warn">{t("upstreams.unsaved", { n: changes })}</span>}
            {form.dirty && (
              <Button onClick={() => form.reset(base)} disabled={save.isPending}>
                {t("common.revert")}
              </Button>
            )}
            <Button variant="primary" onClick={() => save.mutate()} disabled={!form.dirty || save.isPending}>
              {save.isPending ? t("common.saving") : t("common.save")}
            </Button>
          </>
        )
      }
    >
      <Tabs value={tab}>
        <TabsContent value="overview" className="px-5 py-4 outline-none">
          <OverviewTab u={u} form={form} onTab={(v) => setSearch({ tab: v }, { replace: true })} />
        </TabsContent>
        <TabsContent value="credentials" className="px-5 py-4 outline-none">
          <CredentialsTab upstream={u} />
        </TabsContent>
        <TabsContent value="models" className="px-5 py-4 outline-none">
          <ModelsTab u={u} syncOn={d.model_sync ?? true} onSync={(v) => form.set("model_sync", v)} />
        </TabsContent>
        <TabsContent value="advanced" className="px-5 py-4 outline-none">
          <AdvancedTab form={form} declared={declared} />
        </TabsContent>
      </Tabs>
    </Drawer>
  );
}

