import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import type { ColumnDef } from "@tanstack/react-table";
import { Bell, Link2Off, Plus, Send, SlidersHorizontal, Trash2 } from "lucide-react";
import { useMemo, useState, type ReactNode } from "react";
import { toast } from "sonner";
import { usePageOn } from "@/app/session";
import { useConfirm } from "@/components/confirm";
import { DataTable } from "@/components/data-table";
import { EmptyState, ErrorState, LoadingRows, PageHeader, Panel } from "@/components/page";
import { Pill, StatusText, type Tone } from "@/components/status";
import { Button } from "@/components/ui/button";
import { Tooltip } from "@/components/ui/tooltip";
import { FeatureOff } from "@/features/features/feature-off";
import { useI18n } from "@/i18n";
import { api, unwrap } from "@/lib/api/client";
import { errorMessage } from "@/lib/api/errors";
import type { components } from "@/lib/api/schema.gen";
import { formatDateTime, formatRelative } from "@/lib/format";
import { ChannelDrawer } from "./channel-drawer";
import { TYPE_ICON, useEventLabel, type Channel } from "./shared";

type S = components["schemas"];
type Entry = S["Notification"];
type NotifyObject = S["NotifyObject"];

const channelsQuery = { queryKey: ["notify-channels"], queryFn: () => unwrap(api.GET("/notifications/channels")) };
const logQuery = { queryKey: ["notifications"], queryFn: () => unwrap(api.GET("/notifications", { params: { query: { limit: 100 } } })) };

/** What happened, by event: the tone of its pill. */
const EVENT_TONE: Record<string, Tone> = {
  credential_quarantined: "violet",
  credential_cooling: "warn",
  credential_quota_low: "warn",
  credential_secret_unsaved: "bad",
  upstream_paused: "bad",
  upstream_recovered: "ok",
  budget_reached: "warn",
  extension_crashed: "bad",
  test: "info",
};

/** One object of an entry, named like the console names it. */
function useObjectName() {
  const { t } = useI18n();
  return (o: NotifyObject) => {
    const name = o.name || o.id;
    switch (o.type) {
      case "credential":
        return `${o.parent_name || o.parent_id || ""} / ${name}`;
      case "key":
      case "user":
      case "group":
      case "extension":
      case "channel":
        return t(`notify.obj.${o.type}`, { name });
    }
    return name;
  };
}

/** The Notifications page: channels with add / edit / test, and the log of recent alerts. */
export function NotificationsPage() {
  const { t } = useI18n();
  const on = usePageOn("notifications.channels");
  if (!on) return <FeatureOff title={t("nav.notifications")} feature="notifications" />;
  return <Notifications />;
}

type Tested = Record<string, { ok: boolean }>;

