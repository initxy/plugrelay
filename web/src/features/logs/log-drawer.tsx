import { useQuery } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { FileX2 } from "lucide-react";
import { useState, type ReactNode } from "react";
import { ClientName } from "@/components/client-name";
import { RAW_SLOT, WithRawValue } from "@/components/raw-value";
import { JsonView } from "@/components/code-block";
import { useMoney } from "@/components/money";
import { CopyButton } from "@/components/copy-button";
import { DetailList, Drawer } from "@/components/drawer";
import { EmptyState, ErrorState, LoadingRows } from "@/components/page";
import { Pill } from "@/components/status";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { api, unwrap } from "@/lib/api/client";
import type { LogDetail, Usage } from "@/lib/api/types";
import { formatDateTime, formatDuration, formatNumber, formatPercent, formatWhen } from "@/lib/format";
import { useI18n } from "@/i18n";
import { AttemptDetail, AttemptWaterfall } from "./attempts";
import { CostBreakdown } from "./cost";
import { protocolName } from "./diagnostics";
import { HandlingPill, RetryPills } from "./request-pills";

type Item = { label: string; value: ReactNode };

const USAGE_KEYS = ["input", "cache_read", "cache_write_5m", "cache_write_1h", "output", "reasoning"] as const;

function Section({ title, aside, children }: { title: string; aside?: ReactNode; children: ReactNode }) {
  return (
    <section className="grid gap-2.5">
      <div className="flex items-center justify-between gap-2">
        <h3 className="text-sm font-semibold">{title}</h3>
        {aside}
      </div>
      {children}
    </section>
  );
}

function Tokens({ usage }: { usage: Usage }) {
  const { t, locale } = useI18n();
  return (
    <div className="grid grid-cols-3 gap-px overflow-hidden rounded-lg border border-line bg-line max-sm:grid-cols-2">
      {USAGE_KEYS.map((k) => (
        <div key={k} className="bg-surface px-3 py-2">
          <div className="text-xs text-ink-3">{t(`usage.${k}`)}</div>
          <div className="mt-0.5 text-sm font-medium tabular-nums">{formatNumber(usage[k], locale)}</div>
        </div>
      ))}
    </div>
  );
}

function Overview({ d }: { d: LogDetail }) {
  const { t, locale } = useI18n();
  const money = useMoney();
  const last = d.attempts_detail.at(-1);
  const failed = d.status < 200 || d.status >= 300;
  const swapped = d.served_model && d.served_model !== d.model;
  const rows: (Item | null)[] = [
    {
      label: t("logs.result"),
      value: (
        <span className="flex flex-wrap items-center gap-1.5">
          {failed ? <HandlingPill log={d} /> : <Pill tone="ok">{t("logs.okPill", { status: d.status })}</Pill>}
          {!failed && <HandlingPill log={d} />}
          <RetryPills log={d} />
        </span>
      ),
    },
    failed && d.error ? { label: t("logs.whyFailed"), value: <span className="text-bad">{d.error}</span> } : null,
    {
      label: t("logs.model"),
      value: (
        <span>
          {swapped ? t("logs.modelViaPreset", { preset: d.model, model: d.served_model ?? "" }) : d.model}
          {d.upstream_model && d.upstream_model !== (d.served_model ?? d.model) && (
            <span className="text-ink-3"> · <WithRawValue text={t("logs.upstreamCalls", { name: RAW_SLOT })} value={d.upstream_model} /></span>
          )}
        </span>
      ),
    },
    last
      ? {
          label: t("logs.handling"),
          value: t(`tier.${last.tier}.long`, { from: protocolName(d.protocol), to: protocolName(last.protocol) }),
        }
      : null,
    d.upstream ? { label: t("logs.upstream"), value: d.upstream.name } : null,
    {
      label: t("logs.key"),
      value: d.key ? (d.user ? t("logs.keyOfUser", { key: d.key.name, user: d.user.name }) : d.key.name) : "—",
    },
    {
      label: t("logs.client"),
      value: (
        <span className="grid gap-0.5">
          <span className="flex flex-wrap items-center gap-x-1">
            <ClientName client={d.client} showVersion />
            {d.client_ip && <span className="text-ink-3">· {t("logs.fromIp", { ip: d.client_ip })}</span>}
          </span>
          {d.client_ua && <span className="break-all font-mono text-xs text-ink-3">{d.client_ua}</span>}
        </span>
      ),
    },
    { label: t("logs.charged"), value: <span className="tabular-nums">{money(d.charge)}</span> },
    { label: t("logs.time"), value: formatDateTime(d.time, locale) },
    {
      label: t("logs.ttfbTotal"),
      value: `${d.ttfb_ms !== null && d.ttfb_ms !== undefined ? formatDuration(d.ttfb_ms) : "—"} / ${formatDuration(d.duration_ms)}${
        d.stream ? ` · ${t("logs.streamed")}` : ""
      }`,
    },
    d.cache_hit_rate !== null && d.cache_hit_rate !== undefined
      ? { label: t("logs.cacheHit"), value: formatPercent(d.cache_hit_rate, locale, 0) }
      : null,
    d.session_id
      ? {
          label: t("logs.session"),
          value: (
            <Link to="." search={{ session_id: d.session_id }} className="text-accent hover:underline">
              {d.session
                ? t("logs.sessionTotal", { turn: d.session.turn, n: d.session.requests, amount: money(d.session.charge) })
                : t("logs.sessionLink")}
            </Link>
          ),
        }
      : null,
  ];
  const items = rows.filter((x): x is Item => x !== null);
  return (
    <div className="grid gap-5">
      <DetailList items={items} className="grid-cols-[minmax(96px,max-content)_1fr]" />
      {d.attempts_detail.length > 0 && (
        <Section title={t("logs.tabAttempts")}>
          <AttemptWaterfall attempts={d.attempts_detail} total={d.duration_ms} />
        </Section>
      )}
      <Section title={t("logs.tokens")} aside={d.usage.estimated ? <Pill tone="warn">{t("logs.estimated")}</Pill> : undefined}>
        <Tokens usage={d.usage} />
      </Section>
    </div>
  );
}

