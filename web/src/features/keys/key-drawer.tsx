import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { KeyRound, Plug, PlugZap, Power, RotateCw, Trash2 } from "lucide-react";
import { useState } from "react";
import { toast } from "sonner";
import { useClientLabel } from "@/components/client-name";
import { DetailList, Drawer } from "@/components/drawer";
import { useMoney } from "@/components/money";
import { LayerSettingsTab } from "@/components/layer-settings-tab";
import { EmptyState, LoadingRows } from "@/components/page";
import { StatusText } from "@/components/status";
import { Button } from "@/components/ui/button";
import { Select } from "@/components/ui/select";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { isOperator } from "@/app/nav";
import { useMe } from "@/app/session";
import { api, unwrap } from "@/lib/api/client";
import { errorMessage } from "@/lib/api/errors";
import type { ClientId, Key, KeyCreated } from "@/lib/api/types";
import { formatDateTime, formatDuration, formatRelative } from "@/lib/format";
import { useForm } from "@/lib/use-form";
import { useDiscardCheck } from "@/lib/use-unsaved-guard";
import { useI18n } from "@/i18n";
import { KeyFormFields, toFormValues, toSettings } from "./key-form";
import { KeyStatusPill, SpendCell, maskedKey, useKeyActions, useLimitItems } from "./key-parts";
import { useLogsPath } from "@/app/session";

type Tab = "overview" | "limits" | "more";

/** The clients a key test can speak as (each picks the wire protocol). */
const TEST_CLIENTS: readonly ClientId[] = ["claude-code", "codex", "gemini-cli", "cursor", "cline", "opencode", "curl"];

function TestBlock({ k }: { k: Key }) {
  const { t } = useI18n();
  const clientLabel = useClientLabel();
  const logs = useLogsPath();
  const [client, setClient] = useState<ClientId>(TEST_CLIENTS.find((c) => k.settings.clients?.includes(c)) ?? "claude-code");
  const test = useMutation({
    mutationFn: () => unwrap(api.POST("/keys/{id}/test", { params: { path: { id: k.id } }, body: { client } })).then((r) => r.results[0] ?? { ok: false }),
  });
  return (
    <div className="grid gap-2.5 rounded-lg border border-line bg-surface-2/40 p-3.5">
      <div>
        <h3 className="text-sm font-semibold">{t("keys.testTitle")}</h3>
        <p className="text-xs text-ink-3">{t("keys.testHint")}</p>
      </div>
      <div className="flex flex-wrap items-center gap-2">
        <Select
          aria-label={t("connect.client")}
          value={client}
          onChange={(c) => {
            setClient(c);
            test.reset();
          }}
          options={TEST_CLIENTS.map((id) => ({ value: id, label: clientLabel({ id }) }))}
          className="w-40"
        />
        <Button onClick={() => test.mutate()} disabled={test.isPending || k.status === "disabled"}>
          <PlugZap />
          {test.isPending ? t("connect.testing") : t("connect.test")}
        </Button>
      </div>
      {test.data && (
        <div role="status" className="flex flex-wrap items-center gap-x-2 text-xs">
          <StatusText tone={test.data.ok ? "ok" : "bad"}>
            {test.data.ok
              ? t("connect.okModel", { ttfb: formatDuration(test.data.ttfb_ms ?? 0), model: test.data.model ?? "—" })
              : [test.data.status, test.data.error].filter(Boolean).join(" ") || t("connect.failed")}
          </StatusText>
          {test.data.request_id && (
            <Link to={logs} search={{ id: test.data.request_id }} className="text-accent hover:underline">
              {t("connect.viewRequest")}
            </Link>
          )}
        </div>
      )}
      {test.isError && <p className="text-xs text-bad">{errorMessage(test.error)}</p>}
    </div>
  );
}

