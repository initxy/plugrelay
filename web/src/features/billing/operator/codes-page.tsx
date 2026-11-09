import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import type { ColumnDef } from "@tanstack/react-table";
import { AlertTriangle, Download, FileDown, Plus, Ticket } from "lucide-react";
import { useMemo, useState } from "react";
import { toast } from "sonner";
import { useConfirm } from "@/components/confirm";
import { CopyButton } from "@/components/copy-button";
import { DataTable } from "@/components/data-table";
import { Drawer } from "@/components/drawer";
import { HelpText, MoneyInput } from "@/components/form-kit";
import { EmptyState, ErrorState, PageHeader } from "@/components/page";
import { StatTile } from "@/components/stat-tile";
import { Pill, type Tone } from "@/components/status";
import { Button } from "@/components/ui/button";
import { Field } from "@/components/ui/field";
import { Input, Textarea } from "@/components/ui/input";
import { useMoney } from "@/components/money";
import { errorMessage } from "@/lib/api/errors";
import { formatDateTime, formatNumber } from "@/lib/format";
import { useDrawerParam } from "@/lib/use-url-search";
import { useI18n } from "@/i18n";
import { redeemApi, type RedeemBatch, type RedeemCode } from "../api";
import { useNames } from "../shared";
import { csvRow, download, fromLocalInput } from "./parts";
import { useForm } from "@/lib/use-form";

const batchesQuery = { queryKey: ["billing", "batches"], queryFn: () => redeemApi.batches() };

type BatchState = "active" | "disabled" | "expired" | "used_up" | "partly_off";
const BATCH_TONE: Record<BatchState, Tone> = { active: "ok", disabled: "neutral", expired: "neutral", used_up: "info", partly_off: "warn" };

function batchState(b: RedeemBatch): BatchState {
  if (b.disabled === b.codes) return "disabled";
  if (b.expired === b.codes) return "expired";
  if (b.uses >= b.max_uses) return "used_up";
  if (b.disabled > 0) return "partly_off";
  return "active";
}

type CodeState = "active" | "disabled" | "expired" | "used_up";
const CODE_TONE: Record<CodeState, Tone> = { active: "ok", disabled: "neutral", expired: "neutral", used_up: "info" };

function codeState(c: RedeemCode, now: number): CodeState {
  if (c.disabled) return "disabled";
  if (c.expires_at && new Date(c.expires_at).getTime() <= now) return "expired";
  if (c.uses >= c.max_uses) return "used_up";
  return "active";
}

