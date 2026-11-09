import { useQuery } from "@tanstack/react-query";
import { Check, Eye, EyeOff } from "lucide-react";
import { useEffect, useMemo, useState, type ReactElement, type ReactNode } from "react";
import { toast } from "sonner";
import { useMe } from "@/app/session";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Field } from "@/components/ui/field";
import { Input, Textarea } from "@/components/ui/input";
import { Select } from "@/components/ui/select";
import { Switch } from "@/components/ui/switch";
import { api, unwrap } from "@/lib/api/client";
import { ApiError, errorMessage } from "@/lib/api/errors";
import { useUpstreams } from "@/lib/api/queries";
import type { Setting, SettingScope, SettingValues } from "@/lib/api/types";
import { cn } from "@/lib/cn";
import { formatDurationText, parseDuration } from "@/lib/duration";
import { formatMoney } from "@/lib/money";
import { useI18n } from "@/i18n";
import { ApplyBadge } from "./apply-badge";
import { DurationInput, FormSection, MoneyInput } from "./form-kit";
import { TagInput } from "./tag-input";

/**
 * Where the settings live: `global` is the site-wide value of a feature or an extension; the
 * others are one object's layer, where an empty field follows the layer above.
 */
export type SettingsLayer = SettingScope;
type Values = Record<string, unknown>;

const idOf = (s: Setting) => `${s.module}/${s.key}`;
const INHERIT = "__inherit";
const UNSET = "__unset";
const NO_GROUP = "__none";

const isEmpty = (v: unknown) => v === null || v === undefined || v === "";
/** What a `Field` wraps: it hands the control its id and error wiring. */
type FieldControl = ReactElement<{ id?: string; "aria-invalid"?: boolean; "aria-describedby"?: string }>;

function loadedValues(items: readonly Setting[]): Values {
  const out: Values = {};
  for (const s of items) out[idOf(s)] = s.value ?? null;
  return out;
}

/** Server field paths are settings.<module>.<key>; the form's are <module>/<key>. */
function fieldErrors(err: ApiError): Record<string, string> {
  const out: Record<string, string> = {};
  for (const [path, msg] of Object.entries(err.fields)) {
    const rest = path.replace(/^settings\./, "");
    const i = rest.lastIndexOf(".");
    out[i > 0 ? `${rest.slice(0, i)}/${rest.slice(i + 1)}` : rest] = msg;
  }
  return out;
}

/** Client-side checks of site-wide values; the server re-validates and its 422 wins. */
function useValidate() {
  const { t } = useI18n();
  return (items: readonly Setting[], values: Values): Record<string, string> => {
    const errors: Record<string, string> = {};
    for (const s of items) {
      const v = values[idOf(s)];
      if (isEmpty(v)) continue;
      const numeric = s.type === "int" || s.type === "float";
      if (s.type === "int" && s.unit !== "money" && !Number.isInteger(v)) errors[idOf(s)] = t("form.integer");
      else if (numeric && typeof v !== "number") errors[idOf(s)] = t("form.number");
      else if (s.merge === "strictest" && typeof v === "number" && v < 0) errors[idOf(s)] = t("form.min", { n: 0 });
      else if (s.enum?.length && s.type !== "list" && !s.enum.includes(String(v))) errors[idOf(s)] = t("form.enum");
    }
    return errors;
  };
}

/**
 * Draft state for a list of settings. Settings of features that are off are left out. A
 * site-wide save sends every setting shown, a layer's only the changed ones; `null` removes a
 * value (so the layer above applies again).
 */
