import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Fingerprint, KeyRound, LockKeyhole, Pencil, Plus, ShieldAlert, ShieldCheck, Smartphone, Trash2, type LucideIcon } from "lucide-react";
import { useState, type ReactNode } from "react";
import { toast } from "sonner";
import { useConfirm } from "@/components/confirm";
import { CopyButton } from "@/components/copy-button";
import { PasswordInput } from "@/components/form-kit";
import { ErrorState, PageHeader } from "@/components/page";
import { Pill } from "@/components/status";
import { Button } from "@/components/ui/button";
import { Dialog, DialogContent } from "@/components/ui/dialog";
import { Field } from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import { Skeleton } from "@/components/ui/skeleton";
import { meQuery } from "@/app/session";
import { api, unwrap } from "@/lib/api/client";
import { ApiError, errorMessage } from "@/lib/api/errors";
import type { Passkey, SecurityStatus } from "@/lib/api/types";
import { cn } from "@/lib/cn";
import { formatDateTime, formatRelative } from "@/lib/format";
import { useI18n } from "@/i18n";
import { QrCode } from "./qr-code";
import { useReauth, type ReauthRun } from "./use-reauth";
import { createPasskey, passkeyCancelled, passkeysSupported } from "./webauthn";

const securityKey = ["me", "security"];
const MIN_PASSWORD = 10;

function useRefresh() {
  const qc = useQueryClient();
  return () => {
    void qc.invalidateQueries({ queryKey: securityKey });
    void qc.invalidateQueries({ queryKey: meQuery.queryKey });
  };
}

/** One sign-in method: icon, name, state on the right, then its own controls. */
function MethodCard({ icon: Icon, title, description, state, children }: { icon: LucideIcon; title: string; description: string; state?: ReactNode; children?: ReactNode }) {
  return (
    <section aria-label={title} className="rounded-lg border border-line bg-surface shadow-card">
      <div className="flex items-start gap-3.5 p-5">
        <span className="grid size-9 shrink-0 place-items-center rounded-lg border border-line bg-surface-2 text-ink-2">
          <Icon className="size-[18px]" strokeWidth={1.75} aria-hidden />
        </span>
        <div className="min-w-0 flex-1">
          <div className="flex flex-wrap items-center justify-between gap-2">
            <h2 className="text-base font-semibold">{title}</h2>
            {state}
          </div>
          <p className="mt-0.5 max-w-[64ch] text-sm text-ink-2">{description}</p>
          {children && <div className="mt-4">{children}</div>}
        </div>
      </div>
    </section>
  );
}

/** Ten one-time codes, shown once. */
function RecoveryCodesDialog({ codes, onClose }: { codes: string[]; onClose: () => void }) {
  const { t } = useI18n();
  return (
    <Dialog open onOpenChange={(o) => !o && onClose()}>
      <DialogContent
        title={t("recovery.title")}
        description={t("recovery.description")}
        footer={
          <>
            <CopyButton text={codes.join("\n")} label={t("recovery.copy")} size="md" />
            <Button variant="primary" onClick={onClose}>
              {t("recovery.saved")}
            </Button>
          </>
        }
      >
        <ul aria-label={t("recovery.title")} className="grid grid-cols-2 gap-x-6 gap-y-1.5 rounded-lg border border-line bg-surface-2 px-4 py-3 font-mono text-sm">
          {codes.map((c) => (
            <li key={c}>
              <code data-raw>{c}</code>
            </li>
          ))}
        </ul>
      </DialogContent>
    </Dialog>
  );
}

