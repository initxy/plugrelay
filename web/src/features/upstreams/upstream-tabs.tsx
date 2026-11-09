import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { AlertTriangle, Plus, RefreshCw, Trash2 } from "lucide-react";
import { toast } from "sonner";
import { RAW_SLOT, WithRawValue } from "@/components/raw-value";
import { DetailList } from "@/components/drawer";
import { DurationInput, FormSection, HelpText, SuffixInput, SwitchRow } from "@/components/form-kit";
import { HourlyBar } from "@/components/hourly-bar";
import { EmptyState, ErrorState, LoadingRows } from "@/components/page";
import { Pill } from "@/components/status";
import { Button } from "@/components/ui/button";
import { Field } from "@/components/ui/field";
import { Input, Textarea } from "@/components/ui/input";
import { Segmented } from "@/components/ui/segmented";
import { Select } from "@/components/ui/select";
import { api, unwrap } from "@/lib/api/client";
import { errorMessage } from "@/lib/api/errors";
import type { Endpoint, ManifestVariable, TestResult, Upstream } from "@/lib/api/types";
import { cn } from "@/lib/cn";
import { formatClock, formatDuration, formatPercent, formatRelative } from "@/lib/format";
import { useI18n } from "@/i18n";
import { RouteBar, routeShares } from "@/features/models/route-bars";
import { hostOf, PICKABLE_PROTOCOLS, protocolKey, sortProtocols } from "./labels";
import { ManifestVariableFields } from "./manifest-variables";
import { useLastTest, useTestUpstreams } from "./use-upstream-test";
import type { Form } from "./upstream-draft";

export const TABS = ["overview", "credentials", "models", "advanced"] as const;
export type Tab = (typeof TABS)[number];

export function OverviewTab({ u, form, onTab }: { u: Upstream; form: Form; onTab: (t: Tab) => void }) {
  const { t, locale } = useI18n();
  const test = useTestUpstreams();
  const results = useLastTest(u.id);
  const models = useQuery({ queryKey: ["models"], queryFn: () => unwrap(api.GET("/models")) });
  const routed = (models.data?.items ?? []).filter((m) => m.routes.some((r) => r.upstream_id === u.id)).length;
  const okCreds = u.credentials.filter((c) => c.state.status === "ok").length;
  const rate = u.stats.success_rate_24h;
  return (
    <div className="grid gap-5">
      {u.circuit.state !== "closed" && (
        <div className={cn("flex items-start gap-3 rounded-lg border px-3.5 py-3", u.circuit.state === "open" ? "border-bad/30 bg-bad-soft" : "border-warn/30 bg-warn-soft")}>
          <AlertTriangle className={cn("mt-0.5 size-4 shrink-0", u.circuit.state === "open" ? "text-bad" : "text-warn")} />
          <div className="min-w-0 flex-1 text-sm">
            <p className={cn("font-medium", u.circuit.state === "open" ? "text-bad" : "text-warn")}>
              {u.circuit.state === "open" ? t("upstreams.pausedTitle") : t("upstreams.probing")}
            </p>
            <p className="mt-0.5 text-ink-2">
              {u.circuit.state === "open" && u.circuit.until
                ? t("upstreams.pausedBody", { at: formatClock(u.circuit.until, locale).slice(0, 5), rel: formatRelative(u.circuit.until, locale) })
                : t("upstreams.probingBody")}
            </p>
          </div>
          <Button size="sm" onClick={() => test.mutate({ ids: [u.id] })} disabled={test.isPending}>
            {t("upstreams.testNow")}
          </Button>
        </div>
      )}
      <section className="rounded-lg border border-line p-4">
        <div className="flex items-end justify-between gap-3">
          <div>
            <div className="text-xs text-ink-3">{t("upstreams.col.health")}</div>
            <div className={cn("mt-0.5 text-2xl font-semibold tabular-nums", rate !== null && rate < 0.95 && "text-bad")}>{rate === null ? "—" : formatPercent(rate, locale)}</div>
          </div>
          <div className="text-right text-xs text-ink-3">
            <div>{t("upstreams.reqCount", { n: u.stats.requests_24h })}</div>
            {u.stats.ttfb_p50_ms ? <div>{t("upstreams.ttfbMedian", { d: formatDuration(u.stats.ttfb_p50_ms) })}</div> : null}
          </div>
        </div>
        <HourlyBar hourly={u.stats.hourly} className="mt-3 h-6" />
        <div className="mt-1 flex justify-between text-2xs text-ink-3">
          <span>{t("upstreams.hoursAgo", { n: 24 })}</span>
          <span>{t("upstreams.now")}</span>
        </div>
      </section>
      <DetailList
        items={[
          {
            label: t("upstreams.col.endpoints"),
            value: (
              <ul className="grid gap-1">
                {sortProtocols(u.endpoints).map((e) => (
                  <li key={e.id} className="flex flex-wrap items-baseline gap-x-2">
                    <span>{t(protocolKey(e.protocol))}</span>
                    <span className="font-mono text-xs text-ink-3">{hostOf(e.base_url)}</span>
                  </li>
                ))}
              </ul>
            ),
          },
          {
            label: t("upstreams.col.credentials"),
            value: (
              <button type="button" className="text-left text-accent hover:underline" onClick={() => onTab("credentials")}>
                {u.credentials.length === 0 ? t(u.key_optional ? "cred.keyless" : "cred.needsKey") : t("upstreams.credsSummary", { n: u.credentials.length, ok: okCreds })}
              </button>
            ),
          },
          {
            label: t("upstreams.tab.models"),
            value: (
              <button type="button" className="text-left text-accent hover:underline" onClick={() => onTab("models")}>
                {t("upstreams.modelsSummary", { n: routed })}
              </button>
            ),
          },
        ]}
      />
      {results && <TestResults results={results} credentials={u.credentials} />}
      <FormSection title={t("upstreams.basic")}>
        <Field label={t("upstreams.name")} error={form.errors.name}>
          <Input value={form.values.name} onChange={(e) => form.set("name", e.target.value)} />
        </Field>
        <SwitchRow
          label={t("upstreams.enabled")}
          description={t("upstreams.enabledHint")}
          checked={form.values.enabled}
          onChange={(v) => form.set("enabled", v)}
        />
      </FormSection>
    </div>
  );
}

