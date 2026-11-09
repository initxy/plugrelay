import { useQuery } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { Brain, Eraser, KeyRound, MessageSquareText, Send, Square } from "lucide-react";
import { useEffect, useRef, useState, type KeyboardEvent } from "react";
import { EmptyState, PageHeader, Panel } from "@/components/page";
import { Button } from "@/components/ui/button";
import { Field } from "@/components/ui/field";
import { Input, Textarea } from "@/components/ui/input";
import { Segmented } from "@/components/ui/segmented";
import { Select } from "@/components/ui/select";
import { useMoney } from "@/components/money";
import { isOperator } from "@/app/nav";
import { useMe } from "@/app/session";
import { api, unwrap } from "@/lib/api/client";
import { ApiError, errorMessage } from "@/lib/api/errors";
import type { PlaygroundRequest } from "@/lib/api/types";
import { cn } from "@/lib/cn";
import { formatDuration, formatNumber } from "@/lib/format";
import { store } from "@/lib/storage";
import { useI18n } from "@/i18n";
import { streamPlayground, type PlaygroundEvent } from "./stream";
import { useLogsPath } from "@/app/session";

type Protocol = PlaygroundRequest["protocol"];
type Effort = NonNullable<PlaygroundRequest["reasoning_effort"]>;
type Done = Extract<PlaygroundEvent, { type: "done" }>;

const PROTOCOLS: readonly { value: Protocol; label: string }[] = [
  { value: "anthropic", label: "Anthropic" },
  { value: "openai-chat", label: "OpenAI Chat" },
  { value: "openai-responses", label: "OpenAI Responses" },
  { value: "gemini", label: "Gemini" },
];

/** The admin's own identity instead of a key. */
const AS_ME = "__me";

interface Turn {
  role: "user" | "assistant";
  text: string;
  reasoning?: string;
  done?: Done;
  failed?: string;
}

const remembered = (k: string) => store.get("pr.playground." + k) ?? undefined;
const remember = (k: string, v: string) => store.set("pr.playground." + k, v);