function PasswordCard({ status, run }: { status: SecurityStatus; run: ReauthRun }) {
  const { t } = useI18n();
  const refresh = useRefresh();
  const [open, setOpen] = useState(false);
  const [pw, setPw] = useState("");
  const [again, setAgain] = useState("");
  const [error, setError] = useState<string>();
  const change = useMutation({
    mutationFn: () => run(() => unwrap(api.POST("/me/password", { body: { password: pw } }))),
    onSuccess: () => {
      setOpen(false);
      setPw("");
      setAgain("");
      refresh();
      toast.success(t("password.changed"));
    },
    onError: (e) => (e instanceof ApiError && e.fields.password ? setError(e.fields.password) : toast.error(errorMessage(e))),
  });
  const short = pw.length > 0 && pw.length < MIN_PASSWORD;
  const mismatch = again.length > 0 && again !== pw;
  return (
    <MethodCard
      icon={LockKeyhole}
      title={t("password.title")}
      description={t(status.has_password ? "password.description" : "password.noneDescription")}
      state={<Pill tone={status.has_password ? "ok" : "neutral"} dot>{t(status.has_password ? "password.set" : "password.notSet")}</Pill>}
    >
      {open ? (
        <form
          className="grid max-w-sm gap-3"
          onSubmit={(e) => {
            e.preventDefault();
            setError(undefined);
            change.mutate();
          }}
        >
          <Field label={t("password.new")} hint={t("password.hint", { n: MIN_PASSWORD })} error={short ? t("password.tooShort", { n: MIN_PASSWORD }) : error}>
            <PasswordInput autoComplete="new-password" value={pw} onChange={setPw} />
          </Field>
          <Field label={t("password.repeat")} error={mismatch ? t("password.mismatch") : undefined}>
            <PasswordInput autoComplete="new-password" value={again} onChange={setAgain} />
          </Field>
          <div className="flex gap-2">
            <Button type="submit" variant="primary" disabled={change.isPending || pw.length < MIN_PASSWORD || again !== pw}>
              {change.isPending ? t("common.saving") : t(status.has_password ? "password.change" : "password.setNow")}
            </Button>
            <Button
              onClick={() => {
                setOpen(false);
                setPw("");
                setAgain("");
                setError(undefined);
              }}
            >
              {t("common.cancel")}
            </Button>
          </div>
        </form>
      ) : (
        <Button onClick={() => setOpen(true)}>{t(status.has_password ? "password.change" : "password.setNow")}</Button>
      )}
    </MethodCard>
  );
}