function TestResults({ results, credentials }: { results: TestResult[]; credentials: { id: string; label: string }[] }) {
  const { t } = useI18n();
  return (
    <section>
      <h3 className="mb-2 text-sm font-semibold">{t("upstreams.lastTest")}</h3>
      <ul className="grid gap-1.5">
        {results.map((r, i) => {
          const label = credentials.find((c) => c.id === r.credential_id)?.label ?? t("upstreams.noCredential");
          return (
            <li key={i} className={cn("rounded-md border px-3 py-2 text-sm", r.ok ? "border-line" : "border-bad/30 bg-bad-soft/50")}>
              <div className="flex flex-wrap items-center justify-between gap-2">
                <span className="font-medium">{label}</span>
                <span className={cn("text-xs tabular-nums", r.ok ? "text-ok" : "text-bad")}>
                  {r.ok
                    ? t("upstreams.testPassed", { d: formatDuration(r.ttfb_ms ?? r.latency_ms ?? 0) })
                    : r.status
                      ? t("upstreams.testStatus", { status: r.status })
                      : t("upstreams.testNoAnswer")}
                </span>
              </div>
              {r.model && <div className="text-xs text-ink-3">{t("upstreams.testModel", { model: r.model })}</div>}
              {r.error && <pre className="mt-1 max-h-28 overflow-auto whitespace-pre-wrap break-words font-mono text-[11.5px] text-ink-2">{r.error}</pre>}
              {r.request_id && (
                <Link to="/logs" search={{ id: r.request_id }} className="text-xs text-accent hover:underline">
                  {t("cred.viewRequest")}
                </Link>
              )}
            </li>
          );
        })}
      </ul>
    </section>
  );
}

