import { useMutation, useQueryClient } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { ArrowDown, MoreHorizontal, Plus, Trash2 } from "lucide-react";
import { useId, useMemo, useState } from "react";
import { toast } from "sonner";
import { useConfirm } from "@/components/confirm";
import { DetailList, Drawer } from "@/components/drawer";
import { FormSection, SwitchRow } from "@/components/form-kit";
import { ErrorState, LoadingRows } from "@/components/page";
import { Pill } from "@/components/status";
import { Button } from "@/components/ui/button";
import { Field } from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import { Menu, MenuContent, MenuItem, MenuTrigger } from "@/components/ui/menu";
import { Select } from "@/components/ui/select";
import { api, unwrap } from "@/lib/api/client";
import { errorMessage } from "@/lib/api/errors";
import type { Group, Model, Route, Upstream } from "@/lib/api/types";
import { cn } from "@/lib/cn";
import { formatCompact } from "@/lib/format";
import { formatPrice } from "@/lib/money";
import { useForm } from "@/lib/use-form";
import { useDiscardCheck, useUnsavedGuard } from "@/lib/use-unsaved-guard";
import { useI18n } from "@/i18n";
import { priceFor } from "./lookup";
import { modelState, useModelStateText } from "./models-tab";
import { useGroups, useModels, usePricing } from "./queries";
import { RouteBar, routeShares } from "./route-bars";
import { useUpstreams } from "@/lib/api/queries";

interface Draft {
  name: string;
  enabled: boolean;
  routes: Route[];
}

/** The model drawer (`?id=<model>` or `?id=new`). */
export function ModelDrawer({ id, onClose }: { id: string; onClose: () => void }) {
  const { t } = useI18n();
  const models = useModels();
  const upstreams = useUpstreams();
  const groups = useGroups();
  const isNew = id === "new";
  const model = models.data?.items.find((m) => m.id === id);
  const ready = models.data && upstreams.data && groups.data;
  if (!ready || (!isNew && !model)) {
    const err = models.error ?? upstreams.error ?? groups.error;
    const missing = ready && !model;
    return (
      <Drawer open onOpenChange={(o) => !o && onClose()} title={missing ? t("models.notFound") : t("common.loading")} size="lg">
        <div className="p-5">
          {err ? <ErrorState error={err} onRetry={() => void models.refetch()} /> : missing ? <p className="text-sm text-ink-2">{t("models.notFoundHint", { id })}</p> : <LoadingRows rows={6} />}
        </div>
      </Drawer>
    );
  }
  return <Body key={id} model={model} upstreams={upstreams.data.items} groups={groups.data.items} onClose={onClose} />;
}