function TotpCard({ status, run, onCodes }: { status: SecurityStatus; run: ReauthRun; onCodes: (c: string[]) => void }) {
  const { t } = useI18n();
  const refresh = useRefresh();
  const confirm = useConfirm();
  const [setup, setSetup] = useState<{ secret: string; otpauth_uri: string }>();
  const [code, setCode] = useState("");
  const [error, setError] = useState<string>();
  const start = useMutation({
    mutationFn: () => run(() => unwrap(api.POST("/me/totp/setup"))),
    onSuccess: (s) => {
      setSetup(s);
      setCode("");
      setError(undefined);
    },
    onError: (e) => toast.error(errorMessage(e)),
  });
  const enable = useMutation({
    mutationFn: () => unwrap(api.POST("/me/totp", { body: { code } })),
    onSuccess: (r) => {
      setSetup(undefined);
      refresh();
      toast.success(t("totp.enabled"));
      onCodes(r.recovery_codes);
    },
    onError: (e) => setError(e instanceof ApiError && e.fields.code ? e.fields.code : errorMessage(e)),
  });
  const disable = useMutation({
    mutationFn: () => run(() => unwrap(api.DELETE("/me/totp"))),
    onSuccess: () => {
      refresh();
      toast.success(t("totp.disabled"));
    },
    onError: (e) => toast.error(errorMessage(e)),
  });

  return (
    <MethodCard
      icon={Smartphone}
      title={t("totp.title")}
      description={t("totp.description")}
      state={<Pill tone={status.totp_enabled ? "ok" : "neutral"} dot>{t(status.totp_enabled ? "security.on" : "security.off")}</Pill>}
    >
      {status.totp_enabled ? (
        <Button
          variant="danger"
          disabled={disable.isPending}
          onClick={async () => {
            if (await confirm({ title: t("totp.disableTitle"), body: t("totp.disableBody"), confirm: t("totp.disable"), danger: true })) disable.mutate();
          }}
        >
          {t("totp.disable")}
        </Button>
      ) : setup ? (
        <form
          className="grid gap-5 rounded-lg border border-line bg-surface-2/50 p-4 sm:grid-cols-[auto_minmax(0,1fr)]"
          onSubmit={(e) => {
            e.preventDefault();
            enable.mutate();
          }}
        >
          <div className="grid justify-items-center gap-2">
            <div className="rounded-lg border border-line bg-white p-2 shadow-xs">
              <QrCode value={setup.otpauth_uri} label={t("totp.qr")} size={160} />
            </div>
          </div>
          <ol className="grid content-start gap-4 text-sm">
            <li className="grid gap-1.5">
              <span className="font-medium">
                <span className="mr-2 inline-grid size-5 place-items-center rounded-full bg-accent-soft text-2xs font-semibold text-accent-strong">1</span>
                {t("totp.step1")}
              </span>
              <span className="text-xs text-ink-3">{t("totp.manual")}</span>
              <span className="flex min-w-0 items-center gap-1.5">
                <code className="min-w-0 break-all rounded-md border border-line bg-surface px-2 py-1 font-mono text-xs text-ink-2">{setup.secret}</code>
                <CopyButton text={setup.secret} size="icon-sm" variant="ghost" />
              </span>
            </li>
            <li className="grid gap-1.5">
              <span className="font-medium">
                <span className="mr-2 inline-grid size-5 place-items-center rounded-full bg-accent-soft text-2xs font-semibold text-accent-strong">2</span>
                {t("totp.step2")}
              </span>
              <Field label={t("mfa.code")} error={error}>
                <Input
                  value={code}
                  onChange={(e) => setCode(e.target.value.replace(/\s/g, ""))}
                  inputMode="numeric"
                  autoComplete="one-time-code"
                  maxLength={8}
                  placeholder="000000"
                  className="h-10 max-w-44 font-mono text-lg tracking-[0.3em] placeholder:text-ink-3/50"
                  autoFocus
                />
              </Field>
            </li>
            <div className="flex gap-2">
              <Button type="submit" variant="primary" disabled={enable.isPending || code.trim().length < 6}>
                {t("totp.enable")}
              </Button>
              <Button onClick={() => setSetup(undefined)}>{t("common.cancel")}</Button>
            </div>
          </ol>
        </form>
      ) : (
        <Button variant="primary" onClick={() => start.mutate()} disabled={start.isPending}>
          {t("totp.setup")}
        </Button>
      )}
    </MethodCard>
  );
}

function PasskeyRow({ p, onRename, onDelete, busy }: { p: Passkey; onRename: (name: string) => void; onDelete: () => void; busy: boolean }) {
  const { t, locale } = useI18n();
  const [editing, setEditing] = useState(false);
  const [name, setName] = useState(p.name);
  return (
    <li className="flex flex-wrap items-center gap-3 px-3 py-2.5">
      <KeyRound className="size-4 shrink-0 text-ink-3" aria-hidden />
      <div className="min-w-0 flex-1">
        {editing ? (
          <form
            className="flex gap-2"
            onSubmit={(e) => {
              e.preventDefault();
              onRename(name.trim());
              setEditing(false);
            }}
          >
            <Input aria-label={t("passkey.name")} value={name} onChange={(e) => setName(e.target.value)} maxLength={64} className="h-7 max-w-60" autoFocus />
            <Button type="submit" size="sm" disabled={!name.trim()}>
              {t("common.save")}
            </Button>
          </form>
        ) : (
          <div className="truncate text-sm font-medium">{p.name}</div>
        )}
        <div className="text-xs text-ink-3">
          {t("passkey.created", { time: formatDateTime(p.created_at, locale) })} ·{" "}
          {p.last_used_at ? t("passkey.lastUsed", { time: formatRelative(p.last_used_at, locale) }) : t("passkey.neverUsed")}
        </div>
      </div>
      <Button size="icon-sm" variant="ghost" aria-label={t("passkey.renameNamed", { name: p.name })} onClick={() => setEditing((v) => !v)}>
        <Pencil />
      </Button>
      <Button size="icon-sm" variant="ghost" aria-label={t("passkey.deleteNamed", { name: p.name })} onClick={onDelete} disabled={busy}>
        <Trash2 />
      </Button>
    </li>
  );
}