export function ModelsTab({ u, syncOn, onSync }: { u: Upstream; syncOn: boolean; onSync: (v: boolean) => void }) {
  const { t } = useI18n();
  const qc = useQueryClient();
  const models = useQuery({ queryKey: ["models"], queryFn: () => unwrap(api.GET("/models")) });
  const sync = useMutation({
    mutationFn: () => unwrap(api.POST("/models/sync", { body: { upstream_ids: [u.id] } })),
    onSuccess: (r) => {
      void qc.invalidateQueries({ queryKey: ["models"] });
      if (r.failed.length) toast.error(t("models.syncFailed"), { description: r.failed.map((f) => f.error).join("\n") });
      else toast.success(t("models.syncDone", { added: r.added.length, disabled: r.disabled.length, unchanged: r.unchanged }));
    },
    onError: (e) => toast.error(errorMessage(e)),
  });
  const rows = (models.data?.items ?? []).flatMap((m) => {
    const shares = routeShares(m.routes);
    return shares.filter((s) => s.route.upstream_id === u.id).map((s) => ({ model: m, ...s }));
  });
  return (
    <div className="grid gap-5">
      <div className="grid gap-3 rounded-lg border border-line p-4">
        <SwitchRow label={t("upstreams.modelSync")} description={t("upstreams.modelSyncHint")} checked={syncOn} onChange={onSync} />
        <div className="flex items-center justify-between gap-3 border-t border-line-soft pt-3">
          <HelpText>{t("upstreams.syncNowHint")}</HelpText>
          <Button size="sm" onClick={() => sync.mutate()} disabled={sync.isPending}>
            <RefreshCw className={cn(sync.isPending && "animate-spin")} />
            {t("upstreams.syncNow")}
          </Button>
        </div>
      </div>
      <section>
        <div className="mb-2 flex items-center justify-between gap-2">
          <h3 className="text-sm font-semibold">{t("upstreams.servedModels", { n: rows.length })}</h3>
          <Button size="sm" variant="ghost" asChild>
            <Link to="/models" search={{ id: undefined }}>
              <Plus />
              {t("upstreams.addRouteHere")}
            </Link>
          </Button>
        </div>
        {models.isPending ? (
          <LoadingRows rows={4} />
        ) : models.isError ? (
          <ErrorState error={models.error} onRetry={() => void models.refetch()} />
        ) : rows.length === 0 ? (
          <div className="rounded-lg border border-dashed border-line-strong">
            <EmptyState compact title={t("upstreams.noModels")} hint={t("upstreams.noModelsHint")} />
          </div>
        ) : (
          <ul className="divide-y divide-line-soft rounded-lg border border-line">
            {rows.map(({ model: m, route, tier, share }) => (
              <li key={`${m.id}-${route.upstream_model}`} className="flex flex-wrap items-center gap-x-4 gap-y-1 px-3.5 py-2.5">
                <div className="min-w-0 flex-1">
                  <Link to="/models" search={{ id: m.id }} className="font-mono text-[13px] font-medium hover:text-accent hover:underline">
                    {m.name}
                  </Link>
                  {route.upstream_model !== m.name && (
                    <div className="truncate text-xs text-ink-3"><WithRawValue text={t("upstreams.asUpstreamModel", { model: RAW_SLOT })} value={route.upstream_model} /></div>
                  )}
                </div>
                {route.groups?.length ? <Pill tone="info">{t("route.groupsOnly", { n: route.groups.length })}</Pill> : null}
                {!m.enabled && <Pill tone="neutral">{t("models.state.off")}</Pill>}
                <RouteBar tier={tier} share={share} />
              </li>
            ))}
          </ul>
        )}
      </section>
    </div>
  );
}