function Attempts({ d }: { d: LogDetail }) {
  const { t } = useI18n();
  if (d.attempts_detail.length === 0) return <EmptyState compact title={t("logs.noAttempts")} hint={t("logs.noAttemptsHint")} />;
  return (
    <div className="grid gap-5">
      <AttemptWaterfall attempts={d.attempts_detail} total={d.duration_ms} />
      <div className="grid gap-3">
        {d.attempts_detail.map((a) => (
          <AttemptDetail key={a.n} a={a} clientProtocol={d.protocol} />
        ))}
      </div>
    </div>
  );
}

function Content({ d }: { d: LogDetail }) {
  const { t } = useI18n();
  if (!d.body || (!d.body.request && !d.body.response))
    return <EmptyState compact icon={FileX2} title={t("logs.noBody")} hint={t("logs.noBodyHint")} />;
  return (
    <div className="grid gap-4">
      {d.body.request !== undefined && <JsonView value={d.body.request} title={t("logs.bodyRequest")} />}
      {d.body.response !== undefined && <JsonView value={d.body.response} title={t("logs.bodyResponse")} />}
    </div>
  );
}

function Raw({ d }: { d: LogDetail }) {
  const { t } = useI18n();
  const { body: _body, ...rest } = d;
  return (
    <div className="grid gap-4">
      <Section title={t("logs.rawUsage")} aside={d.usage.estimated ? <Pill tone="warn">{t("logs.estimated")}</Pill> : undefined}>
        <JsonView value={d.usage.raw ?? {}} />
      </Section>
      <Section title={t("logs.rawRecord")}>
        <JsonView value={rest} />
      </Section>
    </div>
  );
}

type Tab = "overview" | "attempts" | "cost" | "content" | "raw";

/** The right-side panel of one request: overview, upstream attempts, cost, stored bodies, raw data. */
export function LogDrawer({ id, onOpenChange }: { id: string | undefined; onOpenChange: (open: boolean) => void }) {
  const { t, locale } = useI18n();
  const [tab, setTab] = useState<Tab>("overview");
  const q = useQuery({
    queryKey: ["log", id],
    queryFn: () => unwrap(api.GET("/logs/{id}", { params: { path: { id: id! } } })),
    enabled: Boolean(id),
  });
  const d = q.data;
  const failed = d ? d.status < 200 || d.status >= 300 : false;
  return (
    <Tabs value={tab} onValueChange={(v) => setTab(v as Tab)}>
      <Drawer
        open={Boolean(id)}
        onOpenChange={(o) => {
          if (!o) setTab("overview");
          onOpenChange(o);
        }}
        size="lg"
        title={d ? `${d.served_model ?? d.model} · ${formatWhen(d.time, locale)}` : t("logs.request")}
        subtitle={<span className="font-mono">{id}</span>}
        meta={d && (failed ? <Pill tone="bad" dot>{t("logs.failed")}</Pill> : <Pill tone="ok" dot>{t("logs.ok")}</Pill>)}
        actions={id && <CopyButton text={typeof location !== "undefined" ? location.href : ""} label={t("logs.copyLink")} variant="ghost" />}
        tabs={
          <TabsList>
            <TabsTrigger value="overview">{t("logs.tabOverview")}</TabsTrigger>
            <TabsTrigger value="attempts">
              {t("logs.tabAttempts")}
              {d && d.attempts_detail.length > 1 && <span className="ml-1 tabular-nums text-ink-3">{d.attempts_detail.length}</span>}
            </TabsTrigger>
            <TabsTrigger value="cost">{t("logs.tabCost")}</TabsTrigger>
            <TabsTrigger value="content">{t("logs.tabContent")}</TabsTrigger>
            <TabsTrigger value="raw">{t("logs.tabRaw")}</TabsTrigger>
          </TabsList>
        }
      >
        <div className="p-5">
          {q.isPending ? (
            <LoadingRows rows={8} />
          ) : q.isError ? (
            <ErrorState error={q.error} onRetry={() => void q.refetch()} />
          ) : (
            d && (
              <>
                <TabsContent value="overview">
                  <Overview d={d} />
                </TabsContent>
                <TabsContent value="attempts">
                  <Attempts d={d} />
                </TabsContent>
                <TabsContent value="cost">
                  <CostBreakdown billing={d.billing} attempts={d.attempts_detail} />
                </TabsContent>
                <TabsContent value="content">
                  <Content d={d} />
                </TabsContent>
                <TabsContent value="raw">
                  <Raw d={d} />
                </TabsContent>
              </>
            )
          )}
        </div>
      </Drawer>
    </Tabs>
  );
}