/** Redeem codes by batch: generate (shown once), see what was redeemed and by whom, disable, export. */
export function RedeemCodesPage() {
  const { t, locale } = useI18n();
  const money = useMoney();
  const drawer = useDrawerParam();
  const [generating, setGenerating] = useState(false);
  const q = useQuery(batchesQuery);
  const items = q.data?.items;
  const cur = q.data?.currency ?? "USD";
  const totalCodes = items?.reduce((s, b) => s + b.codes, 0) ?? 0;
  const redeemed = items?.reduce((s, b) => s + b.redeemed.micro, 0) ?? 0;
  const uses = items?.reduce((s, b) => s + b.uses, 0) ?? 0;
  const columns = useMemo<ColumnDef<RedeemBatch>[]>(
    () => [
      {
        id: "batch",
        header: t("billing.op.codes.batch"),
        cell: ({ row }) => (
          <span className="grid min-w-0">
            <span className="truncate font-medium" data-raw>
              {row.original.batch || t("billing.op.codes.noName")}
            </span>
            <span className="text-xs text-ink-3">{formatDateTime(row.original.created_at, locale)}</span>
          </span>
        ),
      },
      { id: "codes", header: t("billing.op.codes.codes"), meta: { numeric: true, className: "w-[90px]" }, cell: ({ row }) => formatNumber(row.original.codes, locale) },
      { id: "amount", header: t("billing.op.codes.amountEach"), meta: { numeric: true, className: "w-[120px]" }, cell: ({ row }) => money(row.original.amount) },
      {
        id: "used",
        header: t("billing.op.codes.used"),
        meta: { numeric: true, className: "w-[120px]" },
        cell: ({ row }) => `${formatNumber(row.original.uses, locale)} / ${formatNumber(row.original.max_uses, locale)}`,
      },
      { id: "redeemed", header: t("billing.op.codes.redeemed"), meta: { numeric: true, className: "w-[130px] max-md:hidden" }, cell: ({ row }) => money(row.original.redeemed) },
      {
        id: "expires",
        header: t("billing.op.codes.expires"),
        meta: { className: "w-[150px] max-lg:hidden" },
        cell: ({ row }) => <span className="text-xs text-ink-2">{row.original.expires_at ? formatDateTime(row.original.expires_at, locale) : t("billing.op.codes.never")}</span>,
      },
      {
        id: "state",
        header: t("billing.op.codes.state"),
        meta: { className: "w-[120px]" },
        cell: ({ row }) => {
          const s = batchState(row.original);
          return (
            <Pill tone={BATCH_TONE[s]} dot>
              {t(`billing.op.codes.batchState.${s}`)}
            </Pill>
          );
        },
      },
    ],
    [t, locale, money],
  );
  return (
    <>
      <PageHeader
        title={t("nav.billing.codes")}
        description={t("billing.op.codes.description")}
        actions={
          <Button variant="primary" onClick={() => setGenerating(true)}>
            <Plus aria-hidden />
            {t("billing.op.codes.generate")}
          </Button>
        }
      />
      <div className="mb-4 grid grid-cols-2 gap-3 lg:grid-cols-3">
        <StatTile label={t("billing.op.codes.statCodes")} loading={q.isPending} value={formatNumber(totalCodes, locale)} note={t("billing.op.codes.statBatches", { n: items?.length ?? 0 })} />
        <StatTile label={t("billing.op.codes.statUses")} loading={q.isPending} value={formatNumber(uses, locale)} note={t("billing.op.codes.statUsesNote")} />
        <StatTile label={t("billing.op.codes.redeemed")} loading={q.isPending} value={money({ micro: redeemed, currency: cur })} note={t("billing.op.codes.statRedeemedNote")} />
      </div>
      {q.isError ? (
        <ErrorState error={q.error} onRetry={() => void q.refetch()} />
      ) : (
        <DataTable
          columns={columns}
          data={items}
          getRowId={(b) => b.batch}
          onRowClick={(b) => drawer.open(b.batch)}
          selectedId={drawer.id}
          empty={
            <EmptyState
              icon={Ticket}
              title={t("billing.op.codes.empty")}
              hint={t("billing.op.codes.emptyHint")}
              action={
                <Button variant="primary" onClick={() => setGenerating(true)}>
                  <Plus aria-hidden />
                  {t("billing.op.codes.generate")}
                </Button>
              }
            />
          }
        />
      )}
      {generating && <GenerateDrawer currency={cur} onClose={() => setGenerating(false)} />}
      {drawer.id && <BatchDrawer batch={drawer.id} summary={items?.find((b) => b.batch === drawer.id)} onClose={drawer.close} />}
    </>
  );
}