function Notifications() {
  const { t } = useI18n();
  const qc = useQueryClient();
  const confirm = useConfirm();
  const eventLabel = useEventLabel();
  const q = useQuery(channelsQuery);
  const [editing, setEditing] = useState<Channel | "new" | null>(null);
  const [tested, setTested] = useState<Tested>({});
  const refresh = () => {
    void qc.invalidateQueries({ queryKey: channelsQuery.queryKey });
    void qc.invalidateQueries({ queryKey: logQuery.queryKey });
  };
  const remove = useMutation({
    mutationFn: (id: string) => unwrap(api.DELETE("/notifications/channels/{id}", { params: { path: { id } } })),
    onSuccess: refresh,
    onError: (e) => toast.error(errorMessage(e)),
  });
  const test = useMutation({
    mutationFn: (c: Channel) =>
      unwrap(api.POST("/notifications/channels/{id}/test", { params: { path: { id: c.id } } })).then(
        (r) => r.results[0] ?? { ok: false, attempts: 0, latency_ms: 0 },
      ),
    onSuccess: (r, c) => {
      setTested((m) => ({ ...m, [c.id]: { ok: r.ok } }));
      if (r.ok) toast.success(t("notify.testOk", { name: c.name }));
      else toast.error(t("notify.testFailed", { name: c.name }), { description: r.error });
      void qc.invalidateQueries({ queryKey: logQuery.queryKey });
    },
    onError: (e) => toast.error(errorMessage(e)),
  });
  const add = (
    <Button variant="primary" onClick={() => setEditing("new")} disabled={!q.data}>
      <Plus aria-hidden />
      {t("notify.add")}
    </Button>
  );
  const items = q.data?.items ?? [];
  return (
    <>
      <PageHeader
        title={t("nav.notifications")}
        description={t("notify.pageDescription")}
        actions={
          <>
            <Button asChild>
              <Link to="/features" search={{ id: "notifications" }}>
                <SlidersHorizontal aria-hidden />
                {t("notify.conditions")}
              </Link>
            </Button>
            {items.length > 0 && add}
          </>
        }
      />
      {q.data && !q.data.public_url_set && (
        <div className="mb-4 flex flex-wrap items-center gap-x-3 gap-y-2 rounded-lg border border-line bg-surface-2 px-4 py-3">
          <Link2Off className="size-4 shrink-0 text-ink-3" aria-hidden />
          <div className="min-w-0 flex-1">
            <p className="text-sm font-medium">{t("notify.noLinkTitle")}</p>
            <p className="text-xs text-ink-3">{t("notify.noLinkHint")}</p>
          </div>
          <Button size="sm" asChild>
            <Link to="/settings">{t("notify.noLinkAction")}</Link>
          </Button>
        </div>
      )}
      <Panel title={t("notify.channels")} className="mb-5" bodyClassName="p-0">
        {q.isError ? (
          <div className="p-4">
            <ErrorState error={q.error} onRetry={() => void q.refetch()} />
          </div>
        ) : q.isPending ? (
          <div className="p-4">
            <LoadingRows rows={3} />
          </div>
        ) : items.length === 0 ? (
          <EmptyState icon={Bell} title={t("notify.empty")} hint={t("notify.emptyHint")} action={add} />
        ) : (
          <ul className="divide-y divide-line-soft">
            {items.map((c) => {
              const Icon = TYPE_ICON[c.type];
              const r = tested[c.id];
              const busy = test.isPending && test.variables?.id === c.id;
              return (
                <li key={c.id} className="flex flex-wrap items-center gap-x-4 gap-y-2 px-4 py-3">
                  <span className="grid size-9 shrink-0 place-items-center rounded-md bg-surface-2 text-ink-2">
                    <Icon className="size-4" aria-hidden />
                  </span>
                  <div className="min-w-0 flex-1">
                    <div className="flex flex-wrap items-center gap-2">
                      <button type="button" className="truncate text-left text-base font-semibold hover:underline" onClick={() => setEditing(c)}>
                        {c.name}
                      </button>
                      <Pill>{t(`notify.type.${c.type}`)}</Pill>
                      {c.records && <Pill tone="info">{t("notify.recordsOn")}</Pill>}
                    </div>
                    <div className="mt-1 flex flex-wrap items-center gap-x-3 gap-y-1 text-xs text-ink-3">
                      <StatusText tone={c.enabled ? "ok" : "neutral"}>{t(c.enabled ? "notify.on" : "notify.off")}</StatusText>
                      <Tooltip content={c.events.length > 0 ? c.events.map(eventLabel).join("、") : t("notify.field.eventsAll")}>
                        <span className="cursor-default">
                          {c.events.length === 0 ? t("notify.allEvents") : t("notify.someEvents", { n: c.events.length })}
                        </span>
                      </Tooltip>
                      <span>{t(`notify.lang.${c.language}`)}</span>
                      <span className="min-w-0 truncate">
                        {c.type === "email" ? (
                          c.to.join(", ")
                        ) : (
                          <span data-raw className="font-mono">
                            {c.url}
                          </span>
                        )}
                      </span>
                      {r && (
                        <StatusText tone={r.ok ? "ok" : "bad"}>{t(r.ok ? "notify.lastTestOk" : "notify.lastTestFailed")}</StatusText>
                      )}
                    </div>
                  </div>
                  <div className="flex items-center gap-1">
                    <Button size="sm" onClick={() => test.mutate(c)} disabled={busy}>
                      <Send aria-hidden />
                      {busy ? t("notify.testing") : t("notify.test")}
                    </Button>
                    <Button size="sm" onClick={() => setEditing(c)}>
                      {t("common.edit")}
                    </Button>
                    <Button
                      size="icon-sm"
                      variant="ghost"
                      aria-label={t("notify.deleteTitle", { name: c.name })}
                      onClick={async () => {
                        const ok = await confirm({
                          title: t("notify.deleteTitle", { name: c.name }),
                          body: t("notify.deleteBody"),
                          confirm: t("common.delete"),
                          danger: true,
                        });
                        if (ok) remove.mutate(c.id);
                      }}
                    >
                      <Trash2 aria-hidden />
                    </Button>
                  </div>
                </li>
              );
            })}
          </ul>
        )}
      </Panel>
      <NotificationLog />
      {editing && q.data && (
        <ChannelDrawer
          key={editing === "new" ? "new" : editing.id}
          channel={editing}
          events={q.data.events}
          recordFields={q.data.record_fields}
          onClose={() => setEditing(null)}
          onSaved={() => {
            refresh();
            setEditing(null);
          }}
        />
      )}
    </>
  );
}

