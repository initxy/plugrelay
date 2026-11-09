import { useMutation } from "@tanstack/react-query";
import type { ColumnDef } from "@tanstack/react-table";
import { AlertTriangle, Check } from "lucide-react";
import { useCallback, useMemo, useState } from "react";
import { toast } from "sonner";
import { DataTable } from "@/components/data-table";
import { HelpText } from "@/components/form-kit";
import { ErrorState, PageHeader, Panel } from "@/components/page";
import { Pill, type Tone } from "@/components/status";
import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import { Field } from "@/components/ui/field";
import { Input, Textarea } from "@/components/ui/input";
import { Segmented } from "@/components/ui/segmented";
import { cn } from "@/lib/cn";
import { useI18n } from "@/i18n";
import { importApi, type ImportItem, type ImportReport, type ImportSource } from "../api";

type Driver = "mysql" | "postgres" | "sqlite";
const OP_TONE: Record<ImportItem["op"], Tone> = { create: "ok", update: "info", unchanged: "neutral", skip: "warn" };

/** Moves a new-api or one-api site over: read, check what would change, then import. */
export function ImportPage() {
  const { t } = useI18n();
  const [mode, setMode] = useState<"db" | "json">("db");
  const [driver, setDriver] = useState<Driver>("mysql");
  const [dsn, setDsn] = useState("");
  const [local, setLocal] = useState(false);
  const [json, setJson] = useState("");
  const [rate, setRate] = useState("");
  const [dry, setDry] = useState<ImportReport>();
  const [done, setDone] = useState<ImportReport>();
  const [jsonBad, setJsonBad] = useState(false);
  // Any change to the input sends the wizard back to step 1.
  const reset = () => {
    setDry(undefined);
    setDone(undefined);
  };
  const edit =
    <T,>(f: (v: T) => void) =>
    (v: T) => {
      f(v);
      reset();
    };
  const source = (): ImportSource | undefined => {
    if (mode === "db") return { driver, dsn: dsn.trim(), confirm_local_file: driver === "sqlite" ? local : undefined };
    try {
      return { data: JSON.parse(json) };
    } catch {
      setJsonBad(true);
      return undefined;
    }
  };
  const rateNum = rate.trim() ? Number(rate) : undefined;
  const rateBad = rateNum !== undefined && !(rateNum > 0);
  const ready = !rateBad && (mode === "db" ? dsn.trim() !== "" && (driver !== "sqlite" || local) : json.trim() !== "");
  const run = useMutation({
    mutationFn: (dryRun: boolean) => {
      const src = source();
      if (!src) throw new Error(t("billing.op.imp.jsonBad"));
      return importApi.run(src, { rate: rateNum, dry_run: dryRun });
    },
    onSuccess: (r) => {
      if (r.dry_run) setDry(r);
      else {
        setDone(r);
        toast.success(t("billing.op.imp.imported"));
      }
    },
  });
  const step = done ? 3 : dry ? 2 : 1;
  return (
    <>
      <PageHeader title={t("nav.billing.import")} description={t("billing.op.imp.description")} />
      <ol className="mb-4 flex flex-wrap gap-2 text-sm">
        {[1, 2, 3].map((n) => (
          <li
            key={n}
            className={cn(
              "flex items-center gap-2 rounded-full border px-3 py-1",
              n === step ? "border-accent bg-accent-soft text-accent-strong" : n < step ? "border-line text-ink-2" : "border-line text-ink-3",
            )}
          >
            <span className="grid size-5 place-items-center rounded-full bg-surface text-xs font-semibold">{n < step ? <Check className="size-3.5 text-ok" /> : n}</span>
            {t(`billing.op.imp.step${n}` as "billing.op.imp.step1")}
          </li>
        ))}
      </ol>
      <Panel title={t("billing.op.imp.sourceTitle")} className="mb-4">
        <div className="grid gap-4">
          <Segmented
            aria-label={t("billing.op.imp.source")}
            value={mode}
            onChange={edit(setMode)}
            options={[
              { value: "db", label: t("billing.op.imp.fromDb") },
              { value: "json", label: t("billing.op.imp.fromJson") },
            ]}
          />
          {mode === "db" ? (
            <>
              <Field label={t("billing.op.imp.driver")}>
                <Segmented
                  value={driver}
                  onChange={edit(setDriver)}
                  options={[
                    { value: "mysql", label: "MySQL" },
                    { value: "postgres", label: "PostgreSQL" },
                    { value: "sqlite", label: "SQLite" },
                  ]}
                />
              </Field>
              <Field label={t("billing.op.imp.dsn")} hint={t("billing.op.imp.dsnHint")}>
                <Input value={dsn} onChange={(e) => edit(setDsn)(e.target.value)} className="font-mono" placeholder="user:pass@tcp(db:3306)/new-api" autoComplete="off" />
              </Field>
              {driver === "sqlite" && (
                <label className="flex items-start gap-2 text-sm">
                  <Checkbox checked={local} onChange={edit(setLocal)} className="mt-0.5" />
                  <span>
                    {t("billing.op.imp.localFile")}
                    <span className="block text-xs text-ink-3">{t("billing.op.imp.localFileHint")}</span>
                  </span>
                </label>
              )}
            </>
          ) : (
            <Field label={t("billing.op.imp.json")} hint={t("billing.op.imp.jsonHint")} error={jsonBad ? t("billing.op.imp.jsonBad") : undefined}>
              <Textarea
                value={json}
                onChange={(e) => {
                  setJsonBad(false);
                  edit(setJson)(e.target.value);
                }}
                rows={8}
                className="font-mono text-xs"
              />
            </Field>
          )}
          <Field label={t("billing.op.imp.rate")} optional hint={t("billing.op.imp.rateHint")} error={rateBad ? t("billing.op.imp.rateBad") : undefined}>
            <Input inputMode="decimal" value={rate} onChange={(e) => edit(setRate)(e.target.value)} placeholder="1" className="w-40" />
          </Field>
          <div className="flex flex-wrap items-center gap-2">
            <Button variant={dry ? "default" : "primary"} disabled={!ready || run.isPending} onClick={() => run.mutate(true)}>
              {run.isPending && run.variables ? t("billing.op.imp.checking") : t("billing.op.imp.dryRun")}
            </Button>
            <Button variant={dry && !done ? "primary" : "default"} disabled={!dry || !!done || run.isPending} onClick={() => run.mutate(false)}>
              {run.isPending && !run.variables ? t("billing.op.imp.importing") : t("billing.op.imp.import")}
            </Button>
            <HelpText>{dry ? t("billing.op.imp.readyHint") : t("billing.op.imp.dryFirst")}</HelpText>
          </div>
        </div>
      </Panel>
      {run.isError && <ErrorState error={run.error} />}
      {(done ?? dry) && <ReportView report={(done ?? dry)!} />}
    </>
  );
}

