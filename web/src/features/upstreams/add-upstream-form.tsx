import { useMutation, useQueryClient } from "@tanstack/react-query";
import { useNavigate } from "@tanstack/react-router";
import { Check, ChevronRight, ExternalLink, Loader2, SquareDashed, X } from "lucide-react";
import { useEffect, useRef, useState } from "react";
import { toast } from "sonner";
import { SwitchRow } from "@/components/form-kit";
import { Button } from "@/components/ui/button";
import { Field } from "@/components/ui/field";
import { Input, Textarea } from "@/components/ui/input";
import { Segmented } from "@/components/ui/segmented";
import { api, unwrap } from "@/lib/api/client";
import { errorMessage } from "@/lib/api/errors";
import type { Manifest, TestResult, UpstreamInput } from "@/lib/api/types";
import { cn } from "@/lib/cn";
import { formatDuration } from "@/lib/format";
import { useForm } from "@/lib/use-form";
import { useI18n } from "@/i18n";
import { useSecretLabels } from "./credentials-tab";
import { hostOf, manifestTitle, protocolLabels, secretForm, splitSecrets } from "./labels";
import { ManifestVariableFields, missingVariables } from "./manifest-variables";
import { ProviderAvatar } from "./provider-avatar";

/** The card that stands for "any server speaking OpenAI's or Anthropic's API". */
export const CUSTOM = "custom";

type CustomProtocol = "openai" | "anthropic";

/** A short line under a provider's name: what it needs or what it speaks. */
export function useTagline() {
  const { t } = useI18n();
  return (m: Manifest): string => {
    const sf = secretForm(m.name, m.key_optional);
    if (sf === "aws") return t("gallery.tag.aws");
    if (sf === "gcp") return t("gallery.tag.gcp");
    const protos = protocolLabels(
      m.protocols.filter((p) => ["anthropic", "openai-chat", "openai-responses", "gemini"].includes(p)),
      t,
    );
    const list = protos.length ? protos.slice(0, 2).join(" · ") : (protocolLabels(m.protocols.slice(0, 1), t)[0] ?? "");
    const speaks = t("gallery.tag.speaks", { list });
    return m.category === "self_hosted" ? t("gallery.tag.local", { speaks }) : speaks;
  };
}

interface AddDraft {
  name: string;
  secret: string;
  config: Record<string, string>;
  protocol: CustomProtocol;
  base_url: string;
  proxy: string;
  allow_private: boolean;
  model_sync: boolean;
  test_first: boolean;
  test_model: string;
}

type Phase = { step: "idle" } | { step: "saving" } | { step: "testing"; n: number } | { step: "failed"; results: TestResult[] };

/**
 * The form for the picked provider: stores the upstream switched off with one credential per key,
 * tests it, and turns it on once the test passes (or the operator saves anyway).
 */