function Body({ model, upstreams, groups, onClose }: { model: Model | undefined; upstreams: Upstream[]; groups: Group[]; onClose: () => void }) {
  const { t, locale } = useI18n();
  const qc = useQueryClient();
  const confirm = useConfirm();
  const discard = useDiscardCheck();
  const stateText = useModelStateText();
  const pricing = usePricing();
  const models = useModels();
  const isNew = !model;
  const form = useForm<Draft>({ name: model?.name ?? "", enabled: model?.enabled ?? true, routes: model?.routes ?? [] });
  const d = form.values;
  const listId = useId();

  // Upstream model names already routed on each upstream: suggestions for a new route.
  const known = useMemo(() => {
    const out = new Map<string, Set<string>>();
    for (const m of models.data?.items ?? [])
      for (const r of m.routes) {
        if (!out.has(r.upstream_id)) out.set(r.upstream_id, new Set());
        out.get(r.upstream_id)!.add(r.upstream_model);
      }
    return out;
  }, [models.data]);

  const save = useMutation({
    mutationFn: () => {
      const errs: Record<string, string> = {};
      if (isNew && !d.name.trim()) errs.name = t("form.required");
      d.routes.forEach((r, i) => {
        if (!r.upstream_id) errs[`routes.${i}.upstream_id`] = t("form.required");
        if (!r.upstream_model.trim()) errs[`routes.${i}.upstream_model`] = t("form.required");
      });
      if (Object.keys(errs).length) {
        form.setErrors(errs);
        return Promise.reject(new Error(t("form.fixErrors")));
      }
      const routes = d.routes.map((r) => ({ ...r, upstream_model: r.upstream_model.trim(), groups: r.groups?.length ? r.groups : [] }));
      return isNew
        ? unwrap(api.POST("/models", { body: { name: d.name.trim(), enabled: d.enabled, routes } }))
        : unwrap(api.PATCH("/models/{id}", { params: { path: { id: model.id } }, body: { enabled: d.enabled, routes } }));
    },
    onSuccess: (m) => {
      void qc.invalidateQueries({ queryKey: ["models"] });
      form.reset({ name: m.name, enabled: m.enabled, routes: m.routes });
      toast.success(t("models.saved", { name: m.name }));
      if (isNew) onClose();
    },
    onError: (e) => {
      form.absorb(e);
      toast.error(errorMessage(e));
    },
  });
  const remove = useMutation({
    mutationFn: () => unwrap(api.DELETE("/models/{id}", { params: { path: { id: model!.id } } })),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: ["models"] });
      toast.success(t("models.deleted", { name: model!.name }));
      onClose();
    },
    onError: (e) => toast.error(errorMessage(e)),
  });

  useUnsavedGuard(form.dirty);
  const close = async () => {
    if (await discard(form.dirty)) onClose();
  };
  const setRoute = (i: number, patch: Partial<Route>) => {
    form.set(
      "routes",
      d.routes.map((r, j) => (j === i ? { ...r, ...patch } : r)),
    );
    form.setErrors(({ [`routes.${i}.upstream_id`]: _a, [`routes.${i}.upstream_model`]: _b, ...rest }) => rest);
  };
  const shares = routeShares(d.routes.map((r, i) => ({ ...r, i })));
  const shareOf = (i: number) => shares.find((s) => s.route.i === i)!;
  const rule = priceFor(pricing.data?.items ?? [], model?.name ?? d.name);
  const info = model?.info;
  const st = model && modelState(model);

  return (
    <Drawer
      open
      onOpenChange={(o) => !o && void close()}
      onRequestClose={() => void close()}
      size="lg"
      title={isNew ? t("models.add") : <span className="font-mono">{model.name}</span>}
      meta={model && st && <Pill tone={st.tone} dot>{stateText(model)}</Pill>}
      subtitle={info?.display_name && info.display_name !== model?.name ? info.display_name : undefined}
      actions={
        model && (
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
                  if (await confirm({ title: t("models.deleteTitle", { name: model.name }), body: t("models.deleteBody"), confirm: t("common.delete"), danger: true }))
                    remove.mutate();
                }}
              >
                <Trash2 />
                {t("models.delete")}
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
            {save.isPending ? t("common.saving") : isNew ? t("models.create") : t("common.save")}
          </Button>
        </>
      }
    >
      <div className="px-5 py-4">
        <FormSection title={t("models.basic")}>
          {isNew && (
            <Field label={t("models.name")} hint={t("models.nameHint")} error={form.errors.name}>
              <Input value={d.name} onChange={(e) => form.set("name", e.target.value)} className="font-mono" spellCheck={false} autoFocus />
            </Field>
          )}
          <SwitchRow label={t("models.enabled")} description={t("models.enabledHint")} checked={d.enabled} onChange={(v) => form.set("enabled", v)} />
        </FormSection>
        <FormSection
          title={t("models.routes")}
          description={t("models.routesHint")}
          aside={
            <Button
              size="sm"
              variant="ghost"
              onClick={() => form.set("routes", [...d.routes, { upstream_id: "", upstream_model: d.name || model?.name || "", priority: d.routes.length ? Math.min(...d.routes.map((r) => r.priority)) : 10, weight: 1, groups: [] }])}
            >
              <Plus />
              {t("models.addRoute")}
            </Button>
          }
        >
          {d.routes.length === 0 ? (
            <p className="rounded-lg border border-dashed border-warn/50 bg-warn-soft/40 px-3 py-3 text-sm text-warn">{t("models.noRoutesHint")}</p>
          ) : (
            <ol className="grid gap-2">
              {d.routes.map((r, i) => {
                const s = shareOf(i);
                const suggestions = [...(known.get(r.upstream_id) ?? [])];
                return (
                  <li key={i} className="grid gap-3 rounded-lg border border-line p-3">
                    <div className="grid items-start gap-3 sm:grid-cols-2">
                      <Field label={t("models.upstream")} error={form.errors[`routes.${i}.upstream_id`]}>
                        <Select
                          value={r.upstream_id || undefined}
                          onChange={(v) => setRoute(i, { upstream_id: v })}
                          placeholder={t("form.pickUpstream")}
                          options={upstreams.map((u) => ({ value: u.id, label: u.enabled ? u.name : `${u.name} (${t("upstreams.state.off")})` }))}
                        />
                      </Field>
                      <Field label={t("models.upstreamModel")} error={form.errors[`routes.${i}.upstream_model`]}>
                        <Input value={r.upstream_model} onChange={(e) => setRoute(i, { upstream_model: e.target.value })} list={`${listId}-${i}`} className="font-mono text-[12.5px]" spellCheck={false} />
                      </Field>
                    </div>
                    <datalist id={`${listId}-${i}`}>
                      {suggestions.map((m) => (
                        <option key={m} value={m} />
                      ))}
                    </datalist>
                    <div className="grid items-end gap-3 sm:grid-cols-[96px_96px_1fr]">
                      <Field label={t("models.priority")} error={form.errors[`routes.${i}.priority`]}>
                        <Input inputMode="numeric" value={String(r.priority)} onChange={(e) => setRoute(i, { priority: Number(e.target.value.replace(/[^\d-]/g, "")) || 0 })} className="tabular-nums" />
                      </Field>
                      <Field label={t("models.weight")} error={form.errors[`routes.${i}.weight`]}>
                        <Input inputMode="numeric" value={String(r.weight)} onChange={(e) => setRoute(i, { weight: Number(e.target.value.replace(/\D/g, "")) || 0 })} className="tabular-nums" />
                      </Field>
                      <div className="flex h-8 items-center justify-between gap-2">
                        <RouteBar tier={s.tier} share={s.share} />
                        <Button size="icon-sm" variant="ghost" aria-label={t("common.remove")} onClick={() => form.set("routes", d.routes.filter((_, j) => j !== i))}>
                          <Trash2 />
                        </Button>
                      </div>
                    </div>
                    <GroupPicker groups={groups} value={r.groups ?? []} onChange={(g) => setRoute(i, { groups: g })} />
                  </li>
                );
              })}
            </ol>
          )}
          {d.routes.length > 1 && <RoutePreview routes={d.routes} upstreams={upstreams} />}
          {form.errors.routes && <p className="text-xs text-bad">{form.errors.routes}</p>}
        </FormSection>
        {model && (
          <FormSection
            title={t("models.about")}
            aside={
              <Link to="/models" search={{ tab: "pricing", id: rule?.id ?? "new" }} className="text-xs text-accent hover:underline">
                {rule ? t("models.editPrice") : t("models.addPrice")}
              </Link>
            }
          >
            <DetailList
              items={[
                {
                  label: t("models.price"),
                  value: rule ? (
                    <span className="tabular-nums">
                      {t("models.pricePair", { input: formatPrice(rule.prices.input, rule.prices.currency, locale), output: formatPrice(rule.prices.output, rule.prices.currency, locale) })}
                      {rule.match !== model.name && <span className="ml-1.5 font-mono text-xs text-ink-3">({rule.match})</span>}
                    </span>
                  ) : (
                    <span className="text-ink-3">{t("models.noPrice")}</span>
                  ),
                },
                ...(info?.context_window ? [{ label: t("models.contextWindow"), value: formatCompact(info.context_window, locale) }] : []),
                ...(info?.max_output ? [{ label: t("models.maxOutput"), value: formatCompact(info.max_output, locale) }] : []),
                ...(info
                  ? [
                      {
                        label: t("models.abilities"),
                        value: (
                          <span className="flex flex-wrap gap-1">
                            {info.tools && <Pill>{t("models.ability.tools")}</Pill>}
                            {info.vision && <Pill>{t("models.ability.vision")}</Pill>}
                            {info.reasoning && <Pill>{t("models.ability.reasoning")}</Pill>}
                            {!info.tools && !info.vision && !info.reasoning && <span className="text-ink-3">—</span>}
                          </span>
                        ),
                      },
                    ]
                  : []),
                {
                  label: t("models.col.synced"),
                  value: model.synced_at ? new Date(model.synced_at).toLocaleString(locale) : t("models.byHand"),
                },
              ]}
            />
          </FormSection>
        )}
      </div>
    </Drawer>
  );
}