function PasskeysCard({ status, run, onCodes }: { status: SecurityStatus; run: ReauthRun; onCodes: (c: string[]) => void }) {
  const { t } = useI18n();
  const refresh = useRefresh();
  const confirm = useConfirm();
  const [name, setName] = useState("");
  const add = useMutation({
    mutationFn: async () => {
      const opts = await run(() => unwrap(api.POST("/me/passkeys/options")));
      const credential = await createPasskey(opts);
      return run(() => unwrap(api.POST("/me/passkeys", { body: { name: name.trim() || undefined, credential } })));
    },
    onSuccess: (r) => {
      setName("");
      refresh();
      toast.success(t("passkey.added", { name: r.passkey.name }));
      if (r.recovery_codes) onCodes(r.recovery_codes);
    },
    onError: (e) => toast.error(passkeyCancelled(e) ? t("passkey.cancelled") : errorMessage(e)),
  });
  const rename = useMutation({
    mutationFn: ({ id, name }: { id: string; name: string }) => unwrap(api.PATCH("/me/passkeys/{id}", { params: { path: { id } }, body: { name } })),
    onSuccess: refresh,
    onError: (e) => toast.error(errorMessage(e)),
  });
  const remove = useMutation({
    mutationFn: (id: string) => run(() => unwrap(api.DELETE("/me/passkeys/{id}", { params: { path: { id } } }))),
    onSuccess: () => {
      refresh();
      toast.success(t("passkey.deleted"));
    },
    onError: (e) => toast.error(errorMessage(e)),
  });
  const unavailable = status.passkeys_unavailable ? t("passkey.needsPublicUrl") : !passkeysSupported() ? t("passkey.unsupported") : undefined;
  const n = status.passkeys.length;

  return (
    <MethodCard
      icon={Fingerprint}
      title={t("passkey.title")}
      description={t("passkey.description")}
      state={<Pill tone={n ? "ok" : "neutral"} dot>{n ? t("passkey.count", { n }) : t("security.off")}</Pill>}
    >
      {n > 0 ? (
        <ul aria-label={t("passkey.title")} className="mb-3 divide-y divide-line-soft rounded-lg border border-line">
          {status.passkeys.map((p) => (
            <PasskeyRow
              key={p.id}
              p={p}
              busy={remove.isPending}
              onRename={(v) => v && v !== p.name && rename.mutate({ id: p.id, name: v })}
              onDelete={async () => {
                if (await confirm({ title: t("passkey.deleteTitle", { name: p.name }), body: t("passkey.deleteBody"), confirm: t("common.delete"), danger: true }))
                  remove.mutate(p.id);
              }}
            />
          ))}
        </ul>
      ) : (
        <p className="mb-3 text-xs text-ink-3">{t("passkey.empty")}</p>
      )}
      {unavailable ? (
        <p className="rounded-md bg-warn-soft px-3 py-2 text-xs text-warn">{unavailable}</p>
      ) : (
        <form
          className="flex flex-wrap gap-2"
          onSubmit={(e) => {
            e.preventDefault();
            add.mutate();
          }}
        >
          <Input aria-label={t("passkey.name")} placeholder={t("passkey.namePlaceholder")} value={name} onChange={(e) => setName(e.target.value)} maxLength={64} className="max-w-60" />
          <Button type="submit" disabled={add.isPending}>
            <Plus />
            {t("passkey.add")}
          </Button>
        </form>
      )}
    </MethodCard>
  );
}

