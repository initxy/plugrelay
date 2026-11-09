import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useNavigate } from "@tanstack/react-router";
import { AlertTriangle, ArrowLeft, ArrowRight, Check, Store, User, Users, type LucideIcon } from "lucide-react";
import { useState } from "react";
import { toast } from "sonner";
import { FormSection, SwitchRow } from "@/components/form-kit";
import { Button } from "@/components/ui/button";
import { Field } from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import { Select } from "@/components/ui/select";
import { meQuery, setupStatusQuery } from "@/app/session";
import { api, unwrap } from "@/lib/api/client";
import { ApiError, errorMessage } from "@/lib/api/errors";
import type { SetupRequest } from "@/lib/api/types";
import { cn } from "@/lib/cn";
import { useForm } from "@/lib/use-form";
import { useI18n } from "@/i18n";
import { AuthLayout } from "../auth/auth-layout";

type Usage = SetupRequest["usage"];

const USAGES: { value: Usage; icon: LucideIcon }[] = [
  { value: "myself", icon: User },
  { value: "team", icon: Users },
  { value: "sell", icon: Store },
];
const STEPS = ["setup.stepAccount", "setup.stepUsage", "setup.stepNetwork"] as const;
const ACCOUNT_FIELDS = new Set(["admin_username", "admin_password", "confirm"]);

interface SetupForm {
  admin_username: string;
  admin_password: string;
  confirm: string;
  usage: Usage;
  listen_public: boolean;
  public_url: string;
  site_name: string;
  currency: string;
}

function validUrl(s: string): boolean {
  try {
    const u = new URL(s);
    return (u.protocol === "http:" || u.protocol === "https:") && u.host !== "";
  } catch {
    return false;
  }
}