function NotificationLog() {
  const { t, locale } = useI18n();
  const q = useQuery({ ...logQuery, refetchInterval: 30_000 });
  const eventLabel = useEventLabel();
  const objectName = useObjectName();
  const columns = useMemo<ColumnDef<Entry>[]>(
    () => [
      {
        id: "time",
        header: t("notify.col.time"),
        meta: { className: "w-[132px]" },
        cell: ({ row }) => (
          <span className="whitespace-nowrap text-xs text-ink-2 tabular-nums" title={formatDateTime(row.original.created_at, locale)}>
            {formatRelative(row.original.created_at, locale)}
          </span>
        ),
      },
      {
        id: "what",
        header: t("notify.col.what"),
        meta: { className: "w-[200px]" },
        cell: ({ row }) => (
          <Pill tone={EVENT_TONE[row.original.event] ?? "neutral"} dot>
            {eventLabel(row.original.event)}
          </Pill>
        ),
      },
      {
        id: "about",
        header: t("notify.col.about"),
        cell: ({ row }) => {
          const objs = row.original.objects;
          const shown = objs.slice(0, 3).map(objectName).join("、");
          return (
            <span className="block min-w-0 truncate text-sm" title={objs.map(objectName).join("\n")}>
              {shown}
              {objs.length > 3 && <span className="ml-1 text-xs text-ink-3">{t("notify.more", { n: objs.length - 3 })}</span>}
            </span>
          );
        },
      },
      {
        id: "sent",
        header: t("notify.col.sent"),
        meta: { className: "w-[280px]" },
        cell: ({ row }) => <Deliveries entry={row.original} />,
      },
    ],
    [t, locale, eventLabel, objectName],
  );
  return (
    <Panel title={t("notify.log")} aside={t("notify.logHint")} bodyClassName="p-0">
      {q.isError ? (
        <div className="p-4">
          <ErrorState error={q.error} onRetry={() => void q.refetch()} />
        </div>
      ) : (
        <DataTable
          columns={columns}
          data={q.data?.items}
          getRowId={(e) => e.id}
          loadingRows={5}
          maxHeight="none"
          className="rounded-none border-0"
          empty={<EmptyState icon={Bell} title={t("notify.logEmpty")} hint={t("notify.logEmptyHint")} />}
        />
      )}
    </Panel>
  );
}

function Deliveries({ entry }: { entry: Entry }) {
  const { t } = useI18n();
  if (entry.status === "pending") return <StatusText tone="info" pulse>{t("notify.pending")}</StatusText>;
  if (entry.deliveries.length === 0) return <span className="text-xs text-ink-3">{t("notify.noChannel")}</span>;
  return (
    <div className="flex flex-wrap gap-1">
      {entry.deliveries.map((d) => {
        const pill: ReactNode = (
          <Pill tone={d.ok ? "ok" : "bad"} dot className="max-w-[16rem]">
            <span className="truncate">{d.channel_name}</span>
          </Pill>
        );
        return (
          <Tooltip
            key={d.channel_id}
            content={
              <span className="grid gap-0.5">
                <span>
                  {t(`notify.type.${d.channel_type}`)} · {t("notify.attempts", { n: d.attempts })}
                </span>
                {d.error && (
                  <span data-raw className="font-mono text-2xs">
                    {d.error}
                  </span>
                )}
              </span>
            }
          >
            <span>{pill}</span>
          </Tooltip>
        );
      })}
    </div>
  );
}