function ReportView({ report }: { report: ImportReport }) {
  const { t, has } = useI18n();
  const kind = useCallback(
    (k: string) => (has(`billing.op.imp.kind.${k}`) ? t(`billing.op.imp.kind.${k}` as "billing.op.imp.kind.user") : k),
    [t, has],
  );
  const columns = useMemo<ColumnDef<ImportItem>[]>(
    () => [
      { id: "what", header: t("billing.op.imp.what"), meta: { className: "w-[120px]" }, cell: ({ row }) => kind(row.original.kind) },
      {
        id: "id",
        header: t("billing.op.imp.id"),
        meta: { className: "w-[220px]" },
        cell: ({ row }) => (
          <span className="font-mono text-xs" data-raw>
            {row.original.id}
          </span>
        ),
      },
      {
        id: "op",
        header: t("billing.op.imp.result"),
        meta: { className: "w-[110px]" },
        cell: ({ row }) => (
          <Pill tone={OP_TONE[row.original.op]} dot>
            {t(`billing.op.imp.op.${row.original.op}`)}
          </Pill>
        ),
      },
      {
        id: "detail",
        header: t("billing.op.imp.detail"),
        cell: ({ row }) =>
          row.original.reason ? (
            <span className="block min-w-0 truncate text-xs text-ink-2" data-raw title={row.original.reason}>
              {row.original.reason}
            </span>
          ) : null,
      },
    ],
    [t, kind],
  );
  const counts = Object.entries(report.counts).sort(([a], [b]) => a.localeCompare(b));
  return (
    <Panel title={report.dry_run ? t("billing.op.imp.dryTitle") : t("billing.op.imp.doneTitle")} bodyClassName="p-0">
      <div className="flex flex-wrap gap-1.5 border-b border-line-soft px-4 py-3">
        {counts.map(([k, n]) => (
          <Pill key={k} tone={k === "skipped" ? "warn" : "neutral"}>
            {t("billing.op.imp.count", { what: kind(k), n })}
          </Pill>
        ))}
      </div>
      {report.warnings.length > 0 && (
        <div className="border-b border-line-soft px-4 py-3">
          <p className="mb-1.5 flex items-center gap-1.5 text-sm font-medium text-warn">
            <AlertTriangle className="size-4" aria-hidden />
            {t("billing.op.imp.warnings", { n: report.warnings.length })}
          </p>
          <ul className="grid gap-1 text-xs text-ink-2" data-raw>
            {report.warnings.map((w, i) => (
              <li key={i}>{w}</li>
            ))}
          </ul>
        </div>
      )}
      <DataTable columns={columns} data={report.items} getRowId={(r) => `${r.kind}:${r.id}:${r.source ?? ""}`} maxHeight="60vh" className="rounded-none border-0" />
    </Panel>
  );
}

