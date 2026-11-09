import { useQuery } from "@tanstack/react-query";
import { Check } from "lucide-react";
import { useState } from "react";
import { useClientLabel } from "@/components/client-name";
import { FormSection, HelpText, MoneyInput, SwitchRow } from "@/components/form-kit";
import { TagInput } from "@/components/tag-input";
import { Field } from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import { Select } from "@/components/ui/select";
import { globMatch } from "@/features/models/lookup";
import { api, unwrap } from "@/lib/api/client";
import type { KeySettings } from "@/lib/api/types";
import { cn } from "@/lib/cn";
import { useI18n } from "@/i18n";

export interface KeyFormValues {
  name: string;
  /** Who the key belongs to; only an admin creating a key picks this. Empty = the caller. */
  owner: string;
  budgetMonthly: number | null;
  budgetTotal: number | null;
  rpm: string;
  concurrency: string;
  models: string[];
  /** Client ids from `GET /clients`. */
  clients: string[];
  ips: string[];
  expires: string;
  exception: boolean;
}

const toUnits = (micro: number | undefined) => (micro === undefined ? null : micro / 1_000_000);

/** The local calendar day an expiry falls on, for a date input. */
function localDay(iso: string): string {
  const d = new Date(iso);
  const pad = (n: number) => String(n).padStart(2, "0");
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}`;
}

export function toFormValues(name: string, s: KeySettings): KeyFormValues {
  return {
    name,
    owner: "",
    budgetMonthly: toUnits(s.budget_monthly?.micro),
    budgetTotal: toUnits(s.budget_total?.micro),
    rpm: s.rpm ? String(s.rpm) : "",
    concurrency: s.max_concurrency ? String(s.max_concurrency) : "",
    models: s.models ?? [],
    clients: s.clients ?? [],
    ips: s.ip_allowlist ?? [],
    expires: s.expires_at ? localDay(s.expires_at) : "",
    exception: Boolean(s.exception),
  };
}

const IPV4 = /^(25[0-5]|2[0-4]\d|1?\d?\d)(\.(25[0-5]|2[0-4]\d|1?\d?\d)){3}(\/(3[0-2]|[12]?\d))?$/;
const IPV6 = /^[0-9a-f:]*:[0-9a-f:.]*(\/(12[0-8]|1[01]\d|\d?\d))?$/i;
const isIpOrRange = (s: string) => IPV4.test(s) || IPV6.test(s);

/** Form values → API settings, or field errors keyed like the API's (`settings.ip_allowlist.0`). */
export function toSettings(v: KeyFormValues, currency: string): { settings: KeySettings; errors: Record<string, string> } {
  const errors: Record<string, string> = {};
  const settings: KeySettings = {};
  if (!v.name.trim()) errors.name = "required";
  const money = (n: number | null, field: string) => {
    if (n === null) return undefined;
    if (n < 0) {
      errors[`settings.${field}`] = "money";
      return undefined;
    }
    return { micro: Math.round(n * 1_000_000), currency };
  };
  const monthly = money(v.budgetMonthly, "budget_monthly");
  if (monthly) settings.budget_monthly = monthly;
  const total = money(v.budgetTotal, "budget_total");
  if (total) settings.budget_total = total;
  const whole = (text: string, field: "rpm" | "max_concurrency") => {
    if (!text.trim()) return;
    const n = Number(text);
    if (!Number.isInteger(n) || n < 0) errors[`settings.${field}`] = "integer";
    else if (n > 0) settings[field] = n;
  };
  whole(v.rpm, "rpm");
  whole(v.concurrency, "max_concurrency");
  settings.models = v.models;
  settings.clients = v.clients;
  settings.ip_allowlist = v.ips;
  v.ips.forEach((ip, i) => {
    if (!isIpOrRange(ip)) errors[`settings.ip_allowlist.${i}`] = `ip:${ip}`;
  });
  settings.expires_at = v.expires ? new Date(`${v.expires}T23:59:59`).toISOString() : null;
  if (v.exception) settings.exception = true;
  return { settings, errors };
}

/**
 * Every limit a key carries itself. `groupModels` narrows the model suggestions to what the
 * key's group may use (a key can only narrow that further).
 */
export function KeyFormFields({
  values,
  set,
  errors,
  currency,
  admin,
  isNew,
  groupModels,
}: {
  values: KeyFormValues;
  set: <K extends keyof KeyFormValues>(k: K, v: KeyFormValues[K]) => void;
  errors: Record<string, string>;
  currency: string;
  admin: boolean;
  isNew: boolean;
  groupModels?: readonly string[];
}) {
  const { t, locale } = useI18n();
  const catalog = useQuery({ queryKey: ["catalog"], queryFn: () => unwrap(api.GET("/catalog")) });
  const users = useQuery({ queryKey: ["users"], queryFn: () => unwrap(api.GET("/users")), enabled: admin && isNew });
  const known = useQuery({ queryKey: ["clients"], queryFn: () => unwrap(api.GET("/clients")), staleTime: Infinity });
  const clientLabel = useClientLabel();
  // Every client the relay recognises ("other" is no client to allow), plus any the key still lists.
  const clientIds = (known.data?.items ?? []).map((c) => c.id).filter((id) => id !== "other");
  for (const id of values.clients) if (!clientIds.includes(id)) clientIds.push(id);
  const clientNames = new Map((known.data?.items ?? []).map((c) => [c.id, c.name]));
  const msg = (code: string | undefined) => {
    if (!code) return undefined;
    if (code === "required") return t("form.required");
    if (code === "money") return t("form.money");
    if (code === "integer") return t("form.integer");
    if (code.startsWith("ip:")) return t("keys.badIp", { ip: code.slice(3) });
    return code;
  };
  const ipError = Object.entries(errors).find(([k]) => k.startsWith("settings.ip_allowlist"))?.[1];
  const allowed = groupModels?.length ? groupModels : undefined;
  const modelSuggestions = (catalog.data?.items ?? [])
    .filter((m) => !allowed || allowed.some((p) => globMatch(p, m.model)))
    .map((m) => ({ value: m.model, hint: m.kind === "preset" ? t("keys.presetHint") : m.info?.display_name }));
  const [now] = useState(() => Date.now());
  const expired = values.expires !== "" && new Date(`${values.expires}T23:59:59`).getTime() < now;

  return (
    <div>
      <FormSection title={t("keys.sectionBasics")}>
        <div className={cn("grid gap-4", admin && isNew && "sm:grid-cols-2")}>
          <Field label={t("keys.name")} hint={isNew ? t("keys.nameHint") : undefined} error={msg(errors.name)}>
            <Input value={values.name} onChange={(e) => set("name", e.target.value)} placeholder="laptop-claude" autoFocus={isNew} />
          </Field>
          {admin && isNew && (
            <Field label={t("keys.owner")} hint={t("keys.ownerHint")}>
              <Select
                value={values.owner || "__me"}
                onChange={(v) => set("owner", v === "__me" ? "" : v)}
                options={[
                  { value: "__me", label: t("keys.ownerMe") },
                  ...(users.data?.items ?? []).map((u) => ({ value: u.id, label: u.display_name ? `${u.display_name} · ${u.username}` : u.username })),
                ]}
              />
            </Field>
          )}
        </div>
      </FormSection>

      <FormSection title={t("keys.sectionSpend")} description={t("keys.sectionSpendHint")}>
        <div className="grid gap-4 sm:grid-cols-2">
          <Field label={t("keys.budgetMonthly")} hint={t("keys.budgetMonthlyHint")} error={msg(errors["settings.budget_monthly"])}>
            <MoneyInput value={values.budgetMonthly} onChange={(v) => set("budgetMonthly", v)} currency={currency} placeholder={t("keys.unlimited")} />
          </Field>
          <Field label={t("keys.budgetTotal")} hint={t("keys.budgetTotalHint")} error={msg(errors["settings.budget_total"])}>
            <MoneyInput value={values.budgetTotal} onChange={(v) => set("budgetTotal", v)} currency={currency} placeholder={t("keys.unlimited")} />
          </Field>
        </div>
      </FormSection>

      <FormSection title={t("keys.sectionSpeed")} description={t("keys.sectionSpeedHint")}>
        <div className="grid gap-4 sm:grid-cols-2">
          <Field label={t("keys.rpm")} hint={t("keys.inheritHint")} error={msg(errors["settings.rpm"])}>
            <Input inputMode="numeric" value={values.rpm} onChange={(e) => set("rpm", e.target.value)} placeholder={t("keys.followGroup")} className="tabular-nums" />
          </Field>
          <Field label={t("keys.concurrency")} hint={t("keys.inheritHint")} error={msg(errors["settings.max_concurrency"])}>
            <Input
              inputMode="numeric"
              value={values.concurrency}
              onChange={(e) => set("concurrency", e.target.value)}
              placeholder={t("keys.followGroup")}
              className="tabular-nums"
            />
          </Field>
        </div>
      </FormSection>

      <FormSection title={t("keys.sectionWhat")}>
        <Field
          label={t("keys.models")}
          hint={allowed ? t("keys.modelsHintGroup", { models: allowed.join(", ") }) : t("keys.modelsHint")}
          error={msg(errors["settings.models"])}
        >
          <TagInput value={values.models} onChange={(v) => set("models", v)} suggestions={modelSuggestions} placeholder={t("keys.modelsPlaceholder")} mono />
        </Field>
        <fieldset className="grid gap-1.5">
          <legend className="mb-1.5 text-xs font-semibold">{t("keys.clients")}</legend>
          <div className="flex flex-wrap gap-1.5">
            {clientIds.map((id) => {
              const on = values.clients.includes(id);
              return (
                <button
                  key={id}
                  type="button"
                  aria-pressed={on}
                  onClick={() => set("clients", on ? values.clients.filter((x) => x !== id) : [...values.clients, id])}
                  className={cn(
                    "inline-flex h-7 items-center gap-1 rounded-full border px-2.5 text-xs transition-colors",
                    on ? "border-accent/50 bg-accent-soft font-medium text-accent-strong" : "border-line-strong text-ink-2 hover:text-ink",
                  )}
                >
                  {on && <Check className="size-3" />}
                  {clientLabel({ id, name: clientNames.get(id) ?? id })}
                </button>
              );
            })}
          </div>
          <HelpText>{t("keys.clientsHint")}</HelpText>
        </fieldset>
      </FormSection>

      <FormSection title={t("keys.sectionWhere")}>
        <Field label={t("keys.ips")} hint={t("keys.ipsHint")} error={msg(ipError)}>
          <TagInput
            value={values.ips}
            onChange={(v) => set("ips", v)}
            validate={(ip) => (isIpOrRange(ip) ? undefined : t("keys.badIp", { ip }))}
            placeholder="10.0.0.0/8"
            mono
          />
        </Field>
        <Field label={t("keys.expires")} hint={expired ? t("keys.expiresPast") : t("keys.expiresHint")}>
          <Input type="date" value={values.expires} onChange={(e) => set("expires", e.target.value)} className="max-w-48" lang={locale} />
        </Field>
      </FormSection>

      {admin && (
        <FormSection title={t("keys.sectionAdmin")}>
          <SwitchRow label={t("keys.exception")} description={t("keys.exceptionHint")} checked={values.exception} onChange={(c) => set("exception", c)} />
        </FormSection>
      )}
    </div>
  );
}
