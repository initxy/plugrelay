import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { ChevronDown, KeyRound, Pencil, PlugZap, Plus, RotateCcw, Trash2 } from "lucide-react";
import { useState } from "react";
import { toast } from "sonner";
import { useConfirm } from "@/components/confirm";
import { EmptyState } from "@/components/page";
import { Pill } from "@/components/status";
import { Button } from "@/components/ui/button";
import { Field } from "@/components/ui/field";
import { Input, Textarea } from "@/components/ui/input";
import { Segmented } from "@/components/ui/segmented";
import { Select } from "@/components/ui/select";
import { Switch } from "@/components/ui/switch";
import { api, unwrap } from "@/lib/api/client";
import { errorMessage } from "@/lib/api/errors";
import type { components } from "@/lib/api/schema.gen";
import type { Credential, Upstream } from "@/lib/api/types";
import { cn } from "@/lib/cn";
import { formatDuration, formatRelative, formatWhen } from "@/lib/format";
import { useForm } from "@/lib/use-form";
import { useI18n } from "@/i18n";
import type { MessageKey } from "@/i18n/en";
import { CredentialPill, CredentialQuota, RecentErrors, useReasonText, useResetCredential } from "./credential-state";
import { credentialResettable, secretForm, splitSecrets, type SecretForm } from "./labels";
import { useLastTest, useTestUpstreams } from "./use-upstream-test";

type Kind = Credential["kind"];

const SECRET_LABEL: Record<SecretForm, { label: MessageKey; hint: MessageKey }> = {
  key: { label: "cred.secret", hint: "cred.secretManyHint" },
  optional: { label: "cred.secretOptional", hint: "cred.secretOptionalHint" },
  aws: { label: "cred.secretAws", hint: "cred.secretAwsHint" },
  gcp: { label: "cred.secretGcp", hint: "cred.secretGcpHint" },
};

/** What kind of secret this upstream's type takes. */
function useUpstreamSecretForm(upstream: Upstream): SecretForm {
  return secretForm(upstream.source.name, upstream.key_optional);
}

export function useSecretLabels(form: SecretForm) {
  const { t } = useI18n();
  return { label: t(SECRET_LABEL[form].label), hint: t(SECRET_LABEL[form].hint) };
}

function useRefresh(upstreamId: string) {
  const qc = useQueryClient();
  return () => {
    void qc.invalidateQueries({ queryKey: ["upstream", upstreamId] });
    void qc.invalidateQueries({ queryKey: ["upstreams"] });
  };
}

function KindPill({ c }: { c: Credential }) {
  const { t } = useI18n();
  if (c.kind === "personal") return <Pill tone="info">{c.owner ? t("cred.kind.personalOf", { name: c.owner.name }) : t("cred.kind.personal")}</Pill>;
  if (c.kind === "service") return <Pill tone="neutral">{t("cred.kind.service")}</Pill>;
  return <Pill tone="neutral">{t("cred.kind.shared")}</Pill>;
}

