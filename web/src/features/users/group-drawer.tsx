import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { Trash2, Users } from "lucide-react";
import { useState, type InputHTMLAttributes } from "react";
import { toast } from "sonner";
import { useConfirm } from "@/components/confirm";
import { Drawer } from "@/components/drawer";
import { FormSection, SwitchRow } from "@/components/form-kit";
import { LayerSettingsTab } from "@/components/layer-settings-tab";
import { useMoney } from "@/components/money";
import { EmptyState, ErrorState, LoadingRows } from "@/components/page";
import { Pill } from "@/components/status";
import { TagInput } from "@/components/tag-input";
import { Button } from "@/components/ui/button";
import { Field } from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { api, unwrap } from "@/lib/api/client";
import { errorMessage } from "@/lib/api/errors";
import type { Group } from "@/lib/api/types";
import { formatRelative } from "@/lib/format";
import { useForm } from "@/lib/use-form";
import { useDiscardCheck } from "@/lib/use-unsaved-guard";
import { useI18n } from "@/i18n";
import { userName } from "./user-account";

type Tab = "basics" | "settings" | "members";

/** A multiplier field with a "×" in front; label wiring goes to the input. */
function TimesInput(props: InputHTMLAttributes<HTMLInputElement>) {
  return (
    <div className="relative max-w-32">
      <span aria-hidden className="pointer-events-none absolute inset-y-0 left-2.5 flex items-center text-sm text-ink-3">
        ×
      </span>
      <Input inputMode="decimal" {...props} className="pl-6 tabular-nums" />
    </div>
  );
}

function initialValues(g: Group | undefined) {
  return {
    name: g?.name ?? "",
    description: g?.description ?? "",
    models: g?.models ?? [],
    show_upstream_names: g ? Boolean(g.show_upstream_names) : true,
    log_bodies: Boolean(g?.log_bodies),
    price_multiplier: String(g?.price_multiplier ?? 1),
  };
}

export function useDeleteGroup(onDeleted?: () => void) {
  const { t } = useI18n();
  const qc = useQueryClient();
  const confirm = useConfirm();
  const del = useMutation({
    mutationFn: (g: Group) => unwrap(api.DELETE("/groups/{id}", { params: { path: { id: g.id } } })),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: ["groups"] });
      void qc.invalidateQueries({ queryKey: ["users"] });
      toast.success(t("groups.deleted"));
      onDeleted?.();
    },
    onError: (e) => toast.error(errorMessage(e)),
  });
  return async (g: Group) => {
    const body = g.member_count ? t("groups.deleteBodyMembers", { n: g.member_count }) : t("groups.deleteBody");
    if (await confirm({ title: t("groups.deleteTitle", { name: g.name }), body, confirm: t("common.delete"), danger: true })) del.mutate(g);
  };
}

function Members({ group }: { group: Group }) {
  const { t, locale } = useI18n();
  const money = useMoney();
  const q = useQuery({ queryKey: ["users", { group: group.id }], queryFn: () => unwrap(api.GET("/users", { params: { query: { group_id: group.id } } })) });
  if (q.isError) return <ErrorState error={q.error} onRetry={() => void q.refetch()} />;
  if (q.isPending) return <LoadingRows rows={3} />;
  if (q.data.items.length === 0) return <EmptyState compact icon={Users} title={t("groups.noMembers")} hint={t("groups.noMembersHint")} />;
  return (
    <ul className="divide-y divide-line-soft rounded-lg border border-line">
      {q.data.items.map((u) => (
        <li key={u.id}>
          <Link
            to="/users"
            search={{ id: u.id }}
            className="flex items-center gap-3 px-3.5 py-2.5 outline-none hover:bg-surface-2/60 focus-visible:bg-accent-soft/60"
          >
            <span className="grid size-7 shrink-0 place-items-center rounded-full bg-surface-2 text-xs font-medium text-ink-2">
              {userName(u).slice(0, 1).toUpperCase()}
            </span>
            <div className="min-w-0 flex-1">
              <div className="truncate text-sm font-medium">{userName(u)}</div>
              <div className="truncate text-xs text-ink-3">{u.username}</div>
            </div>
            <div className="text-right text-xs">
              <div className="tabular-nums text-ink">{money(u.month_spend)}</div>
              <div className="text-ink-3">{u.last_seen_at ? formatRelative(u.last_seen_at, locale) : t("keys.never")}</div>
            </div>
          </Link>
        </li>
      ))}
    </ul>
  );
}