function Overview({ k, admin }: { k: Key; admin: boolean }) {
  const { t, locale } = useI18n();
  const money = useMoney();
  const limits = useLimitItems()(k);
  return (
    <div className="grid gap-5 p-5">
      <div className="grid grid-cols-2 gap-3">
        <div className="rounded-lg border border-line px-3.5 py-3">
          <div className="text-xs text-ink-3">{t("keys.monthSpend")}</div>
          <div className="mt-1 text-lg font-semibold tabular-nums">{money(k.month_spend)}</div>
          {k.settings.budget_monthly && (
            <div className="mt-1">
              <SpendCell k={k} wide />
            </div>
          )}
        </div>
        <div className="rounded-lg border border-line px-3.5 py-3">
          <div className="text-xs text-ink-3">{t("keys.lastUsed")}</div>
          <div className="mt-1 text-lg font-semibold tabular-nums">{k.last_used_at ? formatRelative(k.last_used_at, locale) : t("keys.never")}</div>
        </div>
      </div>
      <DetailList
        items={[
          { label: t("keys.key"), value: <span className="font-mono text-xs">{maskedKey(k)}</span> },
          ...(admin ? [{ label: t("keys.owner"), value: k.owner?.name ?? "—" }] : []),
          { label: t("keys.group"), value: k.group?.name ?? t("keys.noGroup") },
          { label: t("keys.created"), value: formatDateTime(k.created_at, locale) },
        ]}
      />
      <div>
        <h3 className="mb-2 text-sm font-semibold">{t("keys.ownLimits")}</h3>
        {limits.length === 0 ? (
          <p className="text-sm text-ink-2">{t("keys.followsGroupLong")}</p>
        ) : (
          <ul className="grid gap-1.5 text-sm">
            {limits.map((l) => (
              <li key={l.id} className="flex gap-2">
                <span aria-hidden className={l.tone === "warn" ? "text-warn" : "text-ink-3"}>
                  •
                </span>
                <span className="min-w-0 break-words">{l.long}</span>
              </li>
            ))}
          </ul>
        )}
      </div>
      <TestBlock k={k} />
    </div>
  );
}

/**
 * The key drawer: what the key is and does (overview + test), its limits form, and, for admins,
 * the settings that turned-on features add to a key. `id = "new"` creates one.
 */
