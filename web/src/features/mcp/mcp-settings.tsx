import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Plug, Plus, Trash2, Zap } from "lucide-react";
import { useState } from "react";
import { toast } from "sonner";
import { useConfirm } from "@/components/confirm";
import { CopyButton } from "@/components/copy-button";
import { Drawer } from "@/components/drawer";
import { SwitchRow } from "@/components/form-kit";
import { EmptyState, ErrorState, LoadingRows, PageHeader, Panel } from "@/components/page";
import { StatusText } from "@/components/status";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Field } from "@/components/ui/field";
import { Input, Textarea } from "@/components/ui/input";
import { Select } from "@/components/ui/select";
import { useMe } from "@/app/session";
import { useI18n } from "@/i18n";
import { api, unwrap } from "@/lib/api/client";
import { errorMessage } from "@/lib/api/errors";
import type { components } from "@/lib/api/schema.gen";
import { useForm } from "@/lib/use-form";
import { useDiscardCheck } from "@/lib/use-unsaved-guard";
import { parseHeaders, splitList } from "@/lib/text";

type S = components["schemas"];
export type McpServer = S["McpServer"];
type Transport = NonNullable<S["McpServerInput"]["transport"]>;
const TRANSPORTS: Transport[] = ["auto", "streamable-http", "sse"];

const mcpServersQuery = { queryKey: ["mcp-servers"], queryFn: () => unwrap(api.GET("/mcp/servers")) };

type TestState = { ok: boolean; tools: number; ms: number };

