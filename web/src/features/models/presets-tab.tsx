import { useMutation, useQueryClient } from "@tanstack/react-query";
import { createColumnHelper } from "@tanstack/react-table";
import { ArrowDown, ArrowUp, ChevronRight, Layers, MoreHorizontal, Plus, Trash2, X } from "lucide-react";
import { useMemo, useState } from "react";
import { toast } from "sonner";
import { useConfirm } from "@/components/confirm";
import { DataTable } from "@/components/data-table";
import { Drawer } from "@/components/drawer";
import { FormSection } from "@/components/form-kit";
import { EmptyState, ErrorState, LoadingRows } from "@/components/page";
import { Pill } from "@/components/status";
import { Button } from "@/components/ui/button";
import { Field } from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import { Menu, MenuContent, MenuItem, MenuTrigger } from "@/components/ui/menu";
import { Segmented } from "@/components/ui/segmented";
import { Select } from "@/components/ui/select";
import { api, unwrap } from "@/lib/api/client";
import { errorMessage } from "@/lib/api/errors";
import type { Group, Model, Preset, PresetStrategy } from "@/lib/api/types";
import { useForm } from "@/lib/use-form";
import { useDiscardCheck, useUnsavedGuard } from "@/lib/use-unsaved-guard";
import { useDrawerParam } from "@/lib/use-url-search";
import { useI18n } from "@/i18n";
import { groupAllows } from "./lookup";
import { useGroups, useModels, usePresets } from "./queries";

const STRATEGIES: readonly PresetStrategy[] = ["in_order", "cheapest", "fastest"];

/** Groups whose members see the preset: those that may use at least one of its models. */
export function presetAudience(p: Pick<Preset, "models">, groups: readonly Group[]): Group[] {
  return groups.filter((g) => p.models.some((m) => groupAllows(g, m)));
}

function Audience({ p, groups }: { p: Pick<Preset, "models">; groups: Group[] }) {
  const { t } = useI18n();
  const seen = presetAudience(p, groups);
  if (groups.length === 0 || seen.length === groups.length) return <span className="text-ink-2">{t("presets.everyone")}</span>;
  if (seen.length === 0) return <span className="text-warn">{t("presets.noGroup")}</span>;
  return <span className="text-ink-2">{seen.map((g) => g.name).join("、")}</span>;
}

function Chain({ models }: { models: string[] }) {
  return (
    <span className="flex flex-wrap items-center gap-1 py-1.5">
      {models.map((m, i) => (
        <span key={m} className="inline-flex items-center gap-1">
          {i > 0 && <ChevronRight className="size-3 text-ink-3" />}
          <span className="rounded bg-surface-2 px-1.5 py-0.5 font-mono text-xs">{m}</span>
        </span>
      ))}
    </span>
  );
}

const col = createColumnHelper<Preset>();

export function PresetsTab() {
  const { t } = useI18n();
  const drawer = useDrawerParam();
  const presets = usePresets();
  const groups = useGroups();
  const gs = useMemo(() => groups.data?.items ?? [], [groups.data]);
  const columns = useMemo(
    () => [
      col.accessor("name", {
        header: t("presets.col.name"),
        cell: ({ getValue }) => <span className="font-mono text-[13px] font-medium">{getValue()}</span>,
      }),
      col.accessor("models", { header: t("presets.col.models"), cell: ({ getValue }) => <Chain models={getValue()} /> }),
      col.accessor("strategy", { header: t("presets.col.strategy"), cell: ({ getValue }) => <Pill tone="brand">{t(`presets.strategy.${getValue()}`)}</Pill> }),
      col.display({
        id: "audience",
        header: t("presets.col.audience"),
        meta: { className: "hidden lg:table-cell", label: t("presets.col.audience") },
        cell: ({ row }) => <Audience p={row.original} groups={gs} />,
      }),
    ],
    [t, gs],
  );
  return (
    <>
      {presets.isError ? (
        <ErrorState error={presets.error} onRetry={() => void presets.refetch()} />
      ) : (
        <DataTable
          id="presets"
          columns={columns}
          data={presets.data?.items}
          getRowId={(p) => p.name}
          onRowClick={(p) => drawer.open(p.name)}
          selectedId={drawer.id}
          loadingRows={3}
          empty={
            <EmptyState
              icon={Layers}
              title={t("presets.empty")}
              hint={t("presets.emptyHint")}
              action={
                <Button variant="primary" onClick={() => drawer.open("new")}>
                  <Plus />
                  {t("presets.add")}
                </Button>
              }
            />
          }
        />
      )}
      {drawer.id && <PresetDrawer id={drawer.id} onClose={drawer.close} />}
    </>
  );
}