export function AdvancedTab({ form, declared }: { form: Form; declared: ManifestVariable[] }) {
  const { t } = useI18n();
  const d = form.values;
  const setEndpoint = (i: number, patch: Partial<Endpoint>) => form.set("endpoints", d.endpoints.map((e, j) => (j === i ? { ...e, ...patch } : e)));
  return (
    <div>
      <FormSection title={t("upstreams.adv.endpoints")} description={t("upstreams.adv.endpointsHint")}>
        <div className="grid gap-2">
          {d.endpoints.map((e, i) => {
            const pickable = (PICKABLE_PROTOCOLS as readonly string[]).includes(e.protocol);
            const err = form.errors[`endpoints.${i}.base_url`];
            return (
              <div key={i} className="grid grid-cols-[minmax(130px,170px)_1fr_auto] items-start gap-2">
                {pickable ? (
                  <Select
                    aria-label={t("upstreams.protocol")}
                    value={e.protocol}
                    onChange={(p) => setEndpoint(i, { protocol: p })}
                    options={PICKABLE_PROTOCOLS.map((p) => ({ value: p, label: t(protocolKey(p)) }))}
                  />
                ) : (
                  <span className="flex h-8 items-center text-sm text-ink-2">{t(protocolKey(e.protocol))}</span>
                )}
                <div>
                  <Input
                    aria-label={t("upstreams.baseUrl")}
                    aria-invalid={Boolean(err)}
                    value={e.base_url}
                    onChange={(ev) => setEndpoint(i, { base_url: ev.target.value })}
                    className="font-mono text-[12.5px]"
                    spellCheck={false}
                  />
                  {err && <p className="mt-0.5 text-xs text-bad">{err}</p>}
                </div>
                <Button size="icon-sm" variant="ghost" className="mt-0.5" aria-label={t("common.remove")} onClick={() => form.set("endpoints", d.endpoints.filter((_, j) => j !== i))}>
                  <Trash2 />
                </Button>
              </div>
            );
          })}
          <div>
            <Button
              size="sm"
              variant="ghost"
              onClick={() => form.set("endpoints", [...d.endpoints, { id: `ep${d.endpoints.length + 1}`, protocol: "openai-chat", base_url: "https://" }])}
            >
              <Plus />
              {t("upstreams.addEndpoint")}
            </Button>
          </div>
          {form.errors.endpoints && <p className="text-xs text-bad">{form.errors.endpoints}</p>}
        </div>
      </FormSection>
      {declared.length > 0 && (
        <FormSection title={t("upstreams.adv.variables")} description={t("upstreams.adv.variablesHint")}>
          <ManifestVariableFields
            vars={declared}
            values={d.config}
            onChange={(name, value) => {
              form.set("config", { ...d.config, [name]: value });
              form.setErrors(({ [`config.${name}`]: _cleared, ...rest }) => rest);
            }}
            errors={form.errors}
          />
          {form.errors.config && <p className="text-xs text-bad">{form.errors.config}</p>}
        </FormSection>
      )}
      <FormSection title={t("upstreams.adv.network")}>
        <Field label={t("upstreams.proxy")} hint={t("upstreams.proxyHint")} error={form.errors.proxy} optional>
          <Input value={d.proxy} onChange={(e) => form.set("proxy", e.target.value)} placeholder="socks5://10.0.0.2:1080" className="font-mono text-[12.5px]" spellCheck={false} />
        </Field>
        <SwitchRow label={t("upstreams.allowPrivate")} description={t("upstreams.allowPrivateHint")} checked={d.allow_private} onChange={(v) => form.set("allow_private", v)} />
        <Field label={t("upstreams.headers")} hint={t("upstreams.headersHint")} error={Object.entries(form.errors).find(([k]) => k.startsWith("headers"))?.[1]} optional>
          <Textarea value={d.headers} onChange={(e) => form.set("headers", e.target.value)} className="font-mono text-[12.5px]" placeholder="x-custom-header: value" spellCheck={false} />
        </Field>
      </FormSection>
      <FormSection title={t("upstreams.adv.timeouts")} description={t("upstreams.timeoutsHint")}>
        <div className="grid items-start gap-3 sm:grid-cols-2">
          {(
            [
              ["connect", "upstreams.connect"],
              ["first_byte", "upstreams.firstByte"],
              ["idle", "upstreams.idle"],
              ["total", "upstreams.total"],
            ] as const
          ).map(([k, label]) => (
            <Field key={k} label={t(label)} error={k === "connect" ? form.errors.timeouts : undefined}>
              <SuffixInput suffix={t("upstreams.secondsUnit")} inputMode="decimal" value={d[k]} onChange={(e) => form.set(k, e.target.value)} placeholder={t("upstreams.default")} />
            </Field>
          ))}
        </div>
      </FormSection>
      <FormSection title={t("upstreams.adv.checks")}>
        <Field label={t("upstreams.scheduledTest")} hint={t("upstreams.scheduledTestHint")} error={form.errors.scheduled_test} optional>
          <DurationInput value={d.scheduled_test} onChange={(v) => form.set("scheduled_test", v)} units={["m", "h"]} />
        </Field>
        <div className="grid gap-1.5">
          <span className="text-xs font-semibold">{t("upstreams.quotaProbe")}</span>
          <div className="flex flex-wrap items-center gap-2">
            <Segmented
              aria-label={t("upstreams.quotaProbe")}
              value={d.probe_mode}
              onChange={(v) => form.set("probe_mode", v)}
              options={[
                { value: "default", label: t("upstreams.probe.default") },
                { value: "off", label: t("upstreams.probe.off") },
                { value: "custom", label: t("upstreams.probe.custom") },
              ]}
            />
            {d.probe_mode === "custom" && (
              <DurationInput value={d.probe_every} onChange={(v) => form.set("probe_every", v)} units={["m", "h"]} aria-label={t("upstreams.probe.every")} />
            )}
          </div>
          <p className={cn("text-xs", form.errors.quota_probe ? "text-bad" : "text-ink-3")}>{form.errors.quota_probe ?? t("upstreams.quotaProbeHint")}</p>
        </div>
      </FormSection>
    </div>
  );
}