/** Talk to a model through the relay with one of your keys: a real, logged and billed request. */
export function PlaygroundPage() {
  const { t } = useI18n();
  const me = useMe();
  const admin = isOperator(me);
  const keys = useQuery({ queryKey: ["keys"], queryFn: () => unwrap(api.GET("/keys")) });
  const catalog = useQuery({ queryKey: ["catalog"], queryFn: () => unwrap(api.GET("/catalog")) });
  const usable = (keys.data?.items ?? []).filter((k) => k.status === "active" && (!admin || k.owner?.id === me.user.id || !k.owner));

  const [keyId, setKeyId] = useState<string | undefined>(remembered("key"));
  const [model, setModel] = useState<string | undefined>(remembered("model"));
  const [protocol, setProtocol] = useState<Protocol>((remembered("protocol") as Protocol | undefined) ?? "anthropic");
  const [system, setSystem] = useState("");
  const [temperature, setTemperature] = useState("");
  const [maxTokens, setMaxTokens] = useState("1024");
  const [effort, setEffort] = useState<Effort | "none">("none");
  const [turns, setTurns] = useState<Turn[]>([]);
  const [draft, setDraft] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();
  const abort = useRef<AbortController | null>(null);
  const end = useRef<HTMLDivElement>(null);

  // Fall back to a key and a model that exist.
  const keyOptions = [...(admin ? [{ value: AS_ME, label: t("playground.asMe") }] : []), ...usable.map((k) => ({ value: k.id, label: k.name }))];
  const models = catalog.data?.items ?? [];
  const pickedKey = keyOptions.find((o) => o.value === keyId)?.value ?? keyOptions[0]?.value;
  const pickedModel = models.find((m) => m.model === model)?.model ?? models.find((m) => m.kind === "model")?.model;

  useEffect(() => end.current?.scrollIntoView?.({ block: "end" }), [turns]);

  const update = (fn: (last: Turn) => Turn) => setTurns((ts) => [...ts.slice(0, -1), fn(ts[ts.length - 1]!)]);

  const send = async () => {
    const text = draft.trim();
    if (!text || !pickedModel || !pickedKey || busy) return;
    setError(undefined);
    const history = [...turns.filter((x) => !x.failed), { role: "user" as const, text }];
    setTurns([...history, { role: "assistant", text: "" }]);
    setDraft("");
    setBusy(true);
    const temp = Number(temperature.replace(",", "."));
    const max = Number(maxTokens);
    const req: PlaygroundRequest = {
      protocol,
      model: pickedModel,
      messages: history.map((x) => ({ role: x.role, content: x.text })),
      ...(pickedKey !== AS_ME ? { key_id: pickedKey } : {}),
      ...(system.trim() ? { system: system.trim() } : {}),
      ...(temperature.trim() && Number.isFinite(temp) ? { temperature: temp } : {}),
      ...(Number.isInteger(max) && max > 0 ? { max_tokens: max } : {}),
      ...(effort !== "none" ? { reasoning_effort: effort } : {}),
    };
    const ctl = new AbortController();
    abort.current = ctl;
    try {
      await streamPlayground(
        req,
        (e) => {
          if (e.type === "text") update((l) => ({ ...l, text: l.text + e.text }));
          else if (e.type === "reasoning") update((l) => ({ ...l, reasoning: (l.reasoning ?? "") + e.text }));
          else if (e.type === "done") update((l) => ({ ...l, done: e, failed: e.ok ? undefined : e.error || t("playground.failed") }));
        },
        ctl.signal,
      );
    } catch (e) {
      if (ctl.signal.aborted) update((l) => ({ ...l, failed: l.done ? l.failed : t("playground.stopped") }));
      else {
        setTurns(history.slice(0, -1));
        setDraft(text);
        setError(e instanceof ApiError ? errorMessage(e) : t("playground.failed"));
      }
    } finally {
      setBusy(false);
      abort.current = null;
    }
  };

  const onKey = (e: KeyboardEvent<HTMLTextAreaElement>) => {
    if (e.key === "Enter" && (e.metaKey || e.ctrlKey)) {
      e.preventDefault();
      void send();
    }
  };

  const noKey = !admin && keys.data && usable.length === 0;

  return (
    <>
      <PageHeader title={t("nav.playground")} description={t("playground.description")} />
      <div className="grid gap-4 lg:grid-cols-[300px_minmax(0,1fr)]">
        <Panel title={t("playground.settings")} bodyClassName="grid gap-4">
          <Field label={t("playground.key")} hint={admin ? t("playground.keyHintAdmin") : undefined}>
            <Select
              value={pickedKey}
              onChange={(v) => {
                setKeyId(v);
                remember("key", v);
              }}
              options={keyOptions}
              placeholder={t("playground.pickKey")}
            />
          </Field>
          <Field label={t("playground.model")}>
            <Select
              value={pickedModel}
              onChange={(v) => {
                setModel(v);
                remember("model", v);
              }}
              options={models.map((m) => ({
                value: m.model,
                label: m.kind === "preset" ? `${m.model} · ${t("catalog.preset")}` : m.model,
              }))}
              placeholder={t("playground.pickModel")}
            />
          </Field>
          <Field label={t("playground.protocol")} hint={t("playground.protocolHint")}>
            <Select
              value={protocol}
              onChange={(v) => {
                setProtocol(v);
                remember("protocol", v);
              }}
              options={PROTOCOLS}
            />
          </Field>
          <Field label={t("playground.system")} optional>
            <Textarea value={system} onChange={(e) => setSystem(e.target.value)} placeholder={t("playground.systemPlaceholder")} rows={3} />
          </Field>
          <div className="grid grid-cols-2 gap-3">
            <Field label={t("playground.temperature")} optional>
              <Input inputMode="decimal" value={temperature} onChange={(e) => setTemperature(e.target.value)} placeholder="1" />
            </Field>
            <Field label={t("playground.maxTokens")}>
              <Input inputMode="numeric" value={maxTokens} onChange={(e) => setMaxTokens(e.target.value)} />
            </Field>
          </div>
          <Field label={t("playground.effort")}>
            <Segmented
              value={effort}
              onChange={setEffort}
              size="sm"
              options={[
                { value: "none", label: t("playground.effort.none") },
                { value: "low", label: t("playground.effort.low") },
                { value: "medium", label: t("playground.effort.medium") },
                { value: "high", label: t("playground.effort.high") },
              ]}
            />
          </Field>
        </Panel>

        <section className="flex min-h-[560px] flex-col rounded-lg border border-line bg-surface" aria-label={t("playground.conversation")}>
          <div className="flex min-h-10 items-center justify-between gap-2 border-b border-line-soft px-4 py-2">
            <h2 className="text-sm font-semibold">{t("playground.conversation")}</h2>
            <Button size="xs" variant="ghost" onClick={() => setTurns([])} disabled={busy || turns.length === 0}>
              <Eraser />
              {t("playground.clear")}
            </Button>
          </div>
          <div className="flex-1 overflow-y-auto px-4 py-4">
            {noKey ? (
              <EmptyState
                icon={KeyRound}
                title={t("playground.noKey")}
                hint={t("playground.noKeyHint")}
                action={
                  <Button asChild variant="primary">
                    <Link to="/keys">{t("playground.createKey")}</Link>
                  </Button>
                }
              />
            ) : turns.length === 0 ? (
              <EmptyState icon={MessageSquareText} title={t("playground.empty")} hint={t("playground.emptyHint")} />
            ) : (
              <ol className="grid gap-4">
                {turns.map((x, i) => (
                  <TurnView key={i} turn={x} streaming={busy && i === turns.length - 1} />
                ))}
              </ol>
            )}
            <div ref={end} />
          </div>
          <div className="border-t border-line-soft p-3">
            {error && (
              <p role="alert" className="mb-2 text-sm text-bad">
                {error}
              </p>
            )}
            <div className="flex items-end gap-2">
              <Textarea
                value={draft}
                onChange={(e) => setDraft(e.target.value)}
                onKeyDown={onKey}
                placeholder={t("playground.placeholder")}
                aria-label={t("playground.message")}
                rows={2}
                className="min-h-14 flex-1 resize-y"
                disabled={Boolean(noKey)}
              />
              {busy ? (
                <Button onClick={() => abort.current?.abort()}>
                  <Square />
                  {t("playground.stop")}
                </Button>
              ) : (
                <Button variant="primary" onClick={() => void send()} disabled={!draft.trim() || !pickedModel || !pickedKey}>
                  <Send />
                  {t("playground.send")}
                </Button>
              )}
            </div>
            <p className="mt-1.5 text-2xs text-ink-3">{t("playground.footnote")}</p>
          </div>
        </section>
      </div>
    </>
  );
}