export function AddPanel({ manifest, onClose }: { manifest: Manifest | typeof CUSTOM; onClose: () => void }) {
  const { t, locale } = useI18n();
  const qc = useQueryClient();
  const navigate = useNavigate();
  const tagline = useTagline();
  const custom = manifest === CUSTOM;
  const m = custom ? undefined : manifest;
  const local = custom || m?.category === "self_hosted";
  const form = useForm<AddDraft>({
    name: "",
    secret: "",
    config: {},
    protocol: "openai",
    base_url: "",
    proxy: "",
    allow_private: local,
    model_sync: true,
    test_first: true,
    test_model: "",
  });
  const d = form.values;
  // A custom server is stored on the openai-compatible manifest (key optional) or the anthropic
  // one, which needs a key to be routed.
  const sf = custom ? (d.protocol === "openai" ? "optional" : "key") : secretForm(m?.name, m?.key_optional);
  const secretText = useSecretLabels(sf);
  const vars = m?.variables ?? [];
  const secrets = splitSecrets(d.secret, sf);
  const [phase, setPhase] = useState<Phase>({ step: "idle" });
  // The upstream stored for the test; deleted again unless it is kept.
  const draft = useRef<{ id: string; credIds: string[] } | null>(null);
  const kept = useRef(false);

  useEffect(
    () => () => {
      const dr = draft.current;
      if (dr && !kept.current) void api.DELETE("/upstreams/{id}", { params: { path: { id: dr.id } } });
    },
    [],
  );

  const body = (): UpstreamInput => {
    const config: Record<string, unknown> = Object.fromEntries(Object.entries(d.config).filter(([, v]) => v.trim()));
    const out: UpstreamInput = { name: d.name.trim() || undefined, model_sync: d.model_sync, proxy: d.proxy.trim() };
    if (custom) {
      if (d.protocol === "openai") {
        out.manifest = "openai-compatible";
        config.base_url = d.base_url.trim();
      } else {
        out.manifest = "anthropic";
        out.endpoints = [{ id: "messages", protocol: "anthropic", base_url: d.base_url.trim() }];
      }
    } else out.manifest = m!.name;
    if (d.allow_private) config.allow_private = true;
    if (Object.keys(config).length) out.config = config;
    return out;
  };

  const keep = async (id: string, name: string) => {
    kept.current = true;
    await unwrap(api.PATCH("/upstreams/{id}", { params: { path: { id } }, body: { enabled: true } }));
    await qc.invalidateQueries({ queryKey: ["upstreams"] });
    toast.success(t("gallery.added", { name }));
    void navigate({ to: "/upstreams", search: { id } });
  };

  const run = useMutation({
    mutationFn: async () => {
      const errs: Record<string, string> = { ...missingVariables(vars, d.config, t("form.required")) };
      if (custom && !/^https?:\/\/[^/]+/.test(d.base_url.trim())) errs.base_url = t("gallery.urlInvalid");
      if (sf !== "optional" && secrets.length === 0) errs.secret = t("form.required");
      if (Object.keys(errs).length) {
        form.setErrors(errs);
        throw new Error(t("form.fixErrors"));
      }
      setPhase({ step: "saving" });
      let id: string;
      let name: string;
      if (!draft.current) {
        const up = await unwrap(api.POST("/upstreams", { body: { ...body(), enabled: false } }));
        draft.current = { id: up.id, credIds: [] };
        id = up.id;
        name = up.name;
      } else {
        id = draft.current.id;
        const up = await unwrap(api.PATCH("/upstreams/{id}", { params: { path: { id } }, body: { ...body(), manifest: undefined } }));
        name = up.name;
        await Promise.all(draft.current.credIds.map((c) => api.DELETE("/credentials/{id}", { params: { path: { id: c } } })));
        draft.current.credIds = [];
      }
      // No key adds no credential: an upstream without credentials is routed keyless.
      for (const [i, secret] of secrets.entries()) {
        const c = await unwrap(
          api.POST("/upstreams/{id}/credentials", {
            params: { path: { id } },
            body: { label: secrets.length > 1 ? t("gallery.keyN", { n: i + 1 }) : t("gallery.key"), kind: "shared", secret },
          }),
        );
        draft.current.credIds.push(c.id);
      }
      if (!d.test_first) return { id, name, results: null };
      setPhase({ step: "testing", n: Math.max(1, secrets.length) });
      const r = await unwrap(api.POST("/upstreams/{id}/test", { params: { path: { id } }, body: d.test_model.trim() ? { model: d.test_model.trim() } : {} }));
      return { id, name, results: r };
    },
    onSuccess: async ({ id, name, results }) => {
      if (!results || results.ok) {
        try {
          await keep(id, name);
        } catch (e) {
          toast.error(errorMessage(e));
          setPhase({ step: "idle" });
        }
        return;
      }
      setPhase({ step: "failed", results: results.results });
    },
    onError: (e) => {
      setPhase({ step: "idle" });
      form.absorb(e);
      toast.error(errorMessage(e));
    },
  });

  const title = m ? manifestTitle(m, locale) : t("gallery.customTitle");
  const busy = run.isPending || phase.step === "saving" || phase.step === "testing";
  return (
    <form
      className="flex flex-col"
      onSubmit={(e) => {
        e.preventDefault();
        run.mutate();
      }}
    >
      <div className="flex items-start gap-3 border-b border-line px-5 py-4">
        {m ? (
          <ProviderAvatar name={m.name} title={title} size="lg" />
        ) : (
          <span className="grid size-10 shrink-0 place-items-center rounded-[10px] border border-dashed border-line-strong text-ink-3">
            <SquareDashed className="size-5" />
          </span>
        )}
        <div className="min-w-0 flex-1">
          <div className="font-semibold">{title}</div>
          <div className="text-xs text-ink-3">{m ? tagline(m) : t("gallery.customHint")}</div>
          {m?.docs_url && (
            <a href={m.docs_url} target="_blank" rel="noreferrer" className="mt-1 inline-flex items-center gap-1 text-xs text-accent hover:underline">
              {t("gallery.docs", { host: hostOf(m.docs_url) })}
              <ExternalLink className="size-3" />
            </a>
          )}
        </div>
        <Button size="icon-sm" variant="ghost" onClick={onClose} aria-label={t("common.close")} className="-mr-1.5 hidden min-[1180px]:inline-flex">
          <X />
        </Button>
      </div>
      <fieldset disabled={busy} className="grid gap-4 px-5 py-4">
        <Field label={t("upstreams.name")} hint={t("gallery.nameHint")} error={form.errors.name} optional>
          <Input value={d.name} onChange={(e) => form.set("name", e.target.value)} placeholder={m?.name ?? "my-llm"} />
        </Field>
        {custom && (
          <>
            <div className="grid gap-1.5">
              <span className="text-xs font-semibold">{t("gallery.speaks")}</span>
              <Segmented
                aria-label={t("gallery.speaks")}
                value={d.protocol}
                onChange={(p) => form.set("protocol", p)}
                options={[
                  { value: "openai", label: t("gallery.speaks.openai") },
                  { value: "anthropic", label: t("gallery.speaks.anthropic") },
                ]}
              />
            </div>
            <Field label={t("upstreams.baseUrl")} hint={t(d.protocol === "openai" ? "gallery.baseUrlOpenai" : "gallery.baseUrlAnthropic")} error={form.errors.base_url ?? form.errors.config}>
              <Input
                value={d.base_url}
                onChange={(e) => form.set("base_url", e.target.value)}
                placeholder={d.protocol === "openai" ? "http://127.0.0.1:8000/v1" : "https://llm.example.com"}
                className="font-mono text-[12.5px]"
                spellCheck={false}
              />
            </Field>
          </>
        )}
        <Field label={secretText.label} hint={secretText.hint} error={form.errors.secret}>
          <Textarea
            value={d.secret}
            onChange={(e) => form.set("secret", e.target.value)}
            className="min-h-[72px] font-mono text-[12.5px] [-webkit-text-security:disc]"
            autoComplete="off"
            spellCheck={false}
          />
        </Field>
        {secrets.length > 1 && <p className="-mt-2.5 text-xs text-accent-strong">{t("cred.willAdd", { n: secrets.length })}</p>}
        <ManifestVariableFields
          vars={vars}
          values={d.config}
          onChange={(name, value) => {
            form.set("config", { ...d.config, [name]: value });
            form.setErrors(({ [`config.${name}`]: _cleared, ...rest }) => rest);
          }}
          errors={form.errors}
        />
        <SwitchRow label={t("upstreams.modelSync")} description={t("gallery.modelSyncHint")} checked={d.model_sync} onChange={(v) => form.set("model_sync", v)} />
        <SwitchRow label={t("gallery.testFirst")} description={t("gallery.testFirstHint")} checked={d.test_first} onChange={(v) => form.set("test_first", v)} />
        <Disclosure title={t("gallery.more")}>
          <div className="grid gap-4 pt-1">
            <Field label={t("upstreams.proxy")} hint={t("upstreams.proxyHint")} error={form.errors.proxy} optional>
              <Input value={d.proxy} onChange={(e) => form.set("proxy", e.target.value)} placeholder="socks5://10.0.0.2:1080" className="font-mono text-[12.5px]" />
            </Field>
            <SwitchRow label={t("upstreams.allowPrivate")} description={t("upstreams.allowPrivateHint")} checked={d.allow_private} onChange={(v) => form.set("allow_private", v)} />
            <Field label={t("gallery.testModel")} hint={t("gallery.testModelHint")} optional>
              <Input value={d.test_model} onChange={(e) => form.set("test_model", e.target.value)} className="font-mono text-[12.5px]" spellCheck={false} />
            </Field>
          </div>
        </Disclosure>
      </fieldset>
      <div className="sticky bottom-0 grid gap-3 border-t border-line bg-surface px-5 py-3.5">
        {phase.step === "failed" && <FailedTest results={phase.results} />}
        {(phase.step === "saving" || phase.step === "testing") && (
          <p className="flex items-center gap-2 text-sm text-ink-2" role="status">
            <Loader2 className="size-4 animate-spin text-accent" />
            {phase.step === "saving" ? t("gallery.saving") : t("gallery.testing", { n: phase.n })}
          </p>
        )}
        <div className="flex flex-wrap justify-end gap-2">
          <Button onClick={onClose} disabled={busy}>
            {t("common.cancel")}
          </Button>
          {phase.step === "failed" && (
            <Button
              onClick={() => {
                const dr = draft.current;
                if (dr) keep(dr.id, d.name.trim() || title).catch((e) => toast.error(errorMessage(e)));
              }}
            >
              {t("gallery.saveAnyway")}
            </Button>
          )}
          <Button variant="primary" type="submit" disabled={busy}>
            {busy && <Loader2 className="animate-spin" />}
            {phase.step === "failed" ? t("gallery.retry") : d.test_first ? t("gallery.testAndSave") : t("gallery.save")}
          </Button>
        </div>
      </div>
    </form>
  );
}

