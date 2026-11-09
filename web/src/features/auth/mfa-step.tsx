import { ArrowLeft, Fingerprint, ShieldCheck } from "lucide-react";
import { useState, type FormEvent } from "react";
import { Button } from "@/components/ui/button";
import { Field } from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import { api, unwrap } from "@/lib/api/client";
import { ApiError, errorMessage } from "@/lib/api/errors";
import type { Me, MfaMethod } from "@/lib/api/types";
import { useI18n } from "@/i18n";
import { AuthCard } from "./auth-layout";
import { getPasskey, passkeyCancelled, passkeysSupported } from "./webauthn";

/** Second step of a sign-in: authenticator code, passkey or recovery code. No session exists before it passes. */
export function MfaStep({ methods, onDone, onRestart }: { methods: MfaMethod[]; onDone: (me: Me) => void; onRestart: (message: string) => void }) {
  const { t } = useI18n();
  const hasTotp = methods.includes("totp");
  const hasPasskey = methods.includes("passkey") && passkeysSupported();
  const [recovery, setRecovery] = useState(!hasTotp && !hasPasskey);
  const [code, setCode] = useState("");
  const [error, setError] = useState<string>();
  const [busy, setBusy] = useState(false);

  const fail = (e: unknown) => {
    if (e instanceof ApiError && e.code === "mfa_expired") onRestart(t("mfa.expired"));
    else setError(passkeyCancelled(e) ? t("passkey.cancelled") : errorMessage(e));
  };
  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError(undefined);
    try {
      onDone(await unwrap(api.POST("/auth/mfa", { body: { method: recovery ? "recovery" : "totp", code } })));
    } catch (err) {
      setCode("");
      fail(err);
    } finally {
      setBusy(false);
    }
  };
  const signWithPasskey = async () => {
    setBusy(true);
    setError(undefined);
    try {
      const opts = await unwrap(api.POST("/auth/passkey/options", { body: { purpose: "mfa" } }));
      const credential = await getPasskey(opts);
      onDone(await unwrap(api.POST("/auth/mfa", { body: { method: "passkey", credential } })));
    } catch (err) {
      fail(err);
    } finally {
      setBusy(false);
    }
  };

  return (
    <AuthCard
      title={
        <span className="flex items-center gap-2.5">
          <span className="grid size-8 place-items-center rounded-lg bg-accent-soft text-accent-strong">
            <ShieldCheck className="size-4.5" aria-hidden />
          </span>
          {t("mfa.title")}
        </span>
      }
      description={t("mfa.description")}
    >
      <form onSubmit={submit} className="grid gap-4">
        {hasPasskey && (
          <Button variant={hasTotp ? "default" : "primary"} size="lg" className="h-10" onClick={() => void signWithPasskey()} disabled={busy}>
            <Fingerprint />
            {t("mfa.usePasskey")}
          </Button>
        )}
        {hasPasskey && (hasTotp || recovery) && (
          <div className="flex items-center gap-3 text-xs text-ink-3" aria-hidden>
            <span className="h-px flex-1 bg-line" />
            {t("sso.or")}
            <span className="h-px flex-1 bg-line" />
          </div>
        )}
        {(hasTotp || recovery) && (
          <>
            <Field label={recovery ? t("mfa.recoveryCode") : t("mfa.code")} hint={recovery ? t("mfa.recoveryHint") : undefined} error={error}>
              <Input
                value={code}
                onChange={(e) => setCode(recovery ? e.target.value : e.target.value.replace(/\s/g, ""))}
                autoComplete="one-time-code"
                inputMode={recovery ? "text" : "numeric"}
                maxLength={recovery ? 32 : 8}
                placeholder={recovery ? "xxxx-xxxx-xxxx" : "000000"}
                autoFocus
                className="h-12 text-center font-mono text-xl tracking-[0.35em] placeholder:tracking-[0.35em] placeholder:text-ink-3/50"
              />
            </Field>
            <Button type="submit" variant="primary" size="lg" className="h-10" disabled={busy || code.trim().length < (recovery ? 8 : 6)}>
              {busy ? t("login.signingIn") : t("mfa.verify")}
            </Button>
          </>
        )}
        {!hasTotp && !recovery && error && (
          <p role="alert" className="text-xs text-bad">
            {error}
          </p>
        )}
        <div className="mt-1 flex flex-wrap items-center justify-between gap-2 border-t border-line-soft pt-4 text-sm">
          <button type="button" className="inline-flex items-center gap-1 text-ink-3 hover:text-ink" onClick={() => onRestart("")}>
            <ArrowLeft className="size-3.5" aria-hidden />
            {t("mfa.startOver")}
          </button>
          {methods.includes("recovery") && (
            <button
              type="button"
              className="text-accent hover:underline"
              onClick={() => {
                setRecovery((r) => !r);
                setCode("");
                setError(undefined);
              }}
            >
              {recovery ? (hasTotp ? t("mfa.useApp") : t("mfa.usePasskeyInstead")) : t("mfa.useRecovery")}
            </button>
          )}
        </div>
      </form>
    </AuthCard>
  );
}