/** Which groups may use a route; none picked = everyone. */
function GroupPicker({ groups, value, onChange }: { groups: Group[]; value: string[]; onChange: (v: string[]) => void }) {
  const { t } = useI18n();
  if (groups.length === 0) return null;
  return (
    <div className="flex flex-wrap items-center gap-1.5">
      <span className="mr-1 text-xs text-ink-3">{value.length ? t("models.onlyFor") : t("models.forEveryone")}</span>
      {groups.map((g) => {
        const on = value.includes(g.id);
        return (
          <button
            key={g.id}
            type="button"
            aria-pressed={on}
            onClick={() => onChange(on ? value.filter((x) => x !== g.id) : [...value, g.id])}
            className={cn(
              "inline-flex h-6 items-center rounded-full border px-2.5 text-xs transition-colors",
              on ? "border-accent/40 bg-accent-soft font-medium text-accent-strong" : "border-line text-ink-2 hover:border-line-strong",
            )}
          >
            {g.name}
          </button>
        );
      })}
    </div>
  );
}

/** How a new session is placed: first choice split by weight, then each fallback in turn. */
function RoutePreview({ routes, upstreams }: { routes: Route[]; upstreams: Upstream[] }) {
  const { t } = useI18n();
  const shares = routeShares(routes);
  const tiers = [...new Set(shares.map((s) => s.tier))];
  const name = (id: string) => upstreams.find((u) => u.id === id)?.name ?? (id || "?");
  const [open, setOpen] = useState(true);
  return (
    <div className="rounded-lg bg-surface-2/60 px-3 py-2.5">
      <button type="button" className="text-xs font-medium text-ink-2" onClick={() => setOpen((v) => !v)} aria-expanded={open}>
        {t("models.preview")}
      </button>
      {open && (
        <ol className="mt-2 grid gap-1.5">
          {tiers.map((tier, k) => (
            <li key={tier}>
              {k > 0 && <ArrowDown className="mb-1 ml-1 size-3 text-ink-3" />}
              <div className="flex h-5 overflow-hidden rounded">
                {shares
                  .filter((s) => s.tier === tier)
                  .map((s, i) => (
                    <div
                      key={i}
                      className={cn("flex min-w-0 items-center px-1.5 text-2xs font-medium text-white", tier === 0 ? "bg-accent" : "bg-slate", i > 0 && "border-l border-white/40")}
                      style={{ width: `${Math.max(8, s.share * 100)}%` }}
                      title={`${name(s.route.upstream_id)} · ${Math.round(s.share * 100)}%`}
                    >
                      <span className="truncate">{name(s.route.upstream_id)}</span>
                    </div>
                  ))}
              </div>
            </li>
          ))}
        </ol>
      )}
    </div>
  );
}