function CredentialRow({ c, upstream, sf }: { c: Credential; upstream: Upstream; sf: SecretForm }) {
  const { t, locale } = useI18n();
  const confirm = useConfirm();
  const refresh = useRefresh(upstream.id);
  const reasonText = useReasonText();
  const reset = useResetCredential(upstream.id);
  const test = useTestUpstreams();
  const last = useLastTest(upstream.id)?.find((r) => r.credential_id === c.id);
  const [editing, setEditing] = useState(false);
  const [showErrors, setShowErrors] = useState(false);
  const form = useForm({ label: c.label, secret: "", proxy: c.proxy ?? "", max_concurrency: c.max_concurrency ? String(c.max_concurrency) : "" });
  const secretText = useSecretLabels(sf);

  const update = useMutation({
    mutationFn: (body: components["schemas"]["CredentialPatch"]) =>
      unwrap(api.PATCH("/credentials/{id}", { params: { path: { id: c.id } }, body })),
    onSuccess: refresh,
    onError: (e) => {
      form.absorb(e);
      toast.error(errorMessage(e));
    },
  });
  const remove = useMutation({
    mutationFn: () => unwrap(api.DELETE("/credentials/{id}", { params: { path: { id: c.id } } })),
    onSuccess: () => {
      toast.success(t("cred.deleted", { name: c.label }));
      refresh();
    },
    onError: (e) => toast.error(errorMessage(e)),
  });

  const s = c.state;
  const reason = s.status === "ok" || s.status === "disabled" || s.status === "circuit_open" ? undefined : reasonText(s.reason);
  const errs = s.recent_errors?.length ?? 0;
  return (
    <li className={cn("rounded-lg border border-line bg-surface", s.status === "disabled" && "bg-surface-2/50")}>
      <div className="grid gap-2 px-3.5 py-3">
        <div className="flex flex-wrap items-center gap-2">
          <span className={cn("min-w-0 truncate font-medium", s.status === "disabled" && "text-ink-3")}>{c.label}</span>
          <KindPill c={c} />
          <span className="ml-auto">
            <CredentialPill c={c} />
          </span>
        </div>
        <div className="flex flex-wrap gap-x-3 gap-y-0.5 text-xs text-ink-3">
          <span className="font-mono">{c.secret_hint}</span>
          {c.proxy && <span>{t("cred.via", { proxy: c.proxy })}</span>}
          {(c.max_concurrency ?? 0) > 0 && <span>{t("cred.maxConcurrency", { n: c.max_concurrency ?? 0 })}</span>}
          {(s.in_flight ?? 0) > 0 && <span>{t("cred.inFlight", { n: s.in_flight ?? 0 })}</span>}
          {c.expires_at && <span>{t("cred.expires", { at: formatWhen(c.expires_at, locale) })}</span>}
        </div>
        {(reason || (s.until && s.status !== "ok" && s.status !== "disabled")) && (
          <p className="text-sm text-ink-2">
            {reason}
            {s.until && s.status !== "ok" && s.status !== "disabled" && (
              <span className="text-xs text-ink-3">
                {reason ? " · " : ""}
                {t("cred.until", { at: formatWhen(s.until, locale), rel: formatRelative(s.until, locale) })}
              </span>
            )}
          </p>
        )}
        {s.attention && <p className="text-sm text-warn">{reasonText(s.attention.reason) ?? t("cred.attention")}</p>}
        {s.status === "quarantined" && <p className="text-xs text-ink-3">{t("cred.resetWarn")}</p>}
        <CredentialQuota c={c} className="max-w-md" />
        {last && (
          <p className={cn("text-xs", last.ok ? "text-ok" : "text-bad")}>
            {last.ok ? t("upstreams.testPassed", { d: formatDuration(last.ttfb_ms ?? last.latency_ms ?? 0) }) : `${last.status ?? ""} ${last.error ?? ""}`.trim().slice(0, 240)}
          </p>
        )}
        {errs > 0 && (
          <div>
            <button type="button" onClick={() => setShowErrors((v) => !v)} className="inline-flex items-center gap-1 text-xs text-ink-2 hover:text-ink" aria-expanded={showErrors}>
              <ChevronDown className={cn("size-3.5 transition-transform", !showErrors && "-rotate-90")} />
              {t("cred.recentErrorsCount", { n: errs })}
            </button>
            {showErrors && <RecentErrors c={c} className="mt-1.5" />}
          </div>
        )}
        <div className="flex flex-wrap items-center gap-1.5 border-t border-line-soft pt-2">
          <Button size="xs" onClick={() => test.mutate({ ids: [upstream.id], credentialId: c.id })} disabled={test.isPending || s.status === "disabled"}>
            <PlugZap />
            {t("upstreams.test")}
          </Button>
          {credentialResettable(c) && (
            <Button size="xs" onClick={() => reset.mutate(c)} disabled={reset.isPending}>
              <RotateCcw />
              {t("cred.reset")}
            </Button>
          )}
          <Button size="xs" variant="ghost" onClick={() => setEditing((v) => !v)} aria-expanded={editing}>
            <Pencil />
            {t("common.edit")}
          </Button>
          <Button
            size="xs"
            variant="ghost"
            className="text-bad hover:bg-bad-soft hover:text-bad"
            onClick={async () => {
              if (await confirm({ title: t("cred.deleteTitle", { name: c.label }), body: t("cred.deleteBody"), confirm: t("common.delete"), danger: true }))
                remove.mutate();
            }}
          >
            <Trash2 />
            {t("common.delete")}
          </Button>
          <label className="ml-auto flex items-center gap-2 text-xs text-ink-2">
            {t("cred.inRotation")}
            <Switch aria-label={t("cred.inRotation")} checked={c.enabled !== false && s.status !== "disabled"} onChange={(v) => update.mutate({ enabled: v })} disabled={update.isPending} />
          </label>
        </div>
      </div>
      {editing && (
        <form
          className="grid items-start gap-3 border-t border-line bg-surface-2/40 px-3.5 py-3 sm:grid-cols-2"
          onSubmit={(e) => {
            e.preventDefault();
            const v = form.values;
            update.mutate(
              {
                label: v.label.trim(),
                ...(v.secret ? { secret: v.secret } : {}),
                proxy: v.proxy.trim(),
                max_concurrency: v.max_concurrency ? Number(v.max_concurrency) : 0,
              },
              {
                onSuccess: () => {
                  toast.success(t("cred.saved", { name: v.label.trim() }));
                  setEditing(false);
                  form.reset({ ...v, secret: "" });
                },
              },
            );
          }}
        >
          <Field label={t("cred.label")} error={form.errors.label}>
            <Input value={form.values.label} onChange={(e) => form.set("label", e.target.value)} />
          </Field>
          <Field label={t("cred.replaceSecret", { what: secretText.label })} hint={t("cred.replaceSecretHint")} error={form.errors.secret}>
            <Input type="password" autoComplete="off" value={form.values.secret} onChange={(e) => form.set("secret", e.target.value)} />
          </Field>
          <Field label={t("cred.proxy")} hint={t("cred.proxyHint")} error={form.errors.proxy} optional>
            <Input value={form.values.proxy} onChange={(e) => form.set("proxy", e.target.value)} placeholder="socks5://10.0.0.2:1080" className="font-mono text-[12.5px]" />
          </Field>
          <Field label={t("cred.concurrency")} hint={t("cred.concurrencyHint")} error={form.errors.max_concurrency} optional>
            <Input inputMode="numeric" value={form.values.max_concurrency} onChange={(e) => form.set("max_concurrency", e.target.value.replace(/\D/g, ""))} />
          </Field>
          <div className="flex justify-end gap-2 sm:col-span-2">
            <Button size="sm" onClick={() => setEditing(false)}>
              {t("common.cancel")}
            </Button>
            <Button size="sm" variant="primary" type="submit" disabled={update.isPending || !form.values.label.trim()}>
              {update.isPending ? t("common.saving") : t("common.save")}
            </Button>
          </div>
        </form>
      )}
    </li>
  );
}