/** One group: what its members may use and pay, its limits and options, and who is in it. */
export function GroupDrawer({
  id,
  groups,
  loading,
  onClose,
  onCreated,
}: {
  id: string | undefined;
  groups: Group[] | undefined;
  loading: boolean;
  onClose: () => void;
  onCreated: (g: Group) => void;
}) {
  const { t } = useI18n();
  const qc = useQueryClient();
  const discard = useDiscardCheck();
  const isNew = id === "new";
  const group = isNew ? undefined : groups?.find((g) => g.id === id);
  const catalog = useQuery({ queryKey: ["catalog"], queryFn: () => unwrap(api.GET("/catalog")), enabled: id !== undefined });
  const form = useForm(initialValues(undefined));
  const [tab, setTab] = useState<Tab>("basics");
  const [settingsDirty, setSettingsDirty] = useState(false);
  const stamp = isNew ? "new" : group ? `${group.id}|${JSON.stringify(group)}` : undefined;
  const [seen, setSeen] = useState<string | undefined>();
  if (stamp !== seen) {
    const same = seen?.split("|")[0] === stamp?.split("|")[0];
    setSeen(stamp);
    if (!same) setTab("basics");
    if (!same || !form.dirty) form.reset(initialValues(group));
  }
  const v = form.values;
  const remove = useDeleteGroup(onClose);

  const save = useMutation({
    mutationFn: () => {
      const m = Number(v.price_multiplier.replace(",", "."));
      const errs: Record<string, string> = {};
      if (!v.name.trim()) errs.name = t("form.required");
      if (!Number.isFinite(m) || m <= 0) errs.price_multiplier = t("groups.multiplierBad");
      if (Object.keys(errs).length) {
        form.setErrors(errs);
        return Promise.reject(new Error(t("form.fixErrors")));
      }
      const body = {
        name: v.name.trim(),
        description: v.description.trim() || undefined,
        models: v.models,
        show_upstream_names: v.show_upstream_names,
        log_bodies: v.log_bodies,
        price_multiplier: m,
      };
      return isNew ? unwrap(api.POST("/groups", { body })) : unwrap(api.PATCH("/groups/{id}", { params: { path: { id: group!.id } }, body }));
    },
    onSuccess: (g) => {
      void qc.invalidateQueries({ queryKey: ["groups"] });
      form.reset(form.values);
      toast.success(t("groups.saved", { name: g.name }));
      if (isNew) {
        qc.setQueryData<{ items: Group[] }>(["groups"], (prev) => (prev ? { ...prev, items: [...prev.items, g] } : prev));
        onCreated(g);
      }
    },
    onError: (e) => {
      form.absorb(e);
      toast.error(errorMessage(e));
    },
  });

  const requestClose = async () => {
    if (await discard(form.dirty || settingsDirty)) {
      setSettingsDirty(false);
      onClose();
    }
  };
  const open = id !== undefined;
  const missing = open && !isNew && !group && !loading;
  const mult = Number(v.price_multiplier.replace(",", "."));

  const basics = (
    <form
      className="p-5"
      onSubmit={(e) => {
        e.preventDefault();
        save.mutate();
      }}
    >
      <FormSection title={t("groups.sectionBasics")}>
        <Field label={t("groups.name")} error={form.errors.name}>
          <Input value={v.name} onChange={(e) => form.set("name", e.target.value)} autoFocus={isNew} />
        </Field>
        <Field label={t("groups.descriptionLabel")} optional>
          <Input value={v.description} onChange={(e) => form.set("description", e.target.value)} placeholder={t("groups.descriptionPlaceholder")} />
        </Field>
      </FormSection>
      <FormSection title={t("groups.sectionModels")} description={t("groups.sectionModelsHint")}>
        <Field label={t("groups.models")} hint={v.models.length ? t("groups.modelsHint") : t("groups.modelsAll")} error={form.errors.models}>
          <TagInput
            value={v.models}
            onChange={(m) => form.set("models", m)}
            suggestions={(catalog.data?.items ?? []).map((m) => ({ value: m.model, hint: m.info?.display_name }))}
            placeholder={t("groups.modelsPlaceholder")}
            mono
          />
        </Field>
      </FormSection>
      <FormSection title={t("groups.pricing")}>
        <Field
          label={t("groups.multiplier")}
          hint={Number.isFinite(mult) && mult > 0 && mult !== 1 ? t("groups.multiplierExample", { pct: Math.round(mult * 100) }) : t("groups.multiplierHint")}
          error={form.errors.price_multiplier}
        >
          <TimesInput value={v.price_multiplier} onChange={(e) => form.set("price_multiplier", e.target.value)} />
        </Field>
      </FormSection>
      <FormSection title={t("groups.transparency")}>
        <SwitchRow
          label={t("groups.showUpstreams")}
          description={t("groups.showUpstreamsHint")}
          checked={v.show_upstream_names}
          onChange={(c) => form.set("show_upstream_names", c)}
        />
        <SwitchRow label={t("groups.logBodies")} description={t("groups.logBodiesHint")} checked={v.log_bodies} onChange={(c) => form.set("log_bodies", c)} />
      </FormSection>
    </form>
  );

  const footer = (
    <>
      {group && (
        <Button variant="danger" className="mr-auto" onClick={() => void remove(group)}>
          <Trash2 />
          {t("groups.delete")}
        </Button>
      )}
      {form.dirty && <span className="text-xs text-warn">{t("unsaved.badge")}</span>}
      <Button onClick={() => void requestClose()}>{t("common.cancel")}</Button>
      <Button variant="primary" onClick={() => save.mutate()} disabled={save.isPending || (!isNew && !form.dirty)}>
        {save.isPending ? t("common.saving") : isNew ? t("groups.addSubmit") : t("common.save")}
      </Button>
    </>
  );

  return (
    <Tabs value={tab} onValueChange={(x) => setTab(x as Tab)}>
      <Drawer
        open={open}
        onOpenChange={(o) => !o && void requestClose()}
        onRequestClose={() => void requestClose()}
        size="lg"
        title={isNew ? t("groups.add") : group ? group.name : missing ? t("groups.notFound") : t("common.loading")}
        subtitle={isNew ? t("groups.layerHint") : (group?.description ?? t("groups.layerHint"))}
        meta={group && <Pill tone="neutral">{t("groups.memberCount", { n: group.member_count })}</Pill>}
        tabs={
          group && (
            <TabsList>
              <TabsTrigger value="basics">{t("groups.tabBasics")}</TabsTrigger>
              <TabsTrigger value="settings">{t("groups.tabSettings")}</TabsTrigger>
              <TabsTrigger value="members">{t("groups.tabMembers")}</TabsTrigger>
            </TabsList>
          )
        }
        footer={isNew || tab === "basics" ? footer : undefined}
      >
        {missing ? (
          <EmptyState icon={Users} title={t("groups.notFound")} hint={t("groups.notFoundHint")} />
        ) : isNew ? (
          basics
        ) : group ? (
          <>
            <TabsContent value="basics">{basics}</TabsContent>
            <TabsContent value="settings">
              <LayerSettingsTab layer="group" id={group.id} hint={t("groups.settingsHint")} saved={t("groups.saved", { name: group.name })} onDirty={setSettingsDirty} />
            </TabsContent>
            <TabsContent value="members" className="p-5">
              <Members group={group} />
            </TabsContent>
          </>
        ) : (
          <div className="p-5">
            <LoadingRows rows={6} />
          </div>
        )}
      </Drawer>
    </Tabs>
  );
}