function GenerateDrawer({ currency, onClose }: { currency: string; onClose: () => void }) {
  const { t } = useI18n();
  const qc = useQueryClient();
  const confirm = useConfirm();
  const form = useForm({ count: "10", amount: null as number | null, uses: "1", expires: "", name: "" });
  const { count, amount, uses, expires, name } = form.values;
  const [result, setResult] = useState<{ batch: string; codes: string[] }>();
  const n = Number(count);
  const u = Number(uses);
  const countBad = !Number.isInteger(n) || n < 1 || n > 1000;
  const usesBad = !Number.isInteger(u) || u < 1;
  const gen = useMutation({
    mutationFn: () =>
      redeemApi.generate({
        count: n,
        amount_micro: Math.round((amount ?? 0) * 1_000_000),
        max_uses: u,
        expires_at: fromLocalInput(expires),
        batch: name.trim() || undefined,
      }),
    onSuccess: (r) => {
      setResult({ batch: r.batch, codes: r.codes });
      void qc.invalidateQueries({ queryKey: batchesQuery.queryKey });
    },
    onError: (e) => toast.error(errorMessage(e)),
  });
  const close = async () => {
    if (result && !(await confirm({ title: t("billing.op.codes.closeTitle"), body: t("billing.op.codes.closeBody"), confirm: t("billing.op.codes.closeAnyway"), danger: true }))) return;
    onClose();
  };
  const text = result?.codes.join("\n") ?? "";
  return (
    <Drawer
      open
      onOpenChange={(o) => !o && void close()}
      onRequestClose={() => void close()}
      title={result ? t("billing.op.codes.resultTitle", { n: result.codes.length }) : t("billing.op.codes.generate")}
      subtitle={result ? undefined : t("billing.op.codes.generateHint")}
      footer={
        result ? (
          <>
            <Button onClick={() => download(text + "\n", `redeem-codes-${result.batch}.txt`)}>
              <Download aria-hidden />
              {t("billing.op.codes.downloadTxt")}
            </Button>
            <CopyButton text={text} label={t("billing.op.codes.copyAll")} size="md" />
            <Button variant="primary" onClick={() => void close()}>
              {t("billing.op.codes.done")}
            </Button>
          </>
        ) : (
          <>
            <Button onClick={onClose}>{t("common.cancel")}</Button>
            <Button variant="primary" disabled={countBad || usesBad || !amount || amount <= 0 || gen.isPending} onClick={() => gen.mutate()}>
              {gen.isPending ? t("common.saving") : t("billing.op.codes.generate")}
            </Button>
          </>
        )
      }
    >
      <div className="grid gap-4 p-5">
        {result ? (
          <>
            <div className="flex items-start gap-2 rounded-lg border border-warn/40 bg-warn-soft px-3 py-2.5 text-sm">
              <AlertTriangle className="mt-0.5 size-4 shrink-0 text-warn" aria-hidden />
              {t("billing.op.codes.onceWarning")}
            </div>
            <Textarea readOnly value={text} rows={Math.min(16, result.codes.length + 1)} className="font-mono text-xs" aria-label={t("billing.op.codes.codes")} />
          </>
        ) : (
          <>
            <div className="grid grid-cols-2 gap-4">
              <Field label={t("billing.op.codes.count")} hint={t("billing.op.codes.countHint")} error={count && countBad ? t("billing.op.codes.countBad") : undefined}>
                <Input inputMode="numeric" value={count} onChange={(e) => form.set("count", e.target.value)} />
              </Field>
              <Field label={t("billing.op.codes.amountEach")}>
                <MoneyInput value={amount} onChange={(v) => form.set("amount", v)} currency={currency} placeholder="10.00" />
              </Field>
              <Field label={t("billing.op.codes.usesPerCode")} hint={t("billing.op.codes.usesHint")} error={uses && usesBad ? t("billing.op.codes.usesBad") : undefined}>
                <Input inputMode="numeric" value={uses} onChange={(e) => form.set("uses", e.target.value)} />
              </Field>
              <Field label={t("billing.op.codes.expires")} optional>
                <Input type="datetime-local" value={expires} onChange={(e) => form.set("expires", e.target.value)} />
              </Field>
            </div>
            <Field label={t("billing.op.codes.batchName")} optional hint={t("billing.op.codes.batchNameHint")}>
              <Input value={name} onChange={(e) => form.set("name", e.target.value)} placeholder="spring-promo" maxLength={64} />
            </Field>
          </>
        )}
      </div>
    </Drawer>
  );
}

