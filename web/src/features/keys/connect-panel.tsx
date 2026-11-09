import { useMutation, useQuery } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { ChevronRight, Download, Info, PlugZap } from "lucide-react";
import { useState, type ReactNode } from "react";
import { CodeBlock } from "@/components/code-block";
import { ErrorState } from "@/components/page";
import { StatusText } from "@/components/status";
import { Button } from "@/components/ui/button";
import { Segmented } from "@/components/ui/segmented";
import { Skeleton } from "@/components/ui/skeleton";
import { api, unwrap } from "@/lib/api/client";
import { errorMessage } from "@/lib/api/errors";
import type { ClientId, ClientOS, ConnectInfo } from "@/lib/api/types";
import { cn } from "@/lib/cn";
import { formatDuration } from "@/lib/format";
import { Tooltip } from "@/components/ui/tooltip";
import { useI18n } from "@/i18n";
import { useLogsPath } from "@/app/session";

/** Replace the API's `{{api_key}}` placeholder. Without the secret (it is only shown once), use a visible marker. */
export function fillKey(text: string, placeholder: string, secret: string | undefined, fallback: string): string {
  return text.split(placeholder).join(secret ?? fallback);
}

/**
 * The CC Switch import link with the key in it (URL-escaped), or undefined without the secret:
 * CC Switch refuses a provider without a key, so a placeholder would import a broken one.
 */
export function ccSwitchLink(url: string, placeholder: string, secret: string | undefined): string | undefined {
  return secret ? url.split(placeholder).join(encodeURIComponent(secret)) : undefined;
}

function detectOS(): ClientOS {
  const p = typeof navigator !== "undefined" ? navigator.userAgent : "";
  if (/Windows/i.test(p)) return "windows";
  if (/Mac/i.test(p)) return "macos";
  return "linux";
}

const OS_OPTIONS: { value: ClientOS; label: string }[] = [
  { value: "macos", label: "macOS" },
  { value: "linux", label: "Linux" },
  { value: "windows", label: "Windows" },
];

function Step({ n, label, children }: { n: number; label: string; children: ReactNode }) {
  return (
    <div className="grid gap-1.5">
      <div className="flex items-center gap-1.5 text-xs font-medium text-ink-3">
        <span className="grid size-4 place-items-center rounded-full bg-surface-2 text-[10px] tabular-nums text-ink-2">{n}</span>
        {label}
      </div>
      {children}
    </div>
  );
}

/**
 * Per-client setup for one key: snippets ready to copy, CC Switch import, a live connection test
 * and the usual errors. `keyPicker` puts a key choice in front of the client and OS steps.
 */