export function useSettingsDraft(items: readonly Setting[] | undefined, layer: SettingsLayer, save: (body: SettingValues) => Promise<unknown>) {
  const { t } = useI18n();
  const me = useMe();
  const validate = useValidate();
  const shown = useMemo(() => (items ?? []).filter((s) => !s.feature || me.features.includes(s.feature)), [items, me.features]);
  const loaded = useMemo(() => loadedValues(shown), [shown]);
  const [values, setValues] = useState<Values>(loaded);
  const [seen, setSeen] = useState(loaded);
  const [errors, setErrors] = useState<Record<string, string>>({});
  const [saving, setSaving] = useState(false);
  if (seen !== loaded) {
    setSeen(loaded);
    setValues(loaded);
    setErrors({});
  }
  const changed = shown.filter((s) => JSON.stringify(values[idOf(s)] ?? null) !== JSON.stringify(loaded[idOf(s)] ?? null));
  const submit = async () => {
    if (layer === "global") {
      const local = validate(shown, values);
      if (Object.keys(local).length) {
        setErrors(local);
        throw new Error(t("form.fixErrors"));
      }
    }
    const body: SettingValues = {};
    for (const s of layer === "global" ? shown : changed) (body[s.module] ??= {})[s.key] = values[idOf(s)] ?? null;
    setSaving(true);
    try {
      await save(body);
      setErrors({});
    } catch (e) {
      if (e instanceof ApiError) setErrors(fieldErrors(e));
      throw e;
    } finally {
      setSaving(false);
    }
  };
  return {
    layer,
    items: shown,
    values,
    errors,
    dirty: changed.length > 0,
    saving,
    set: (s: Setting, v: unknown) => setValues((prev) => ({ ...prev, [idOf(s)]: v === undefined || v === "" ? null : v })),
    revert: () => {
      setValues(loaded);
      setErrors({});
    },
    submit,
  };
}

export type SettingsDraft = ReturnType<typeof useSettingsDraft>;

// ---------------------------------------------------------------- pickers

function SecretInput({ value, isSet, onChange, id }: { value: unknown; isSet: boolean; onChange: (v: string | null) => void; id?: string }) {
  const { t } = useI18n();
  const [show, setShow] = useState(false);
  return (
    <div className="relative">
      <Input
        id={id}
        type={show ? "text" : "password"}
        autoComplete="off"
        value={typeof value === "string" ? value : ""}
        placeholder={isSet ? t("form.secretKeep") : t("form.secretEmpty")}
        onChange={(e) => onChange(e.target.value === "" ? null : e.target.value)}
        className="pr-8"
      />
      <button
        type="button"
        className="absolute inset-y-0 right-0 px-2 text-ink-3 hover:text-ink"
        onClick={() => setShow((s) => !s)}
        aria-label={show ? t("form.hide") : t("form.show")}
      >
        {show ? <EyeOff className="size-3.5" /> : <Eye className="size-3.5" />}
      </button>
    </div>
  );
}

function ModelPicker({ value, onChange, id }: { value: unknown; onChange: (v: string) => void; id?: string }) {
  const { t } = useI18n();
  const { data } = useQuery({ queryKey: ["models"], queryFn: () => unwrap(api.GET("/models")) });
  return (
    <Select
      id={id}
      value={typeof value === "string" ? value : undefined}
      onChange={onChange}
      placeholder={t("form.pickModel")}
      options={(data?.items ?? []).map((m) => ({ value: m.name, label: m.name }))}
    />
  );
}

function UpstreamPicker({ value, onChange, id }: { value: unknown; onChange: (v: string) => void; id?: string }) {
  const { t } = useI18n();
  const { data } = useUpstreams();
  return (
    <Select
      id={id}
      value={typeof value === "string" ? value : undefined}
      onChange={onChange}
      placeholder={t("form.pickUpstream")}
      options={(data?.items ?? []).map((u) => ({ value: u.id, label: u.name }))}
    />
  );
}

function GroupPicker({ value, onChange, id }: { value: unknown; onChange: (v: string | undefined) => void; id?: string }) {
  const { t } = useI18n();
  const { data } = useQuery({ queryKey: ["groups"], queryFn: () => unwrap(api.GET("/groups")) });
  return (
    <Select
      id={id}
      value={typeof value === "string" && value !== "" ? value : NO_GROUP}
      onChange={(v) => onChange(v === NO_GROUP ? undefined : v)}
      options={[{ value: NO_GROUP, label: t("form.noGroup") }, ...(data?.items ?? []).map((g) => ({ value: g.id, label: g.name }))]}
    />
  );
}

function numberOrRaw(text: string, integer: boolean): number | string | null {
  if (text.trim() === "") return null;
  const n = Number(text);
  if (!Number.isFinite(n)) return text;
  return integer && !Number.isInteger(n) ? text : n;
}

// ---------------------------------------------------------------- one field

