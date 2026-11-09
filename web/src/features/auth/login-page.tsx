import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, useNavigate, useSearch } from "@tanstack/react-router";
import { AlertCircle, Fingerprint } from "lucide-react";
import { useEffect, useState, type FormEvent } from "react";
import { PasswordInput } from "@/components/form-kit";
import { Button } from "@/components/ui/button";
import { Field } from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import { meQuery } from "@/app/session";
import { api, unwrap } from "@/lib/api/client";
import { ApiError, errorMessage } from "@/lib/api/errors";
import type { ApiErrorBody, Me, MfaChallenge, MfaMethod } from "@/lib/api/types";
import { useI18n } from "@/i18n";
import { AuthCard, AuthLayout } from "./auth-layout";
import { CaptchaWidget } from "./captcha-widget";
import { MfaStep } from "./mfa-step";
import { authProvidersQuery, SsoButtons, useSsoErrorText } from "./sso-buttons";
import { getPasskey, passkeyCancelled, passkeysSupported } from "./webauthn";
import { SignupLink } from "@/features/billing/signup";

function Banner({ children }: { children: string }) {
  return (
    <div role="alert" className="mb-5 flex gap-2 rounded-lg border border-bad/25 bg-bad-soft px-3 py-2.5 text-sm text-bad">
      <AlertCircle className="mt-0.5 size-4 shrink-0" aria-hidden />
      <span>{children}</span>
    </div>
  );
}

export function LoginPage() {
  const { t } = useI18n();
  const qc = useQueryClient();
  const navigate = useNavigate();
  const { next, sso_error, mfa } = useSearch({ strict: false }) as { next?: string; sso_error?: string; mfa?: string };
  const methods = useQuery(authProvidersQuery);
  const ssoError = useSsoErrorText(sso_error);
  const passwordOn = methods.data?.password_login ?? true;
  const providers = methods.data?.providers ?? [];
  const captcha = methods.data?.captcha;
  const brand = methods.data?.site;
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [error, setError] = useState<string>();
  const [notice, setNotice] = useState<string>();
  const [busy, setBusy] = useState(false);
  const [second, setSecond] = useState<{ methods: MfaMethod[]; returnTo?: string }>();
  const [failures, setFailures] = useState(0);
  const [captchaAsked, setCaptchaAsked] = useState(false);
  const [captchaToken, setCaptchaToken] = useState<string>();
  const [captchaRound, setCaptchaRound] = useState(0);
  const showCaptcha = !!captcha && (captchaAsked || failures >= captcha.after_failures);

  // Single sign-on lands here with ?mfa=1 when the account has a second step.
  useEffect(() => {
    if (mfa !== "1") return;
    let gone = false;
    unwrap(api.GET("/auth/mfa"))
      .then((p) => !gone && setSecond({ methods: p.methods, returnTo: p.return_to }))
      .catch(() => !gone && setNotice(t("mfa.expired")));
    return () => {
      gone = true;
    };
  }, [mfa, t]);

  const finish = async (me: Me, to?: string) => {
    qc.setQueryData(meQuery.queryKey, me);
    await navigate({ to: next ?? to ?? "/" });
  };

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError(undefined);
    setNotice(undefined);
    try {
      const { data, error: body, response } = await api.POST("/auth/login", {
        body: { username, password, ...(captchaToken ? { captcha_token: captchaToken } : {}) },
      });
      if (response.status === 202) {
        setSecond({ methods: (data as unknown as MfaChallenge).methods });
        return;
      }
      if (!response.ok) throw new ApiError(response.status, body as ApiErrorBody | undefined, `${response.status}`);
      await finish(data as Me);
    } catch (err) {
      if (err instanceof ApiError && (err.code === "captcha_required" || err.code === "captcha_failed")) setCaptchaAsked(true);
      else if (err instanceof ApiError && err.status === 401) setFailures((n) => n + 1);
      setError(errorMessage(err));
    } finally {
      setBusy(false);
      // A token is good for one attempt.
      setCaptchaToken(undefined);
      setCaptchaRound((n) => n + 1);
    }
  };

  const passkeyLogin = async () => {
    setBusy(true);
    setError(undefined);
    try {
      const opts = await unwrap(api.POST("/auth/passkey/options", { body: { purpose: "login" } }));
      const credential = await getPasskey(opts);
      await finish(await unwrap(api.POST("/auth/passkey", { body: { credential } })));
    } catch (err) {
      if (!passkeyCancelled(err)) setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  if (second) {
    return (
      <AuthLayout brand={brand}>
        <MfaStep
          methods={second.methods}
          onDone={(me) => void finish(me, second.returnTo)}
          onRestart={(msg) => {
            setSecond(undefined);
            setPassword("");
            setNotice(msg || undefined);
            if (mfa) void navigate({ to: "/login", search: next ? { next } : {} });
          }}
        />
      </AuthLayout>
    );
  }

  const banner = notice ?? ssoError;
  return (
    <AuthLayout brand={brand}>
      <AuthCard title={t("login.title")} description={brand && brand.name !== "plugrelay" ? t("login.subtitleNamed", { name: brand.name }) : t("login.subtitle")}>
        {banner && <Banner>{banner}</Banner>}
        <SsoButtons providers={providers} next={next} />
        {passwordOn && providers.length > 0 && (
          <div className="my-5 flex items-center gap-3 text-xs text-ink-3" aria-hidden>
            <span className="h-px flex-1 bg-line" />
            {t("sso.or")}
            <span className="h-px flex-1 bg-line" />
          </div>
        )}
        {!passwordOn && <p className="mt-4 text-center text-xs text-ink-3">{t("sso.passwordOff")}</p>}
        <form onSubmit={submit} className="grid gap-4" hidden={!passwordOn}>
          <Field label={t("login.username")}>
            <Input autoComplete="username webauthn" value={username} onChange={(e) => setUsername(e.target.value)} autoFocus className="h-10" />
          </Field>
          <Field label={t("login.password")} error={error}>
            <PasswordInput autoComplete="current-password" value={password} onChange={setPassword} />
          </Field>
          {showCaptcha && captcha && <CaptchaWidget key={captchaRound} provider={captcha.provider} siteKey={captcha.site_key} onToken={setCaptchaToken} />}
          <Button type="submit" variant="primary" size="lg" className="mt-1 h-10" disabled={busy || !username || !password || (showCaptcha && !captchaToken)}>
            {busy ? t("login.signingIn") : t("login.submit")}
          </Button>
          {passkeysSupported() && (
            <Button size="lg" className="h-10" onClick={() => void passkeyLogin()} disabled={busy}>
              <Fingerprint />
              {t("login.passkey")}
            </Button>
          )}
        </form>
      </AuthCard>
      <SignupLink />
      {methods.data?.public_pages && (
        <p className="mt-4 flex justify-center gap-4 text-sm">
          <Link to="/public/models" className="text-ink-2 hover:text-ink hover:underline">
            {t("public.models")}
          </Link>
          {methods.data.public_pages.status && (
            <Link to="/public/status" className="text-ink-2 hover:text-ink hover:underline">
              {t("public.status")}
            </Link>
          )}
        </p>
      )}
    </AuthLayout>
  );
}
