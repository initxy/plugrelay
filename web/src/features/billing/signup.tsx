import { useMutation, useQuery } from "@tanstack/react-query";
import { Link, useNavigate, useRouterState } from "@tanstack/react-router";
import { useState } from "react";
import { toast } from "sonner";
import { AuthLayout } from "@/features/auth/auth-layout";
import { CaptchaWidget, type CaptchaProvider } from "@/features/auth/captcha-widget";
import { Button } from "@/components/ui/button";
import { Field } from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import { errorMessage } from "@/lib/api/errors";
import { SIGNUP, pluginJSON } from "./api";
import { useI18n } from "@/i18n";

interface SignupConfig {
  enabled: boolean;
  invite_required: boolean;
  allowed_domains: string[] | null;
  min_password: number;
  captcha?: { provider: CaptchaProvider; site_key: string };
}

/** admin.signup's public config; a 404 (the sign-up feature off, or the module not built in) means sign-up is closed. */
export const signupConfigQuery = {
  queryKey: ["signup-config"],
  queryFn: () => pluginJSON<SignupConfig>(SIGNUP, "config"),
  retry: false,
  staleTime: 60_000,
};

/** "Create an account" under the login form, only when sign-up is open. */
export function SignupLink() {
  const { t } = useI18n();
  const q = useQuery(signupConfigQuery);
  if (!q.data?.enabled) return null;
  return (
    <p className="mt-3 text-center text-[13px]">
      <Link to="/signup" className="text-accent hover:underline">
        {t("billing.signup.link")}
      </Link>
    </p>
  );
}

export function SignupPage() {
  const { t } = useI18n();
  const navigate = useNavigate();
  const cfg = useQuery(signupConfigQuery);
  const ref = useRouterState({ select: (st) => (st.location.search as Record<string, unknown>).ref });
  const referral = typeof ref === "string" || typeof ref === "number" ? String(ref).trim() : "";
  const [form, setForm] = useState({ email: "", name: "", password: "", invite: "" });
  const [captchaToken, setCaptchaToken] = useState<string>();
  const [captchaRound, setCaptchaRound] = useState(0);
  const set = (k: keyof typeof form) => (e: { target: { value: string } }) => setForm((f) => ({ ...f, [k]: e.target.value }));
  const register = useMutation({
    mutationFn: () =>
      pluginJSON(SIGNUP, "register", {
        body: {
          email: form.email.trim(),
          name: form.name.trim(),
          password: form.password,
          invite_code: form.invite.trim(),
          ...(referral ? { referral_code: referral } : {}),
          ...(captchaToken ? { captcha_token: captchaToken } : {}),
        },
      }),
    onSettled: () => {
      // A token is good for one attempt.
      setCaptchaToken(undefined);
      setCaptchaRound((n) => n + 1);
    },
    onSuccess: async () => {
      toast.success(t("billing.signup.done"));
      await navigate({ to: "/login" });
    },
  });
  if (cfg.isPending) return <AuthLayout>{null}</AuthLayout>;
  if (!cfg.data?.enabled) {
    return (
      <AuthLayout>
        <div className="rounded-lg border border-line bg-surface p-6 text-[14px]">
          <p>{t("billing.signup.closed")}</p>
          <Link to="/login" className="mt-3 inline-block text-[13px] text-accent hover:underline">
            {t("billing.signup.haveAccount")}
          </Link>
        </div>
      </AuthLayout>
    );
  }
  const c = cfg.data;
  const ok =
    form.email.includes("@") &&
    form.password.length >= c.min_password &&
    (!c.invite_required || form.invite.trim() !== "") &&
    (!c.captcha || !!captchaToken);
  return (
    <AuthLayout>
      <form
        className="rounded-lg border border-line bg-surface p-6 shadow-pop"
        onSubmit={(e) => {
          e.preventDefault();
          if (ok) register.mutate();
        }}
      >
        <h1 className="text-[17px] font-semibold">{t("billing.signup.title")}</h1>
        {referral && <p className="mt-1 text-[13px] text-ink-2">{t("billing.signup.invited")}</p>}
        <div className="mt-5 grid gap-4">
          <Field
            label={t("billing.signup.email")}
            hint={c.allowed_domains?.length ? t("billing.signup.domains", { domains: c.allowed_domains.join(", ") }) : undefined}
          >
            <Input type="email" autoComplete="email" value={form.email} onChange={set("email")} autoFocus />
          </Field>
          <Field label={t("billing.signup.name")}>
            <Input autoComplete="name" value={form.name} onChange={set("name")} />
          </Field>
          <Field label={t("billing.signup.password")} hint={t("billing.signup.passwordHint", { n: c.min_password })}>
            <Input type="password" autoComplete="new-password" value={form.password} onChange={set("password")} />
          </Field>
          {c.invite_required && (
            <Field label={t("billing.signup.invite")}>
              <Input value={form.invite} onChange={set("invite")} autoComplete="off" />
            </Field>
          )}
          {c.captcha && <CaptchaWidget key={captchaRound} provider={c.captcha.provider} siteKey={c.captcha.site_key} onToken={setCaptchaToken} />}
          {register.isError && (
            <p role="alert" className="rounded-md bg-bad-soft px-3 py-2 text-[13px] text-bad">
              {errorMessage(register.error)}
            </p>
          )}
          <Button type="submit" variant="primary" size="lg" disabled={!ok || register.isPending}>
            {t("billing.signup.submit")}
          </Button>
          <Link to="/login" className="text-center text-[13px] text-accent hover:underline">
            {t("billing.signup.haveAccount")}
          </Link>
        </div>
      </form>
    </AuthLayout>
  );
}