function BatchDrawer({ batch, summary, onClose }: { batch: string; summary?: RedeemBatch; onClose: () => void }) {
  const { t, locale } = useI18n();
  const money = useMoney();
  const names = useNames();
  const qc = useQueryClient();
  const confirm = useConfirm();
  const [now] = useState(() => Date.now());
  const q = useQuery({ queryKey: ["billing", "codes", batch], queryFn: () => redeemApi.codes(batch, true) });
  const refresh = () => {
    void qc.invalidateQueries({ queryKey: ["billing", "codes", batch] });
    void qc.invalidateQueries({ queryKey: batchesQuery.queryKey });
  };
  const toggle = useMutation({
    mutationFn: (c: RedeemCode) => redeemApi.disableCode(c.id, !c.disabled),
    onSuccess: refresh,
    onError: (e) => toast.error(errorMessage(e)),
  });
  const allOff = summary ? summary.disabled === summary.codes : false;
  const toggleBatch = useMutation({
    mutationFn: () => redeemApi.disableBatch(batch, !allOff),
    onSuccess: () => {
      toast.success(allOff ? t("billing.op.codes.batchOn") : t("billing.op.codes.batchOff"));
      refresh();
    },
    onError: (e) => toast.error(errorMessage(e)),
  });
  const exportCsv = () => {
    const rows = [csvRow((["code", "amountEach", "used", "usesPerCode", "state", "expires", "who", "whenRedeemed"] as const).map((k) => t(`billing.op.codes.${k}`)))];
    for (const c of q.data?.items ?? []) {
      const base = [`…${c.last4}`, money(c.amount), c.uses, c.max_uses, t(`billing.op.codes.codeState.${codeState(c, now)}`), c.expires_at ?? ""];
      if (!c.redeemed?.length) rows.push(csvRow([...base, "", ""]));
      for (const r of c.redeemed ?? []) rows.push(csvRow([...base, names.subject(r.subject).name, r.created_at]));
    }
    download(rows.join("\n") + "\n", `redeem-batch-${batch}.csv`, "text/csv");
  };
  const columns = useMemo<ColumnDef<RedeemCode>[]>(
    () => [
      { id: "code", header: t("billing.op.codes.code"), meta: { className: "w-[110px]" }, cell: ({ row }) => <span className="font-mono text-sm" data-raw>…{row.original.last4}</span> },
      {
        id: "uses",
        header: t("billing.op.codes.used"),
        meta: { numeric: true, className: "w-[80px]" },
        cell: ({ row }) => `${row.original.uses} / ${row.original.max_uses}`,
      },
      {
        id: "who",
        header: t("billing.op.codes.who"),
        cell: ({ row }) => {
          const r = row.original.redeemed ?? [];
          if (r.length === 0) return <span className="text-ink-3">—</span>;
          return (
            <span className="block min-w-0 truncate text-sm" title={r.map((x) => `${names.subject(x.subject).name} · ${formatDateTime(x.created_at, locale)}`).join("\n")}>
              {r.slice(0, 2).map((x) => names.subject(x.subject).name).join("、")}
              {r.length > 2 && <span className="ml-1 text-xs text-ink-3">{t("billing.op.codes.andMore", { n: r.length - 2 })}</span>}
            </span>
          );
        },
      },
      {
        id: "state",
        header: t("billing.op.codes.state"),
        meta: { className: "w-[100px]" },
        cell: ({ row }) => {
          const s = codeState(row.original, now);
          return (
            <Pill tone={CODE_TONE[s]} dot>
              {t(`billing.op.codes.codeState.${s}`)}
            </Pill>
          );
        },
      },
      {
        id: "act",
        header: "",
        meta: { className: "w-[80px] text-right" },
        cell: ({ row }) => (
          <Button size="xs" variant="ghost" onClick={() => toggle.mutate(row.original)} disabled={toggle.isPending}>
            {row.original.disabled ? t("billing.op.codes.enable") : t("billing.op.codes.disable")}
          </Button>
        ),
      },
    ],
    [t, locale, names, now, toggle],
  );
  return (
    <Drawer
      open
      onOpenChange={(o) => !o && onClose()}
      size="lg"
      title={batch || t("billing.op.codes.noName")}
      subtitle={summary ? t("billing.op.codes.batchSub", { n: summary.codes, amount: money(summary.amount) }) : undefined}
      actions={
        <>
          <Button size="sm" onClick={exportCsv} disabled={!q.data}>
            <FileDown aria-hidden />
            {t("billing.op.codes.exportCsv")}
          </Button>
          <Button
            size="sm"
            variant={allOff ? "default" : "danger"}
            disabled={toggleBatch.isPending}
            onClick={async () => {
              if (!allOff && !(await confirm({ title: t("billing.op.codes.disableBatchTitle", { name: batch }), body: t("billing.op.codes.disableBatchBody"), confirm: t("billing.op.codes.disableBatch"), danger: true })))
                return;
              toggleBatch.mutate();
            }}
          >
            {allOff ? t("billing.op.codes.enableBatch") : t("billing.op.codes.disableBatch")}
          </Button>
        </>
      }
    >
      <div className="grid gap-3 p-5">
        <HelpText>{t("billing.op.codes.lastFour")}</HelpText>
        {q.isError ? (
          <ErrorState error={q.error} onRetry={() => void q.refetch()} />
        ) : (
          <DataTable columns={columns} data={q.data?.items} getRowId={(c) => c.id} maxHeight="none" loadingRows={5} empty={<EmptyState compact title={t("billing.op.codes.noCodes")} />} />
        )}
      </div>
    </Drawer>
  );
}