/** The MCP servers page: one row per upstream MCP server, test inline, editor in a drawer. */
export function McpSettings() {
  const { t } = useI18n();
  const qc = useQueryClient();
  const confirm = useConfirm();
  const q = useQuery(mcpServersQuery);
  const [editing, setEditing] = useState<McpServer | "new" | null>(null);
  const [tested, setTested] = useState<Record<string, TestState>>({});
  const refresh = () => void qc.invalidateQueries({ queryKey: mcpServersQuery.queryKey });
  const remove = useMutation({
    mutationFn: (id: string) => unwrap(api.DELETE("/mcp/servers/{id}", { params: { path: { id } } })),
    onSuccess: refresh,
    onError: (e) => toast.error(errorMessage(e)),
  });
  const test = useMutation({
    mutationFn: (s: McpServer) =>
      unwrap(api.POST("/mcp/servers/{id}/test", { params: { path: { id: s.id } } })).then(
        (r) => r.results[0] ?? { ok: false, tools: [], latency_ms: 0 },
      ),
    onSuccess: (r, s) => {
      const name = s.name || s.id;
      setTested((m) => ({
        ...m,
        [s.id]: { ok: r.ok, tools: r.tools.length, ms: r.latency_ms },
      }));
      if (r.ok) toast.success(t("mcp.testOk", { name, n: r.tools.length }));
      else toast.error(t("mcp.testFailedTitle", { name }), { description: r.error });
    },
    onError: (e) => toast.error(errorMessage(e)),
  });
  const endpoint = `${(useMe().site.public_base_url || window.location.origin).replace(/\/+$/, "")}/mcp`;
  const add = (
    <Button variant="primary" onClick={() => setEditing("new")} disabled={!q.data}>
      <Plus aria-hidden />
      {t("mcp.add")}
    </Button>
  );

  return (
    <>
      <PageHeader
        title={t("nav.mcp")}
        description={t("mcp.pageDescription")}
        actions={q.data && q.data.items.length > 0 ? add : undefined}
      />
      <Panel className="mb-4" bodyClassName="flex flex-wrap items-center gap-x-4 gap-y-2 p-4">
        <div className="min-w-0 flex-1">
          <p className="text-sm font-medium">{t("mcp.endpointTitle")}</p>
          <p className="mt-0.5 text-xs text-ink-3">{t("mcp.endpointHint")}</p>
        </div>
        <span className="flex min-w-0 items-center gap-1">
          <code className="truncate rounded-md bg-surface-2 px-2 py-1 font-mono text-xs">{endpoint}</code>
          <CopyButton text={endpoint} size="icon-sm" variant="ghost" />
        </span>
      </Panel>
      {q.isError ? (
        <ErrorState error={q.error} onRetry={() => void q.refetch()} />
      ) : q.isPending ? (
        <LoadingRows rows={3} />
      ) : q.data.items.length === 0 ? (
        <div className="rounded-lg border border-dashed border-line-strong bg-surface">
          <EmptyState icon={Plug} title={t("mcp.empty")} hint={t("mcp.emptyHint")} action={add} />
        </div>
      ) : (
        <ul className="divide-y divide-line-soft rounded-lg border border-line bg-surface shadow-card">
          {q.data.items.map((s) => {
            const r = tested[s.id];
            return (
              <li key={s.id} className="flex flex-wrap items-center gap-x-4 gap-y-2 px-4 py-3.5">
                <div className="min-w-0 flex-1">
                  <div className="flex flex-wrap items-center gap-2">
                    <span className="text-base font-semibold">{s.name || s.id}</span>
                    <Badge>{t(`mcp.transport.${s.transport}`)}</Badge>
                  </div>
                  <code className="mt-0.5 block truncate font-mono text-xs text-ink-3">{s.url}</code>
                  <div className="mt-1.5 flex flex-wrap items-center gap-x-3 gap-y-1 text-xs">
                    <StatusText tone={s.enabled ? "ok" : "neutral"}>
                      {t(s.enabled ? "mcp.enabled" : "mcp.disabled")}
                    </StatusText>
                    {r && (
                      <StatusText tone={r.ok ? "ok" : "bad"}>
                        {r.ok ? t("mcp.testSummary", { n: r.tools, ms: r.ms }) : t("mcp.testFailedShort")}
                      </StatusText>
                    )}
                  </div>
                </div>
                <div className="flex items-center gap-1">
                  <Button
                    size="sm"
                    onClick={() => test.mutate(s)}
                    disabled={test.isPending && test.variables?.id === s.id}
                  >
                    <Zap aria-hidden />
                    {test.isPending && test.variables?.id === s.id ? t("mcp.testing") : t("mcp.test")}
                  </Button>
                  <Button size="sm" onClick={() => setEditing(s)}>
                    {t("common.edit")}
                  </Button>
                  <Button
                    size="icon-sm"
                    variant="ghost"
                    aria-label={t("mcp.deleteTitle", { name: s.name || s.id })}
                    onClick={async () => {
                      const ok = await confirm({
                        title: t("mcp.deleteTitle", { name: s.name || s.id }),
                        body: t("mcp.deleteBody"),
                        confirm: t("common.delete"),
                        danger: true,
                      });
                      if (ok) remove.mutate(s.id);
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
      {editing && (
        <ServerDrawer
          key={editing === "new" ? "new" : editing.id}
          server={editing}
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

function ServerDrawer({
  server,
  onClose,
  onSaved,
}: {
  server: McpServer | "new";
  onClose: () => void;
  onSaved: () => void;
}) {
  const { t } = useI18n();
  const discard = useDiscardCheck();
  const isNew = server === "new";
  const s = isNew ? undefined : server;
  const form = useForm({
    id: s?.id ?? "",
    name: s?.name ?? "",
    url: s?.url ?? "",
    transport: (s?.transport ?? "auto") as Transport,
    auth: "",
    headers: Object.entries(s?.headers ?? {})
      .map(([k, v]) => `${k}: ${v}`)
      .join("\n"),
    tools_deny: (s?.tools_deny ?? []).join("\n"),
    allow_private: s?.allow_private ?? false,
    enabled: s?.enabled ?? true,
  });
  const v = form.values;
  const hasAuth = (s?.secret_headers ?? []).includes("Authorization");
  const save = useMutation({
    mutationFn: () => {
      const body: S["McpServerInput"] = {
        name: v.name.trim(),
        url: v.url.trim(),
        transport: v.transport,
        headers: parseHeaders(v.headers),
        tools_deny: splitList(v.tools_deny),
        allow_private: v.allow_private,
        enabled: v.enabled,
        ...(v.auth ? { secret_headers: { Authorization: v.auth } } : {}),
      };
      if (isNew) return unwrap(api.POST("/mcp/servers", { body: { ...body, id: v.id.trim() } }));
      return unwrap(api.PATCH("/mcp/servers/{id}", { params: { path: { id: s!.id } }, body }));
    },
    onSuccess: (r) => {
      toast.success(t("mcp.saved", { name: r.name || r.id }));
      onSaved();
    },
    onError: (e) => {
      form.absorb(e);
      toast.error(errorMessage(e));
    },
  });
  const close = async () => {
    if (await discard(form.dirty)) onClose();
  };
  return (
    <Drawer
      open
      onOpenChange={(o) => !o && void close()}
      title={isNew ? t("mcp.add") : s!.name || s!.id}
      size="md"
      onRequestClose={() => void close()}
      footer={
        <>
          <Button onClick={() => void close()}>{t("common.cancel")}</Button>
          <Button variant="primary" onClick={() => save.mutate()} disabled={save.isPending}>
            {save.isPending ? t("common.saving") : t("common.save")}
          </Button>
        </>
      }
    >
      <div className="grid gap-4 p-5">
        <div className="grid gap-4 sm:grid-cols-2">
          <Field label={t("mcp.field.id")} hint={t("mcp.field.idHint")} error={form.errors.id}>
            <Input
              value={v.id}
              disabled={!isNew}
              onChange={(e) => form.set("id", e.target.value)}
              autoComplete="off"
              className="font-mono"
            />
          </Field>
          <Field label={t("mcp.field.name")} error={form.errors.name}>
            <Input value={v.name} onChange={(e) => form.set("name", e.target.value)} />
          </Field>
        </div>
        <Field label={t("mcp.field.url")} hint={t("mcp.field.urlHint")} error={form.errors.url}>
          <Input
            value={v.url}
            onChange={(e) => form.set("url", e.target.value)}
            placeholder="https://mcp.example.com/mcp"
            className="font-mono"
          />
        </Field>
        <Field label={t("mcp.field.transport")} error={form.errors.transport}>
          <Select
            value={v.transport}
            onChange={(x) => form.set("transport", x)}
            options={TRANSPORTS.map((x) => ({ value: x, label: t(`mcp.transport.${x}`) }))}
          />
        </Field>
        <Field
          label={t("mcp.field.auth")}
          hint={t("mcp.field.authHint")}
          error={form.errors["secret_headers.Authorization"]}
        >
          <Input
            type="password"
            autoComplete="off"
            value={v.auth}
            placeholder={hasAuth ? t("form.secretKeep") : "Bearer …"}
            onChange={(e) => form.set("auth", e.target.value)}
          />
        </Field>
        <Field label={t("mcp.field.headers")} hint={t("mcp.field.headersHint")} error={form.errors.headers}>
          <Textarea
            value={v.headers}
            onChange={(e) => form.set("headers", e.target.value)}
            className="min-h-14 font-mono"
          />
        </Field>
        <Field
          label={t("mcp.field.toolsDeny")}
          hint={t("mcp.field.toolsDenyHint")}
          error={form.errors.tools_deny}
        >
          <Textarea
            value={v.tools_deny}
            onChange={(e) => form.set("tools_deny", e.target.value)}
            className="min-h-14 font-mono"
          />
        </Field>
        <SwitchRow
          label={t("mcp.field.allowPrivate")}
          description={t("mcp.field.allowPrivateHint")}
          checked={v.allow_private}
          onChange={(c) => form.set("allow_private", c)}
        />
        <SwitchRow
          label={t("mcp.field.enabled")}
          checked={v.enabled}
          onChange={(c) => form.set("enabled", c)}
        />
      </div>
    </Drawer>
  );
}