/**
 * What the relay makes of a saved value that does not fit the setting, while the field still shows
 * it: an allowlist allows nothing, a deny list keeps its valid entries, anything else is ignored.
 */
function useInvalidNote(s: Setting, v: unknown): string | undefined {
  const { t } = useI18n();
  if (!s.invalid || JSON.stringify(v ?? null) !== JSON.stringify(s.value ?? null)) return undefined;
  if (s.type === "list" && s.merge === "strictest") return t("form.invalidDenyAll");
  if (s.type === "list" && s.merge === "union") return t("form.invalidPartly");
  return t("form.invalidIgnored");
}

/** A list setting's allowed values, by their labels, as chips that toggle on and off. */
function Choices({ s, value, onChange }: { s: Setting; value: readonly string[]; onChange: (v: string[]) => void }) {
  const { pick } = useI18n();
  const enumLabel = (o: string) => (s.enum_labels?.[o] ? pick(s.enum_labels[o]) : o);
  return (
    <div className="flex flex-wrap gap-1.5">
      {(s.enum ?? []).map((o) => {
        const on = value.includes(o);
        return (
          <button
            key={o}
            type="button"
            role="checkbox"
            aria-checked={on}
            onClick={() => onChange(on ? value.filter((x) => x !== o) : [...value, o])}
            className={cn(
              "inline-flex h-7 items-center gap-1 rounded-full border px-2.5 text-xs transition-colors duration-100",
              on ? "border-accent/40 bg-accent-soft font-medium text-accent-strong" : "border-line bg-surface text-ink-2 hover:border-line-strong hover:text-ink",
            )}
          >
            {on && <Check className="size-3.5" aria-hidden />}
            {enumLabel(o) !== o ? enumLabel(o) : <code className="font-mono text-[11.5px]">{o}</code>}
          </button>
        );
      })}
    </div>
  );
}

/** A group of chips under one label: what `Field` is for a single control. */
function ChoiceField({ label, aside, hint, error, className, children }: { label: string; aside?: ReactNode; hint?: string; error?: string; className?: string; children: ReactNode }) {
  return (
    <fieldset className={cn("grid gap-1.5", className)}>
      <div className="flex items-center justify-between gap-2">
        <legend className="text-xs font-semibold text-ink">{label}</legend>
        {aside}
      </div>
      {children}
      {(error || hint) && <p className={cn("text-xs", error ? "text-bad" : "text-ink-3")}>{error ?? hint}</p>}
    </fieldset>
  );
}