function FailedTest({ results }: { results: TestResult[] }) {
  const { t } = useI18n();
  const failed = results.filter((r) => !r.ok);
  return (
    <div role="alert" className="rounded-lg border border-bad/30 bg-bad-soft px-3 py-2.5">
      <p className="text-sm font-medium text-bad">{t("gallery.testFailedTitle", { n: failed.length, of: results.length })}</p>
      <p className="mt-0.5 text-xs text-ink-2">{t("gallery.testFailedHint")}</p>
      <ul className="mt-2 grid max-h-40 gap-1.5 overflow-y-auto">
        {results.map((r, i) => (
          <li key={i} className="text-xs">
            <span className={cn("inline-flex items-center gap-1 font-medium", r.ok ? "text-ok" : "text-bad")}>
              {r.ok ? <Check className="size-3.5" /> : <X className="size-3.5" />}
              {r.ok ? t("upstreams.testPassed", { d: formatDuration(r.ttfb_ms ?? r.latency_ms ?? 0) }) : r.status ? t("upstreams.testStatus", { status: r.status }) : t("upstreams.testNoAnswer")}
              {r.model ? <span className="font-normal text-ink-3">· {r.model}</span> : null}
            </span>
            {r.error && <pre className="mt-0.5 whitespace-pre-wrap break-words font-mono text-[11px] text-ink-2">{r.error.slice(0, 400)}</pre>}
          </li>
        ))}
      </ul>
    </div>
  );
}

function Disclosure({ title, children }: { title: string; children: React.ReactNode }) {
  const [open, setOpen] = useState(false);
  return (
    <div>
      <button type="button" onClick={() => setOpen((v) => !v)} aria-expanded={open} className="inline-flex items-center gap-1 text-xs font-medium text-ink-2 hover:text-ink">
        <ChevronRight className={cn("size-3.5 transition-transform", open && "rotate-90")} />
        {title}
      </button>
      <div className={cn("mt-3", !open && "hidden")}>{children}</div>
    </div>
  );
}