/** At a glance: is the account protected by a second step? */
function Summary({ s }: { s: SecurityStatus }) {
  const { t, locale } = useI18n();
  const on = s.totp_enabled || s.passkeys.length > 0;
  const Icon = on ? ShieldCheck : ShieldAlert;
  return (
    <div className={cn("flex items-start gap-4 rounded-lg border p-5", on ? "border-ok/25 bg-ok-soft/60" : "border-warn/30 bg-warn-soft/60")}>
      <span className={cn("grid size-10 shrink-0 place-items-center rounded-full", on ? "bg-ok/15 text-ok" : "bg-warn/15 text-warn")}>
        <Icon className="size-5" aria-hidden />
      </span>
      <div className="min-w-0">
        <p className="text-base font-semibold">{t(on ? "security.summaryOn" : "security.summaryOff")}</p>
        <p className="mt-0.5 text-sm text-ink-2">
          {s.required && !on && s.enroll_by
            ? t("security.requiredBy", { date: formatDateTime(s.enroll_by, locale) })
            : t(on ? "security.summaryOnHint" : "security.summaryOffHint")}
        </p>
      </div>
    </div>
  );
}

function LoadingCards() {
  return (
    <div className="grid gap-4" aria-busy="true">
      {Array.from({ length: 4 }, (_, i) => (
        <div key={i} className="flex gap-3.5 rounded-lg border border-line bg-surface p-5">
          <Skeleton className="size-9" />
          <div className="grid flex-1 gap-2">
            <Skeleton className="h-4 w-32" />
            <Skeleton className="h-4 w-2/3" />
          </div>
        </div>
      ))}
    </div>
  );
}

/** The signed-in user's own sign-in methods: password, authenticator app, passkeys, recovery codes. */
export function SecurityPage() {
  const { t } = useI18n();
  const q = useQuery({ queryKey: securityKey, queryFn: () => unwrap(api.GET("/me/security")), retry: false });
  const { run, dialog } = useReauth(q.data);
  const [codes, setCodes] = useState<string[]>();
  const regenerate = useMutation({
    mutationFn: () => run(() => unwrap(api.POST("/me/recovery-codes"))),
    onSuccess: (r) => setCodes(r.recovery_codes),
    onError: (e) => toast.error(errorMessage(e)),
  });
  const s = q.data;
  const enabled = !!s && (s.totp_enabled || s.passkeys.length > 0);
  return (
    <>
      <PageHeader title={t("security.title")} description={t("security.description")} />
      <div className="grid max-w-3xl gap-4 pb-12">
        {q.isError ? (
          <ErrorState error={q.error} onRetry={() => void q.refetch()} />
        ) : !s ? (
          <LoadingCards />
        ) : (
          <>
            <Summary s={s} />
            <PasswordCard status={s} run={run} />
            <TotpCard status={s} run={run} onCodes={setCodes} />
            <PasskeysCard status={s} run={run} onCodes={setCodes} />
            <MethodCard
              icon={KeyRound}
              title={t("recovery.title")}
              description={t("recovery.hint")}
              state={enabled ? <Pill tone={s.recovery_codes_left > 2 ? "ok" : "warn"} dot>{t("recovery.left", { n: s.recovery_codes_left })}</Pill> : undefined}
            >
              <Button onClick={() => regenerate.mutate()} disabled={!enabled || regenerate.isPending}>
                {t("recovery.regenerate")}
              </Button>
              {!enabled && <p className="mt-2 text-xs text-ink-3">{t("recovery.needsFactor")}</p>}
            </MethodCard>
          </>
        )}
      </div>
      {codes && <RecoveryCodesDialog codes={codes} onClose={() => setCodes(undefined)} />}
      {dialog}
    </>
  );
}