/** A site-wide value: its default shows through; nothing above it to follow or tighten. */
function GlobalField({ s, draft, currency }: { s: Setting; draft: SettingsDraft; currency: string }) {
  const { t, pick } = useI18n();
  const fid = idOf(s);
  const v = draft.values[fid];
  const set = (x: unknown) => draft.set(s, x);
  const label = pick(s.label);
  const hint = pick(s.help) || undefined;
  const invalid = useInvalidNote(s, v);
  const err = draft.errors[fid] ?? invalid;
  const aside = s.restart ? <ApplyBadge apply="restart" /> : undefined;
  const enumLabel = (o: string) => (s.enum_labels?.[o] ? pick(s.enum_labels[o]) : o);
  const field = (control: FieldControl, fieldHint = hint) => (
    <Field label={label} hint={fieldHint} error={err} aside={aside}>
      {control}
    </Field>
  );

  if (s.type === "bool") return field(<Switch checked={typeof v === "boolean" ? v : Boolean(s.default)} onChange={set} aria-label={label} />);
  if (s.enum?.length && s.type !== "list") {
    const noDefault = s.default === undefined || s.default === null;
    return field(
      <Select
        value={!isEmpty(v) ? String(v) : noDefault ? UNSET : String(s.default)}
        onChange={(x) => set(x === UNSET ? null : s.type === "int" || s.type === "float" ? Number(x) : x)}
        className="max-w-80"
        options={[
          ...(noDefault ? [{ value: UNSET, label: t("form.notSet") }] : []),
          ...s.enum.map((o) => ({ value: o, label: o === String(s.default) ? `${enumLabel(o)}${t("form.defaultMark")}` : enumLabel(o) })),
        ]}
      />,
    );
  }
  if (s.type === "list" && s.enum?.length) {
    const list = Array.isArray(v) ? (v as string[]) : Array.isArray(s.default) ? (s.default as string[]) : [];
    return (
      <ChoiceField label={label} aside={aside} hint={hint} error={err}>
        <Choices s={s} value={list} onChange={set} />
      </ChoiceField>
    );
  }
  if (s.unit === "money" && s.type !== "list")
    return field(
      <MoneyInput
        value={typeof v === "number" ? v : null}
        onChange={set}
        currency={currency}
        placeholder={typeof s.default === "number" ? String(s.default) : ""}
        className="max-w-48"
      />,
    );
  if (s.type === "duration") {
    const fallback = typeof s.default === "string" ? parseDuration(s.default) : null;
    const durHint = [hint, typeof v !== "string" && fallback !== null ? t("form.defaultIs", { value: formatDurationText(fallback, t) }) : undefined].filter(Boolean).join(" · ");
    return field(<DurationInput value={typeof v === "string" ? v : null} onChange={set} />, durHint || undefined);
  }
  if (s.type === "secret") return field(<SecretInput value={v} isSet={Boolean(s.secret_set || s.effective?.secret_set)} onChange={set} />);
  if (s.type === "model") return field(<ModelPicker value={v ?? s.default} onChange={set} />);
  if (s.type === "group") return field(<GroupPicker value={v ?? s.default} onChange={set} />);
  if (s.type === "upstream") return field(<UpstreamPicker value={v ?? s.default} onChange={set} />);
  if (s.type === "int" || s.type === "float")
    return field(
      <Input
        inputMode={s.type === "int" ? "numeric" : "decimal"}
        value={isEmpty(v) ? "" : String(v)}
        placeholder={isEmpty(s.default) ? "" : String(s.default)}
        onChange={(e) => set(numberOrRaw(e.target.value, s.type === "int"))}
        className="max-w-48 tabular-nums"
      />,
    );
  if (s.type === "list") {
    const list = Array.isArray(v) ? (v as string[]) : undefined;
    return field(
      <Textarea
        value={list ? list.join("\n") : ""}
        placeholder={Array.isArray(s.default) ? (s.default as string[]).join("\n") : ""}
        onChange={(e) => {
          const items = e.target.value.split("\n").map((x) => x.trim()).filter(Boolean);
          set(items.length ? items : null);
        }}
        className="font-mono text-[12.5px]"
      />,
      hint ?? t("form.onePerLine"),
    );
  }
  return field(<Input value={typeof v === "string" ? v : ""} placeholder={typeof s.default === "string" ? s.default : ""} onChange={(e) => set(e.target.value)} />);
}