/** First run: the admin account, what the relay is for (which only pre-enables features), and how clients reach it. */
export function SetupPage() {
  const { t } = useI18n();
  const qc = useQueryClient();
  const navigate = useNavigate();
  const status = useQuery(setupStatusQuery);
  const [step, setStep] = useState(0);
  const [busy, setBusy] = useState(false);
  const form = useForm<SetupForm>({
    admin_username: "admin",
    admin_password: "",
    confirm: "",
    usage: "team",
    listen_public: false,
    public_url: "",
    site_name: "plugrelay",
    currency: "USD",
  });
  const v = form.values;

  const check = (s: number) => {
    const e: Record<string, string> = {};
    if (s === 0) {
      if (!v.admin_username.trim()) e.admin_username = t("form.required");
      if (v.admin_password.length < 10) e.admin_password = t("setup.passwordShort");
      else if (v.confirm !== v.admin_password) e.confirm = t("setup.passwordMismatch");
    }
    if (s === 2 && v.public_url.trim() && !validUrl(v.public_url.trim())) e.public_url = t("setup.publicUrlBad");
    form.setErrors(e);
    return Object.keys(e).length === 0;
  };

  const finish = async () => {
    if (!check(2)) return;
    setBusy(true);
    try {
      const body: SetupRequest = {
        admin_username: v.admin_username.trim(),
        admin_password: v.admin_password,
        usage: v.usage,
        listen_public: v.listen_public,
        site_name: v.site_name.trim() || undefined,
        currency: v.currency,
        ...(v.public_url.trim() ? { public_url: v.public_url.trim() } : {}),
      };
      const me = await unwrap(api.POST("/setup", { body }));
      qc.setQueryData(meQuery.queryKey, me);
      qc.setQueryData(setupStatusQuery.queryKey, (s) => (s ? { ...s, needs_setup: false } : s));
      toast.success(t("setup.done"));
      await navigate({ to: "/" });
    } catch (err) {
      if (form.absorb(err) && err instanceof ApiError) setStep(Object.keys(err.fields).some((k) => ACCOUNT_FIELDS.has(k)) ? 0 : 2);
      toast.error(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  const next = () => {
    if (!check(step)) return;
    if (step < STEPS.length - 1) setStep(step + 1);
    else void finish();
  };

  return (
    <AuthLayout wide>
      <div className="overflow-hidden rounded-xl border border-line bg-surface shadow-pop">
        <div className="border-b border-line bg-surface-2/40 px-6 pb-4 pt-5">
          <h1 className="text-xl font-semibold tracking-[-0.01em]">{t("setup.title")}</h1>
          <p className="mt-1 text-sm text-ink-2">{t("setup.subtitle")}</p>
          <ol className="mt-5 grid grid-cols-3 gap-2" aria-label={t("setup.progress")}>
            {STEPS.map((s, i) => (
              <li key={s} aria-current={i === step ? "step" : undefined} className="grid gap-1.5">
                <span className={cn("h-1 rounded-full transition-colors", i <= step ? "bg-accent" : "bg-line")} />
                <span className={cn("flex items-center gap-1.5 text-xs", i === step ? "font-medium text-ink" : "text-ink-3")}>
                  {i < step ? <Check className="size-3.5 text-accent" /> : <span className="tabular-nums">{i + 1}</span>}
                  {t(s)}
                </span>
              </li>
            ))}
          </ol>
        </div>

        <form
          className="px-6 py-6"
          onSubmit={(e) => {
            e.preventDefault();
            next();
          }}
        >
          {step === 0 && (
            <div className="grid gap-4 sm:max-w-sm">
              <p className="text-sm text-ink-2">{t("setup.accountIntro")}</p>
              <Field label={t("setup.username")} hint={t("setup.usernameHint")} error={form.errors.admin_username}>
                <Input value={v.admin_username} onChange={(e) => form.set("admin_username", e.target.value)} autoComplete="username" autoFocus />
              </Field>
              <Field label={t("setup.password")} hint={t("setup.passwordHint")} error={form.errors.admin_password}>
                <Input type="password" autoComplete="new-password" value={v.admin_password} onChange={(e) => form.set("admin_password", e.target.value)} />
              </Field>
              <Field label={t("setup.confirm")} error={form.errors.confirm}>
                <Input type="password" autoComplete="new-password" value={v.confirm} onChange={(e) => form.set("confirm", e.target.value)} />
              </Field>
            </div>
          )}

          {step === 1 && (
            <div className="grid gap-3">
              <p className="text-sm text-ink-2">{t("setup.usageIntro")}</p>
              <div role="radiogroup" aria-label={t("setup.stepUsage")} className="grid gap-2.5 sm:grid-cols-3">
                {USAGES.map(({ value, icon: Icon }) => {
                  const on = v.usage === value;
                  return (
                    <button
                      key={value}
                      type="button"
                      role="radio"
                      aria-checked={on}
                      onClick={() => form.set("usage", value)}
                      onKeyDown={(e) => {
                        const d = e.key === "ArrowRight" || e.key === "ArrowDown" ? 1 : e.key === "ArrowLeft" || e.key === "ArrowUp" ? -1 : 0;
                        if (!d) return;
                        e.preventDefault();
                        const i = (USAGES.findIndex((u) => u.value === v.usage) + d + USAGES.length) % USAGES.length;
                        form.set("usage", USAGES[i]!.value);
                        (e.currentTarget.parentElement?.children[i] as HTMLElement | undefined)?.focus();
                      }}
                      tabIndex={on ? 0 : -1}
                      className={cn(
                        "flex flex-col gap-2 rounded-lg border p-4 text-left transition-[border-color,background-color,box-shadow]",
                        on ? "border-accent bg-accent-soft/40 shadow-[0_0_0_3px_var(--ring-soft)]" : "border-line hover:border-line-strong hover:bg-surface-2/50",
                      )}
                    >
                      <div className="flex items-center justify-between">
                        <span className={cn("grid size-8 place-items-center rounded-md", on ? "bg-accent text-accent-ink" : "bg-surface-2 text-ink-2")}>
                          <Icon className="size-4" />
                        </span>
                        <span className={cn("grid size-4 place-items-center rounded-full border", on ? "border-accent bg-accent" : "border-line-strong")}>
                          {on && <Check className="size-3 text-accent-ink" strokeWidth={3} />}
                        </span>
                      </div>
                      <span className="text-sm font-semibold">{t(`setup.usage.${value}`)}</span>
                      <span className="text-xs text-ink-2">{t(`setup.usage.${value}.hint`)}</span>
                    </button>
                  );
                })}
              </div>
              <div className="rounded-lg border border-line bg-surface-2/40 px-4 py-3">
                <div className="text-xs font-semibold text-ink">{t("setup.turnsOn")}</div>
                <ul className="mt-1.5 grid gap-1 text-sm text-ink-2">
                  {(["1", "2", "3"] as const).map((n) => (
                    <li key={n} className="flex gap-2">
                      <Check className="mt-0.5 size-3.5 shrink-0 text-accent" />
                      {t(`setup.usage.${v.usage}.on${n}`)}
                    </li>
                  ))}
                </ul>
              </div>
            </div>
          )}

          {step === 2 && (
            <div>
              <FormSection title={t("setup.reachTitle")} description={status.data?.listen ? t("setup.listenNow", { addr: status.data.listen }) : undefined}>
                <SwitchRow
                  label={t("setup.listenPublic")}
                  description={v.listen_public ? t("setup.listenPublicOn") : t("setup.listenLocal")}
                  checked={v.listen_public}
                  onChange={(c) => form.set("listen_public", c)}
                />
                {v.listen_public && (
                  <div className="flex gap-2 rounded-md border border-warn/40 bg-warn-soft px-3 py-2 text-sm text-ink">
                    <AlertTriangle className="mt-0.5 size-4 shrink-0 text-warn" />
                    {t("setup.listenWarn")}
                  </div>
                )}
                <Field label={t("setup.publicUrl")} hint={t("setup.publicUrlHint")} error={form.errors.public_url} optional>
                  <Input
                    value={v.public_url}
                    onChange={(e) => form.set("public_url", e.target.value)}
                    placeholder="https://relay.example.com"
                    inputMode="url"
                    className="font-mono text-[13px]"
                  />
                </Field>
              </FormSection>
              <FormSection title={t("setup.siteTitle")}>
                <div className="grid gap-4 sm:grid-cols-[1fr_200px]">
                  <Field label={t("setup.siteName")} hint={t("setup.siteNameHint")} error={form.errors.site_name}>
                    <Input value={v.site_name} onChange={(e) => form.set("site_name", e.target.value)} />
                  </Field>
                  <Field label={t("setup.currency")} hint={t("setup.currencyHint")} error={form.errors.currency}>
                    <Select
                      value={v.currency}
                      onChange={(c) => form.set("currency", c)}
                      options={[
                        { value: "USD", label: t("setup.currency.USD") },
                        { value: "CNY", label: t("setup.currency.CNY") },
                        { value: "EUR", label: t("setup.currency.EUR") },
                      ]}
                    />
                  </Field>
                </div>
              </FormSection>
            </div>
          )}

          <div className="mt-6 flex items-center justify-between border-t border-line pt-4">
            <Button type="button" variant="ghost" onClick={() => setStep((s) => s - 1)} disabled={step === 0 || busy}>
              <ArrowLeft />
              {t("common.back")}
            </Button>
            <Button type="submit" variant="primary" size="lg" disabled={busy}>
              {step < STEPS.length - 1 ? (
                <>
                  {t("common.next")}
                  <ArrowRight />
                </>
              ) : busy ? (
                t("common.saving")
              ) : (
                t("setup.finish")
              )}
            </Button>
          </div>
        </form>
      </div>
      <p className="mt-4 text-center text-xs text-ink-3">{t("setup.footer")}</p>
    </AuthLayout>
  );
}