function PresetDrawer({ id, onClose }: { id: string; onClose: () => void }) {
  const { t } = useI18n();
  const presets = usePresets();
  const models = useModels();
  const groups = useGroups();
  const isNew = id === "new";
  const preset = presets.data?.items.find((p) => p.name === id);
  const ready = presets.data && models.data && groups.data;
  if (!ready || (!isNew && !preset))
    return (
      <Drawer open onOpenChange={(o) => !o && onClose()} title={ready ? t("presets.notFound") : t("common.loading")}>
        <div className="p-5">
          {presets.error ? <ErrorState error={presets.error} onRetry={() => void presets.refetch()} /> : ready ? <p className="text-sm text-ink-2">{t("presets.notFoundHint", { id })}</p> : <LoadingRows rows={5} />}
        </div>
      </Drawer>
    );
  return <PresetBody key={id} preset={preset} models={models.data.items} groups={groups.data.items} onClose={onClose} />;
}

function PresetBody({ preset, models, groups, onClose }: { preset: Preset | undefined; models: Model[]; groups: Group[]; onClose: () => void }) {
  const { t } = useI18n();
  const qc = useQueryClient();
  const confirm = useConfirm();
  const discard = useDiscardCheck();
  const isNew = !preset;
  const form = useForm({ name: preset?.name ?? "", strategy: preset?.strategy ?? ("in_order" as PresetStrategy), models: preset?.models ?? ([] as string[]) });
  const d = form.values;
  const [pick, setPick] = useState<string | undefined>();
  const available = models.filter((m) => !d.models.includes(m.name));

  const save = useMutation({
    mutationFn: () => {
      const errs: Record<string, string> = {};
      if (isNew && !d.name.trim()) errs.name = t("form.required");
      if (d.models.length === 0) errs.models = t("presets.needModel");
      if (Object.keys(errs).length) {
        form.setErrors(errs);
        return Promise.reject(new Error(t("form.fixErrors")));
      }
      return isNew
        ? unwrap(api.POST("/presets", { body: { name: d.name.trim(), models: d.models, strategy: d.strategy } }))
        : unwrap(api.PATCH("/presets/{name}", { params: { path: { name: preset.name } }, body: { models: d.models, strategy: d.strategy } }));
    },
    onSuccess: (p) => {
      void qc.invalidateQueries({ queryKey: ["presets"] });
      form.reset({ name: p.name, strategy: p.strategy, models: p.models });
      toast.success(t("presets.saved", { name: p.name }));
      if (isNew) onClose();
    },
    onError: (e) => {
      form.absorb(e);
      toast.error(errorMessage(e));
    },
  });
  const remove = useMutation({
    mutationFn: () => unwrap(api.DELETE("/presets/{name}", { params: { path: { name: preset!.name } } })),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: ["presets"] });
      toast.success(t("presets.deleted", { name: preset!.name }));
      onClose();
    },
    onError: (e) => toast.error(errorMessage(e)),
  });
  useUnsavedGuard(form.dirty);
  const close = async () => {
    if (await discard(form.dirty)) onClose();
  };
  const move = (i: number, by: number) => {
    const next = [...d.models];
    const [m] = next.splice(i, 1);
    next.splice(i + by, 0, m!);
    form.set("models", next);
  };
  const disabled = new Set(models.filter((m) => !m.enabled).map((m) => m.name));
  const audience = presetAudience(d, groups);

  return (
    <Drawer
      open
      onOpenChange={(o) => !o && void close()}
      onRequestClose={() => void close()}
      title={isNew ? t("presets.add") : <span className="font-mono">{preset.name}</span>}
      subtitle={t("presets.subtitle")}
      actions={
        preset && (
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
                  if (await confirm({ title: t("presets.deleteTitle", { name: preset.name }), body: t("presets.deleteBody"), confirm: t("common.delete"), danger: true }))
                    remove.mutate();
                }}
              >
                <Trash2 />
                {t("presets.delete")}
              </MenuItem>
            </MenuContent>
          </Menu>
        )
      }
      footer={
        <>
          {form.dirty && <span className="mr-auto text-xs text-warn">{t("unsaved.badge")}</span>}
          <Button onClick={() => void close()}>{t("common.cancel")}</Button>
          <Button variant="primary" onClick={() => save.mutate()} disabled={save.isPending || (!isNew && !form.dirty)}>
            {save.isPending ? t("common.saving") : isNew ? t("presets.create") : t("common.save")}
          </Button>
        </>
      }
    >
      <div className="px-5 py-4">
        <FormSection title={t("presets.basic")}>
          {isNew && (
            <Field label={t("presets.name")} hint={t("presets.nameHint")} error={form.errors.name}>
              <Input value={d.name} onChange={(e) => form.set("name", e.target.value)} className="font-mono" spellCheck={false} autoFocus />
            </Field>
          )}
          <div className="grid gap-1.5">
            <span className="text-xs font-semibold">{t("presets.col.strategy")}</span>
            <Segmented aria-label={t("presets.col.strategy")} value={d.strategy} onChange={(v) => form.set("strategy", v)} options={STRATEGIES.map((s) => ({ value: s, label: t(`presets.strategy.${s}`) }))} />
            <p className="text-xs text-ink-3">{t(`presets.strategyHint.${d.strategy}`)}</p>
          </div>
        </FormSection>
        <FormSection title={t("presets.col.models")} description={t("presets.modelsHint")}>
          {d.models.length === 0 ? (
            <p className="rounded-lg border border-dashed border-line-strong px-3 py-3 text-sm text-ink-3">{t("presets.noModels")}</p>
          ) : (
            <ol className="divide-y divide-line-soft rounded-lg border border-line">
              {d.models.map((m, i) => (
                <li key={m} className="flex items-center gap-2 px-3 py-2">
                  <span className="w-5 text-right text-xs tabular-nums text-ink-3">{i + 1}</span>
                  <span className="min-w-0 flex-1 truncate font-mono text-[13px]">{m}</span>
                  {disabled.has(m) && <Pill tone="neutral">{t("models.state.manual")}</Pill>}
                  {!models.some((x) => x.name === m) && <Pill tone="warn">{t("presets.unknownModel")}</Pill>}
                  <Button size="icon-sm" variant="ghost" aria-label={t("common.moveUp")} disabled={i === 0} onClick={() => move(i, -1)}>
                    <ArrowUp />
                  </Button>
                  <Button size="icon-sm" variant="ghost" aria-label={t("common.moveDown")} disabled={i === d.models.length - 1} onClick={() => move(i, 1)}>
                    <ArrowDown />
                  </Button>
                  <Button size="icon-sm" variant="ghost" aria-label={t("common.remove")} onClick={() => form.set("models", d.models.filter((x) => x !== m))}>
                    <X />
                  </Button>
                </li>
              ))}
            </ol>
          )}
          <div className="flex gap-2">
            <Select
              aria-label={t("presets.addModel")}
              value={pick}
              onChange={setPick}
              placeholder={t("presets.addModel")}
              options={available.map((m) => ({ value: m.name, label: m.name }))}
              className="flex-1 font-mono"
            />
            <Button
              disabled={!pick}
              onClick={() => {
                if (!pick) return;
                form.set("models", [...d.models, pick]);
                form.setErrors(({ models: _m, ...rest }) => rest);
                setPick(undefined);
              }}
            >
              <Plus />
              {t("common.add")}
            </Button>
          </div>
          {form.errors.models && <p className="text-xs text-bad">{form.errors.models}</p>}
        </FormSection>
        <FormSection title={t("presets.col.audience")} description={t("presets.audienceHint")}>
          {groups.length === 0 ? (
            <p className="text-sm text-ink-2">{t("presets.everyone")}</p>
          ) : (
            <ul className="flex flex-wrap gap-1.5">
              {groups.map((g) => {
                const sees = audience.includes(g);
                return (
                  <li key={g.id}>
                    <Pill tone={sees ? "ok" : "neutral"} dot>
                      {g.name}
                      <span className="font-normal opacity-80">{sees ? t("presets.sees") : t("presets.doesntSee")}</span>
                    </Pill>
                  </li>
                );
              })}
            </ul>
          )}
        </FormSection>
      </div>
    </Drawer>
  );
}