/** One object's layer: an empty field follows the layer above, whose value shows as the placeholder. */
function LayerField({ s, draft, currency }: { s: Setting; draft: SettingsDraft; currency: string }) {
  const { t, pick, locale } = useI18n();
  const layer = draft.layer;
  const fid = idOf(s);
  const v = draft.values[fid];
  const invalid = useInvalidNote(s, v);
  const err = draft.errors[fid] ?? invalid;
  const set = (x: unknown) => draft.set(s, x);

  const show = (x: unknown): string => {
    if (isEmpty(x)) return t("layer.unset");
    if (s.type === "bool") return x ? t("layer.on") : t("layer.off");
    if (s.unit === "money" && typeof x === "number") return x === 0 ? t("layer.noLimit") : formatMoney({ micro: Math.round(x * 1e6), currency }, locale);
    const label = (o: string) => (s.enum_labels?.[o] ? pick(s.enum_labels[o]) : o);
    if (s.enum && typeof x === "string") return label(x);
    if (Array.isArray(x)) return x.length ? x.map((o) => label(String(o))).join(", ") : t("layer.none");
    if (typeof x === "number" && x === 0 && s.merge === "strictest") return t("layer.noLimit");
    return String(x);
  };

  const eff = s.effective;
  const fromAbove = eff && eff.source !== layer;
  const inherited = fromAbove ? eff.value : s.default;
  const from = fromAbove && eff.source !== "default" ? t(`scope.${eff.source}`) : t("layer.default");
  const compact = s.type === "bool" || s.type === "int" || s.type === "float" || (Boolean(s.enum) && s.type !== "list");
  const now = (v === null || v === undefined) && fromAbove && eff.source !== "default" ? t("layer.following", { value: show(inherited), source: from }) : undefined;
  const hint = [pick(s.help), now].filter(Boolean).join(" ");
  const aside = s.merge === "strictest" || s.merge === "union" ? <Badge tone="outline">{t("form.tightenOnly")}</Badge> : undefined;
  const field = (control: FieldControl) => (
    <Field label={pick(s.label)} hint={hint} error={err} aside={aside} className={compact ? undefined : "sm:col-span-2"}>
      {control}
    </Field>
  );

  if (s.type === "bool" || (s.enum && s.type !== "list")) {
    const opts =
      s.type === "bool"
        ? [
            { value: "true", label: t("layer.on") },
            { value: "false", label: t("layer.off") },
          ]
        : s.enum!.map((o) => ({ value: o, label: s.enum_labels?.[o] ? pick(s.enum_labels[o]) : o }));
    return field(
      <Select
        value={isEmpty(v) ? INHERIT : String(v)}
        onChange={(x) => set(x === INHERIT ? null : s.type === "bool" ? x === "true" : x)}
        options={[{ value: INHERIT, label: t("layer.inheritOption", { value: show(inherited) }) }, ...opts]}
        className="w-full"
      />,
    );
  }
  if (s.unit === "money")
    return field(<MoneyInput value={typeof v === "number" ? v : null} onChange={set} currency={currency} placeholder={show(inherited)} />);
  if (s.type === "int" || s.type === "float")
    return field(
      <Input
        inputMode={s.type === "int" ? "numeric" : "decimal"}
        value={isEmpty(v) ? "" : String(v)}
        placeholder={show(inherited)}
        onChange={(e) => {
          const raw = e.target.value.trim();
          const n = Number(raw);
          set(raw === "" ? null : Number.isFinite(n) ? n : raw);
        }}
        className="tabular-nums"
      />,
    );
  if (s.type === "list" && s.enum?.length)
    return (
      <ChoiceField label={pick(s.label)} aside={aside} hint={hint || undefined} error={err} className="sm:col-span-2">
        <Choices s={s} value={Array.isArray(v) ? (v as string[]) : []} onChange={(x) => set(x.length ? x : null)} />
      </ChoiceField>
    );
  if (s.type === "list")
    return field(
      <TagInput
        value={Array.isArray(v) ? (v as string[]) : []}
        onChange={(x) => set(x.length ? x : null)}
        mono
      />,
    );
  if (s.type === "json")
    return field(
      <Textarea
        value={isEmpty(v) ? "" : typeof v === "string" ? v : JSON.stringify(v, null, 2)}
        onChange={(e) => set(e.target.value)}
        className="font-mono text-[12.5px]"
      />,
    );
  return field(
    <Input
      type={s.type === "secret" ? "password" : "text"}
      autoComplete="off"
      value={typeof v === "string" ? v : ""}
      placeholder={s.type === "secret" ? (s.secret_set ? t("form.secretKeep") : t("form.secretEmpty")) : show(inherited)}
      onChange={(e) => set(e.target.value)}
      className={s.type === "duration" ? "max-w-48 font-mono" : undefined}
    />,
  );
}

// ---------------------------------------------------------------- sections

/** Section title of each feature that has settings on a layer, in the feature list's order. */
function useSectionOf() {
  const { pick, t } = useI18n();
  const me = useMe();
  const admin = me.permissions.includes("features.manage");
  const features = useQuery({ queryKey: ["features"], queryFn: () => unwrap(api.GET("/features")), enabled: admin, staleTime: 60_000 });
  const extensions = useQuery({ queryKey: ["extensions"], queryFn: () => unwrap(api.GET("/extensions")), enabled: admin, staleTime: 60_000 });
  return (s: Setting): { id: string; title: string; order: number } => {
    if (s.feature) {
      const list = features.data?.items ?? [];
      const i = list.findIndex((f) => f.id === s.feature);
      const f = list[i];
      return { id: `f:${s.feature}`, title: f ? pick(f.name) : t("layer.otherFeature"), order: i < 0 ? 50 : 1 + i };
    }
    const ext = extensions.data?.items.find((x) => x.id === s.module);
    if (ext) return { id: `x:${ext.id}`, title: ext.title, order: 100 };
    return { id: "core", title: t("layer.access"), order: 0 };
  };
}