const emptyAdd = { kind: "shared" as Kind, owner_id: "", label: "", secret: "", proxy: "", max_concurrency: "" };

function AddCredential({ upstream, sf, onDone }: { upstream: Upstream; sf: SecretForm; onDone: () => void }) {
  const { t } = useI18n();
  const refresh = useRefresh(upstream.id);
  const form = useForm(emptyAdd);
  const v = form.values;
  const secretText = useSecretLabels(sf);
  const users = useQuery({ queryKey: ["users"], queryFn: () => unwrap(api.GET("/users")), enabled: v.kind === "personal" });
  const secrets = splitSecrets(v.secret, sf);
  const add = useMutation({
    mutationFn: async () => {
      const errs: Record<string, string> = {};
      if (!v.label.trim()) errs.label = t("form.required");
      if (v.kind === "personal" && !v.owner_id) errs.owner_id = t("form.required");
      if (sf !== "optional" && secrets.length === 0) errs.secret = t("form.required");
      if (Object.keys(errs).length) {
        form.setErrors(errs);
        throw new Error(t("form.fixErrors"));
      }
      const list = secrets.length ? secrets : [""];
      for (const [i, secret] of list.entries()) {
        await unwrap(
          api.POST("/upstreams/{id}/credentials", {
            params: { path: { id: upstream.id } },
            body: {
              label: list.length > 1 ? `${v.label.trim()} ${i + 1}` : v.label.trim(),
              kind: v.kind,
              owner_id: v.kind === "personal" ? v.owner_id : undefined,
              secret,
              proxy: v.proxy.trim() || undefined,
              max_concurrency: v.max_concurrency ? Number(v.max_concurrency) : undefined,
            },
          }),
        );
      }
      return list.length;
    },
    onSuccess: (n) => {
      toast.success(t("cred.addedN", { n }));
      refresh();
      onDone();
    },
    onError: (e) => {
      form.absorb(e);
      toast.error(errorMessage(e));
    },
  });
  return (
    <form
      className="grid gap-3.5 rounded-lg border border-dashed border-line-strong p-3.5"
      onSubmit={(e) => {
        e.preventDefault();
        add.mutate();
      }}
    >
      <div className="grid gap-1.5">
        <span className="text-xs font-semibold">{t("cred.kind")}</span>
        <Segmented
          aria-label={t("cred.kind")}
          value={v.kind}
          onChange={(k) => form.set("kind", k)}
          options={[
            { value: "shared", label: t("cred.kind.shared") },
            { value: "personal", label: t("cred.kind.personal") },
            { value: "service", label: t("cred.kind.service") },
          ]}
        />
        <p className="text-xs text-ink-3">{t(`cred.kind.${v.kind}Hint` as MessageKey)}</p>
      </div>
      <div className="grid items-start gap-3 sm:grid-cols-2">
        <Field label={t("cred.label")} error={form.errors.label}>
          <Input value={v.label} onChange={(e) => form.set("label", e.target.value)} placeholder={t("cred.labelPlaceholder")} autoFocus />
        </Field>
        {v.kind === "personal" && (
          <Field label={t("cred.owner")} error={form.errors.owner_id}>
            <Select
              value={v.owner_id || undefined}
              onChange={(id) => form.set("owner_id", id)}
              placeholder={t("cred.pickOwner")}
              options={(users.data?.items ?? []).map((u) => ({ value: u.id, label: u.display_name || u.username }))}
            />
          </Field>
        )}
      </div>
      <Field label={secretText.label} hint={secretText.hint} error={form.errors.secret}>
        <Textarea
          value={v.secret}
          onChange={(e) => form.set("secret", e.target.value)}
          className="min-h-16 font-mono text-[12.5px] [-webkit-text-security:disc]"
          autoComplete="off"
          spellCheck={false}
        />
      </Field>
      {secrets.length > 1 && <p className="-mt-2 text-xs text-accent-strong">{t("cred.willAdd", { n: secrets.length })}</p>}
      <div className="grid items-start gap-3 sm:grid-cols-2">
        <Field label={t("cred.proxy")} hint={t("cred.proxyHint")} error={form.errors.proxy} optional>
          <Input value={v.proxy} onChange={(e) => form.set("proxy", e.target.value)} placeholder="socks5://10.0.0.2:1080" className="font-mono text-[12.5px]" />
        </Field>
        <Field label={t("cred.concurrency")} hint={t("cred.concurrencyHint")} error={form.errors.max_concurrency} optional>
          <Input inputMode="numeric" value={v.max_concurrency} onChange={(e) => form.set("max_concurrency", e.target.value.replace(/\D/g, ""))} />
        </Field>
      </div>
      <div className="flex justify-end gap-2">
        <Button size="sm" onClick={onDone}>
          {t("common.cancel")}
        </Button>
        <Button size="sm" variant="primary" type="submit" disabled={add.isPending}>
          {add.isPending ? t("common.saving") : secrets.length > 1 ? t("cred.addN", { n: secrets.length }) : t("cred.add")}
        </Button>
      </div>
    </form>
  );
}