export function KeyDrawer({
  id,
  keys,
  loading,
  onClose,
  onSecret,
  onConnect,
}: {
  id: string | undefined;
  keys: Key[] | undefined;
  loading: boolean;
  onClose: () => void;
  onSecret: (c: KeyCreated) => void;
  onConnect: (k: Key) => void;
}) {
  const { t } = useI18n();
  const me = useMe();
  const admin = isOperator(me);
  const qc = useQueryClient();
  const discard = useDiscardCheck();
  const isNew = id === "new";
  const k = isNew ? undefined : keys?.find((x) => x.id === id);
  const [tab, setTab] = useState<Tab>("overview");
  const [moreDirty, setMoreDirty] = useState(false);
  const form = useForm(toFormValues("", {}));
  const [seen, setSeen] = useState<string | undefined>(undefined);
  const stamp = isNew ? "new" : k ? `${k.id}:${JSON.stringify(k.settings)}:${k.name}` : undefined;
  if (stamp !== seen) {
    const sameKey = seen?.split(":")[0] === stamp?.split(":")[0];
    setSeen(stamp);
    if (!sameKey) setTab(isNew ? "limits" : "overview");
    if (!sameKey || !form.dirty) form.reset(toFormValues(k?.name ?? "", k?.settings ?? {}));
  }
  const groups = useQuery({ queryKey: ["groups"], queryFn: () => unwrap(api.GET("/groups")), enabled: admin && id !== undefined });
  const ownerGroup = isNew
    ? groups.data?.items.find((g) => g.id === (form.values.owner ? undefined : me.user.group_id))
    : groups.data?.items.find((g) => g.id === k?.group?.id);

  const actions = useKeyActions({ onSecret, onDeleted: () => onClose() });

  const save = useMutation({
    mutationFn: async () => {
      const { settings, errors } = toSettings(form.values, me.site.currency);
      if (Object.keys(errors).length) {
        form.setErrors(errors);
        throw new Error(t("form.fixErrors"));
      }
      const name = form.values.name.trim();
      if (isNew) return unwrap(api.POST("/keys", { body: { name, settings, ...(form.values.owner ? { user_id: form.values.owner } : {}) } }));
      await unwrap(api.PATCH("/keys/{id}", { params: { path: { id: k!.id } }, body: { name, settings } }));
      return undefined;
    },
    onSuccess: (created) => {
      void qc.invalidateQueries({ queryKey: ["keys"] });
      form.reset(form.values);
      if (created) {
        onSecret(created);
        onClose();
      } else toast.success(t("keys.saved"));
    },
    onError: (e) => {
      form.absorb(e);
      toast.error(errorMessage(e));
    },
  });

  const requestClose = async () => {
    if (await discard(form.dirty || moreDirty)) {
      setMoreDirty(false);
      onClose();
    }
  };

  const open = id !== undefined;
  const missing = open && !isNew && !k && !loading;

  const formFooter = (
    <>
      {form.dirty && <span className="mr-auto text-xs text-warn">{t("unsaved.badge")}</span>}
      <Button onClick={() => void requestClose()}>{t("common.cancel")}</Button>
      <Button variant="primary" onClick={() => save.mutate()} disabled={save.isPending || (!isNew && !form.dirty)}>
        {save.isPending ? t("common.saving") : isNew ? t("keys.createSubmit") : t("common.save")}
      </Button>
    </>
  );
  const overviewFooter = k && (
    <>
      <Button variant="danger" onClick={() => void actions.remove(k)} disabled={actions.pending} className="mr-auto">
        <Trash2 />
        {t("common.delete")}
      </Button>
      <Button onClick={() => void actions.toggle(k)} disabled={actions.pending || k.status === "expired"}>
        <Power />
        {k.status === "disabled" ? t("keys.enable") : t("keys.disable")}
      </Button>
      <Button onClick={() => void actions.rotate(k)} disabled={actions.pending}>
        <RotateCw />
        {t("keys.rotate")}
      </Button>
      <Button variant="primary" onClick={() => onConnect(k)}>
        <Plug />
        {t("keys.connect")}
      </Button>
    </>
  );

  return (
    <Tabs value={tab} onValueChange={(v) => setTab(v as Tab)}>
      <Drawer
        open={open}
        onOpenChange={(o) => !o && void requestClose()}
        onRequestClose={() => void requestClose()}
        size="lg"
        title={isNew ? t("keys.create") : (k?.name ?? (missing ? t("keys.notFound") : t("common.loading")))}
        subtitle={isNew ? t("keys.createHint") : k ? [maskedKey(k), admin ? k.owner?.name : undefined].filter(Boolean).join(" · ") : undefined}
        meta={k && <KeyStatusPill k={k} />}
        tabs={
          k && (
            <TabsList>
              <TabsTrigger value="overview">{t("keys.tabOverview")}</TabsTrigger>
              <TabsTrigger value="limits">{t("keys.tabLimits")}</TabsTrigger>
              {admin && <TabsTrigger value="more">{t("keys.tabMore")}</TabsTrigger>}
            </TabsList>
          )
        }
        footer={isNew || tab === "limits" ? formFooter : tab === "overview" ? overviewFooter : undefined}
      >
        {missing ? (
          <EmptyState icon={KeyRound} title={t("keys.notFound")} hint={t("keys.notFoundHint")} />
        ) : isNew ? (
          <form
            className="p-5"
            onSubmit={(e) => {
              e.preventDefault();
              save.mutate();
            }}
          >
            <KeyFormFields
              values={form.values}
              set={form.set}
              errors={form.errors}
              currency={me.site.currency}
              admin={admin}
              isNew
              groupModels={ownerGroup?.models}
            />
          </form>
        ) : k ? (
          <>
            <TabsContent value="overview">
              <Overview k={k} admin={admin} />
            </TabsContent>
            <TabsContent value="limits">
              <form
                className="p-5"
                onSubmit={(e) => {
                  e.preventDefault();
                  save.mutate();
                }}
              >
                <KeyFormFields
                  values={form.values}
                  set={form.set}
                  errors={form.errors}
                  currency={me.site.currency}
                  admin={admin}
                  isNew={false}
                  groupModels={ownerGroup?.models}
                />
              </form>
            </TabsContent>
            <TabsContent value="more">
              {admin && <LayerSettingsTab layer="key" id={k.id} hint={t("keys.moreHint")} saved={t("keys.saved")} onDirty={setMoreDirty} />}
            </TabsContent>
          </>
        ) : (
          <div className="p-5">
            <LoadingRows rows={6} />
          </div>
        )}
      </Drawer>
    </Tabs>
  );
}