function TurnView({ turn, streaming }: { turn: Turn; streaming: boolean }) {
  const { t } = useI18n();
  if (turn.role === "user")
    return (
      <li className="ml-auto max-w-[80%] whitespace-pre-wrap rounded-lg rounded-br-sm bg-accent-soft px-3.5 py-2 text-sm text-ink">{turn.text}</li>
    );
  return (
    <li className="max-w-[92%]">
      {turn.reasoning && (
        <details className="mb-1.5 rounded-md border border-line-soft bg-surface-2 px-3 py-1.5 text-xs text-ink-2" open={streaming && !turn.text}>
          <summary className="flex cursor-pointer items-center gap-1.5 font-medium text-ink-3">
            <Brain className="size-3.5" />
            {t("playground.thinking")}
          </summary>
          <p className="mt-1.5 whitespace-pre-wrap">{turn.reasoning}</p>
        </details>
      )}
      <div className={cn("whitespace-pre-wrap text-sm leading-relaxed text-ink", turn.failed && !turn.text && "text-bad")}>
        {turn.text || (turn.failed ? turn.failed : streaming ? <span className="text-ink-3">{t("playground.waiting")}</span> : null)}
        {streaming && turn.text && <span className="ml-0.5 inline-block h-4 w-1.5 animate-pulse bg-ink-3 align-text-bottom" aria-hidden />}
      </div>
      {turn.failed && turn.text && <p className="mt-1 text-xs text-bad">{turn.failed}</p>}
      {turn.done && <TurnFacts done={turn.done} />}
    </li>
  );
}

/** First token, total time, tokens, what it cost, and the request's log entry. */
function TurnFacts({ done }: { done: Done }) {
  const { t, locale } = useI18n();
  const money = useMoney();
  const id = done.request_id;
  // The log entry is written right after the answer ends; ask a few times.
  const log = useQuery({
    queryKey: ["logs", id],
    queryFn: () => unwrap(api.GET("/logs/{id}", { params: { path: { id: id! } } })),
    enabled: Boolean(id),
    retry: (n, e) => n < 6 && e instanceof ApiError && e.status === 404,
    retryDelay: 600,
  });
  const logs = useLogsPath();
  const u = done.usage;
  const facts = [
    done.ttfb_ms ? t("playground.ttfb", { d: formatDuration(done.ttfb_ms) }) : null,
    done.latency_ms ? t("playground.total", { d: formatDuration(done.latency_ms) }) : null,
    u ? t("playground.tokens", { input: formatNumber(u.input + u.cache_read + u.cache_write_5m + u.cache_write_1h, locale), output: formatNumber(u.output, locale) }) : null,
    log.data ? t("playground.cost", { amount: money(log.data.charge) }) : null,
  ].filter(Boolean);
  return (
    <p className="mt-1.5 flex flex-wrap items-center gap-x-2 gap-y-1 text-xs text-ink-3">
      {facts.join(" · ")}
      {id && (
        <Link to={logs} search={{ id }} className="font-medium text-accent hover:underline">
          {t("playground.viewLog")}
        </Link>
      )}
    </p>
  );
}