/** A layer's fields, grouped by the feature that adds them and titled with its name. */
function LayerSections({ draft, currency }: { draft: SettingsDraft; currency: string }) {
  const sectionOf = useSectionOf();
  const sections = useMemo(() => {
    const map = new Map<string, { title: string; order: number; items: Setting[] }>();
    for (const s of draft.items) {
      const sec = sectionOf(s);
      const hit = map.get(sec.id) ?? { title: sec.title, order: sec.order, items: [] };
      hit.items.push(s);
      map.set(sec.id, hit);
    }
    return [...map.entries()].sort((a, b) => a[1].order - b[1].order);
  }, [draft.items, sectionOf]);
  return (
    <div>
      {sections.map(([id, sec]) => (
        <FormSection key={id} title={sec.title}>
          <div className="grid gap-x-5 gap-y-4 sm:grid-cols-2">
            {sec.items.map((s) => (
              <LayerField key={idOf(s)} s={s} draft={draft} currency={currency} />
            ))}
          </div>
        </FormSection>
      ))}
    </div>
  );
}

/** Site-wide fields in the order given, cut into runs of the same module under `heading`. */
function GlobalSections({ draft, currency, heading }: { draft: SettingsDraft; currency: string; heading?: (module: string) => ReactNode }) {
  const runs: { module: string; items: Setting[] }[] = [];
  for (const s of draft.items) {
    const last = runs.at(-1);
    if (last?.module === s.module) last.items.push(s);
    else runs.push({ module: s.module, items: [s] });
  }
  return (
    <div className="grid gap-6">
      {runs.map((g, i) => {
        const h = heading?.(g.module);
        return (
          <section key={`${g.module}-${i}`} className="grid gap-3">
            {h && <h3 className="text-xs font-semibold text-ink-3">{h}</h3>}
            <div className="grid gap-4">
              {g.items.map((s) => (
                <GlobalField key={idOf(s)} s={s} draft={draft} currency={currency} />
              ))}
            </div>
          </section>
        );
      })}
    </div>
  );
}

/**
 * The one form for a list of settings, site-wide (`layer="global"`) or one object's layer, with a
 * save bar stuck to the bottom of the drawer it sits in. `heading` names a module's run of
 * site-wide fields (return nothing for no heading); `saved` is the toast after a save.
 */
export function SettingsForm({
  items,
  layer,
  save,
  saved,
  heading,
  empty = null,
  intro,
  className,
  onDirtyChange,
}: {
  items: readonly Setting[];
  layer: SettingsLayer;
  save: (body: SettingValues) => Promise<unknown>;
  saved: string;
  heading?: (module: string) => ReactNode;
  /** Shown instead when no setting is left to show. */
  empty?: ReactNode;
  /** Above the fields, e.g. what this layer is for. */
  intro?: ReactNode;
  className?: string;
  onDirtyChange?: (dirty: boolean) => void;
}) {
  const { t } = useI18n();
  const currency = useMe().site.currency;
  const draft = useSettingsDraft(items, layer, save);
  const dirty = draft.dirty;
  useEffect(() => onDirtyChange?.(dirty), [dirty, onDirtyChange]);
  if (draft.items.length === 0) return empty;
  return (
    <div className={className}>
      {intro}
      {layer === "global" ? <GlobalSections draft={draft} currency={currency} heading={heading} /> : <LayerSections draft={draft} currency={currency} />}
      <div className="sticky bottom-0 -mx-5 mt-6 flex items-center justify-end gap-2 border-t border-line bg-surface px-5 py-3">
        {dirty && <span className="mr-auto text-xs text-warn">{t("unsaved.badge")}</span>}
        <Button size="sm" onClick={draft.revert} disabled={!dirty}>
          {t("common.revert")}
        </Button>
        <Button
          size="sm"
          variant="primary"
          disabled={!dirty || draft.saving}
          onClick={() =>
            void draft.submit().then(
              () => toast.success(saved),
              (e) => toast.error(errorMessage(e)),
            )
          }
        >
          {draft.saving ? t("common.saving") : t("common.save")}
        </Button>
      </div>
    </div>
  );
}