export function ConnectPanel({ keyId, secret, keyPicker }: { keyId: string; secret?: string; keyPicker?: ReactNode }) {
  const { t, pick } = useI18n();
  const [os, setOS] = useState<ClientOS>(detectOS);
  const [client, setClient] = useState<ClientId>("claude-code");
  const q = useQuery({
    queryKey: ["connect", keyId, os],
    queryFn: () => unwrap(api.GET("/keys/{id}/connect", { params: { path: { id: keyId }, query: { os } } })),
    placeholderData: (prev) => prev,
  });
  const logs = useLogsPath();
  const test = useMutation({
    mutationFn: (c: ClientId) =>
      unwrap(api.POST("/keys/{id}/test", { params: { path: { id: keyId } }, body: { client: c } })).then((r) => r.results[0] ?? { ok: false }),
  });

  const info: ConnectInfo | undefined = q.data;
  const current = info?.clients.find((c) => c.client === client) ?? info?.clients[0];
  const marker = t("connect.keyMarker");
  const fill = (s: string) => (info ? fillKey(s, info.placeholder, secret, marker) : s);
  const step = keyPicker ? 1 : 0;

  return (
    <div className="grid gap-5 lg:grid-cols-[232px_minmax(0,1fr)]">
      <div className="grid content-start gap-4">
        {keyPicker && (
          <Step n={1} label={t("connect.stepKey")}>
            {keyPicker}
          </Step>
        )}
        <Step n={step + 1} label={t("connect.client")}>
          {info ? (
            <div role="tablist" aria-label={t("connect.client")} className="flex flex-wrap gap-1 lg:grid lg:gap-0.5">
              {info.clients.map((c) => {
                const on = c.client === current?.client;
                return (
                  <button
                    key={c.client}
                    type="button"
                    role="tab"
                    aria-selected={on}
                    onClick={() => {
                      setClient(c.client);
                      test.reset();
                    }}
                    className={cn(
                      "flex h-8 items-center justify-between gap-2 rounded-md border px-2.5 text-left text-sm transition-colors lg:border-transparent",
                      on ? "border-accent/40 bg-accent-soft font-medium text-accent-strong" : "border-line text-ink-2 hover:bg-surface-2 hover:text-ink",
                    )}
                  >
                    {c.name}
                    {on && <ChevronRight className="hidden size-3.5 lg:block" />}
                  </button>
                );
              })}
            </div>
          ) : (
            <div className="grid gap-1">
              {Array.from({ length: 5 }, (_, i) => (
                <Skeleton key={i} className="h-7" />
              ))}
            </div>
          )}
        </Step>
        <Step n={step + 2} label={t("connect.os")}>
          <Segmented size="sm" aria-label={t("connect.os")} value={os} onChange={setOS} options={OS_OPTIONS} />
        </Step>
      </div>

      <div className="grid min-w-0 content-start gap-3">
        {q.isError ? (
          <ErrorState error={q.error} onRetry={() => void q.refetch()} />
        ) : !info || !current ? (
          <div className="grid gap-2" aria-busy="true">
            <Skeleton className="h-28" />
            <Skeleton className="h-16" />
          </div>
        ) : (
          <>
            {!secret && (
              <p className="flex items-start gap-2 rounded-md bg-surface-2 px-3 py-2 text-xs text-ink-2">
                <Info className="mt-px size-3.5 shrink-0 text-ink-3" />
                {t("connect.noSecret", { marker })}
              </p>
            )}
            {current.snippets.map((s, i) => (
              <CodeBlock key={i} title={s.path ?? pick(s.title)} code={fill(s.content)} />
            ))}
            {current.notes?.length ? (
              <ul className="grid gap-1 text-xs text-ink-2">
                {current.notes.map((n) => (
                  <li key={n.en} className="flex gap-2">
                    <span aria-hidden className="text-ink-3">
                      •
                    </span>
                    <span className="min-w-0 break-words">{fill(pick(n))}</span>
                  </li>
                ))}
              </ul>
            ) : null}
            <div className="flex flex-wrap items-center gap-2">
              <Button size="sm" onClick={() => test.mutate(current.client)} disabled={test.isPending}>
                <PlugZap />
                {test.isPending ? t("connect.testing") : t("connect.test")}
              </Button>
              {current.ccswitch_url &&
                (ccSwitchLink(current.ccswitch_url, info.placeholder, secret) ? (
                  <Button asChild size="sm" variant="ghost">
                    <a href={ccSwitchLink(current.ccswitch_url, info.placeholder, secret)}>
                      <Download />
                      {t("connect.ccswitch")}
                    </a>
                  </Button>
                ) : (
                  <Tooltip content={t("connect.ccswitchNoSecret")}>
                    <span tabIndex={0} className="inline-flex">
                      <Button size="sm" variant="ghost" disabled>
                        <Download />
                        {t("connect.ccswitch")}
                      </Button>
                    </span>
                  </Tooltip>
                ))}
              {test.data && (
                <span role="status" className="inline-flex flex-wrap items-center gap-x-2 text-xs">
                  <StatusText tone={test.data.ok ? "ok" : "bad"}>
                    {test.data.ok
                      ? t("connect.ok", { ttfb: formatDuration(test.data.ttfb_ms ?? 0) })
                      : [test.data.status, test.data.error].filter(Boolean).join(" ") || t("connect.failed")}
                  </StatusText>
                  {test.data.request_id && (
                    <Link to={logs} search={{ id: test.data.request_id }} className="text-accent hover:underline">
                      {t("connect.viewRequest")}
                    </Link>
                  )}
                </span>
              )}
              {test.isError && <span className="text-xs text-bad">{errorMessage(test.error)}</span>}
            </div>
            {info.common_errors.length > 0 && (
              <details className="group rounded-md border border-line">
                <summary className="cursor-pointer select-none px-3 py-2 text-xs font-medium text-ink-2 hover:text-ink">
                  {t("connect.commonErrors")}
                </summary>
                <dl className="grid border-t border-line text-xs sm:grid-cols-[minmax(0,2fr)_minmax(0,3fr)]">
                  {info.common_errors.map((e) => (
                    <div key={e.symptom} className="contents">
                      <dt className="border-b border-line-soft px-3 py-2 font-mono text-[11.5px] text-ink">{e.symptom}</dt>
                      <dd className="border-b border-line-soft px-3 py-2 text-ink-2">{pick(e.fix)}</dd>
                    </div>
                  ))}
                </dl>
              </details>
            )}
          </>
        )}
      </div>
    </div>
  );
}