/** Every credential of an upstream with its live state; add, edit, test, take out of rotation, delete. */
export function CredentialsTab({ upstream }: { upstream: Upstream }) {
  const { t } = useI18n();
  const sf = useUpstreamSecretForm(upstream);
  const [open, setAdding] = useState<boolean>();
  const adding = open ?? (upstream.credentials.length === 0 && sf !== "optional");
  return (
    <div className="grid gap-3">
      <div className="flex items-center justify-between gap-2">
        <p className="text-xs text-ink-3">{t("cred.writeOnly")}</p>
        {!adding && (
          <Button size="sm" onClick={() => setAdding(true)}>
            <Plus />
            {t("cred.add")}
          </Button>
        )}
      </div>
      {adding && <AddCredential upstream={upstream} sf={sf} onDone={() => setAdding(false)} />}
      {upstream.credentials.length === 0 && !adding ? (
        <div className="rounded-lg border border-dashed border-line-strong">
          <EmptyState compact icon={KeyRound} title={t(sf === "optional" ? "cred.none" : "cred.needsKey")} hint={t(sf === "optional" ? "cred.noneKeyless" : "cred.noneHint")} />
        </div>
      ) : (
        <ul className="grid gap-2">
          {upstream.credentials.map((c) => (
            <CredentialRow key={c.id} c={c} upstream={upstream} sf={sf} />
          ))}
        </ul>
      )}
    </div>
  );
}
