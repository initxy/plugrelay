import { KeyRound } from "lucide-react";
import { useCallback, useRef, useState } from "react";
import { Button } from "@/components/ui/button";
import { Dialog, DialogContent } from "@/components/ui/dialog";
import { Field } from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import { api, unwrap } from "@/lib/api/client";
import { ApiError, errorMessage } from "@/lib/api/errors";
import type { FactorInput, SecurityStatus } from "@/lib/api/types";
import { useI18n } from "@/i18n";
import { getPasskey, passkeyCancelled, passkeysSupported } from "./webauthn";

/** Confirm-it-is-you dialog: sensitive calls answering 403 reauth_required are retried after it. */
export function useReauth(status: SecurityStatus | undefined) {
  const { t } = useI18n();
  const [open, setOpen] = useState(false);
  const waiter = useRef<{ resolve: () => void; reject: (e: unknown) => void } | undefined>(undefined);
  const [secretText, setSecretText] = useState("");
  const [error, setError] = useState<string>();
  const [busy, setBusy] = useState(false);

  const run = useCallback(async <T,>(fn: () => Promise<T>): Promise<T> => {
    try {
      return await fn();
    } catch (e) {
      if (!(e instanceof ApiError && e.code === "reauth_required")) throw e;
    }
    await new Promise<void>((resolve, reject) => {
      waiter.current = { resolve, reject };
      setSecretText("");
      setError(undefined);
      setOpen(true);
    });
    return fn();
  }, []);

  const send = async (body: FactorInput) => {
    setBusy(true);
    setError(undefined);
    try {
      await unwrap(api.POST("/me/reauth", { body }));
      setOpen(false);
      waiter.current?.resolve();
    } catch (e) {
      setError(passkeyCancelled(e) ? t("passkey.cancelled") : errorMessage(e));
    } finally {
      setBusy(false);
    }
  };
  const submitText = () => {
    const v = secretText.trim();
    if (status?.totp_enabled && /^\d{6}$/.test(v.replace(/\s/g, ""))) return send({ method: "totp", code: v });
    if (!status?.has_password || /^[a-z0-9]{4}-[a-z0-9]{4}-[a-z0-9]{4}$/i.test(v)) return send({ method: "recovery", code: v });
    return send({ method: "password", password: secretText });
  };
  const withPasskey = async () => {
    try {
      const opts = await unwrap(api.POST("/auth/passkey/options", { body: { purpose: "reauth" } }));
      await send({ method: "passkey", credential: await getPasskey(opts) });
    } catch (e) {
      setError(passkeyCancelled(e) ? t("passkey.cancelled") : errorMessage(e));
    }
  };
  const cancel = () => {
    setOpen(false);
    waiter.current?.reject(new Error(t("reauth.cancelled")));
  };
  const label = status?.has_password ? (status.totp_enabled ? t("reauth.passwordOrCode") : t("login.password")) : t("reauth.code");

  const dialog = (
    <Dialog open={open} onOpenChange={(o) => !o && cancel()}>
      <DialogContent
        title={t("reauth.title")}
        description={t("reauth.description")}
        footer={
          <>
            <Button onClick={cancel}>{t("common.cancel")}</Button>
            <Button variant="primary" onClick={() => void submitText()} disabled={busy || !secretText}>
              {t("common.confirm")}
            </Button>
          </>
        }
      >
        <form
          className="grid gap-3"
          onSubmit={(e) => {
            e.preventDefault();
            if (secretText) void submitText();
          }}
        >
          <Field label={label} error={error}>
            <Input type={status?.has_password ? "password" : "text"} value={secretText} onChange={(e) => setSecretText(e.target.value)} autoFocus autoComplete="current-password" />
          </Field>
          {(status?.passkeys.length ?? 0) > 0 && passkeysSupported() && (
            <Button onClick={() => void withPasskey()} disabled={busy}>
              <KeyRound />
              {t("mfa.usePasskey")}
            </Button>
          )}
        </form>
      </DialogContent>
    </Dialog>
  );
  return { run, dialog };
}

export type ReauthRun = ReturnType<typeof useReauth>["run"];
