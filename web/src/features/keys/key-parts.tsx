import { useMutation, useQueryClient } from "@tanstack/react-query";
import { AlertTriangle } from "lucide-react";
import { useState } from "react";
import { toast } from "sonner";
import { useClientLabel } from "@/components/client-name";
import { CodeBlock } from "@/components/code-block";
import { useConfirm } from "@/components/confirm";
import { useMoney } from "@/components/money";
import { Pill, type Tone } from "@/components/status";
import { Button } from "@/components/ui/button";
import { Dialog, DialogContent } from "@/components/ui/dialog";
import { Tooltip } from "@/components/ui/tooltip";
import { api, unwrap } from "@/lib/api/client";
import { errorMessage } from "@/lib/api/errors";
import type { Key, KeyCreated } from "@/lib/api/types";
import { cn } from "@/lib/cn";
import { useI18n } from "@/i18n";

const STATUS_TONE: Record<Key["status"], Tone> = { active: "ok", disabled: "neutral", expired: "neutral", over_budget: "warn" };

export function KeyStatusPill({ k }: { k: Key }) {
  const { t } = useI18n();
  return (
    <Pill tone={STATUS_TONE[k.status]} dot>
      {t(`keyStatus.${k.status}`)}
    </Pill>
  );
}

export const maskedKey = (k: Key) => `${k.prefix ?? "pr_"}••••${k.last4}`;

/** Spend this month, against the monthly budget when the key has one. */
export function SpendCell({ k, wide }: { k: Key; wide?: boolean }) {
  const money = useMoney();
  const budget = k.settings.budget_monthly;
  if (!budget) return <span className="tabular-nums">{money(k.month_spend)}</span>;
  const r = budget.micro > 0 ? Math.min(1, k.month_spend.micro / budget.micro) : 1;
  return (
    <div className={cn("ml-auto grid gap-1", wide ? "w-full" : "w-32")}>
      <div className="flex justify-between gap-2 text-xs tabular-nums">
        <span className="text-ink">{money(k.month_spend)}</span>
        <span className="text-ink-3">/ {money(budget)}</span>
      </div>
      <div className="h-1 overflow-hidden rounded-full bg-surface-2">
        <div className={cn("h-full rounded-full", r >= 1 ? "bg-bad" : r >= 0.8 ? "bg-warn" : "bg-accent")} style={{ width: `${Math.max(3, r * 100)}%` }} />
      </div>
    </div>
  );
}

export interface LimitItem {
  id: string;
  /** Short, for table chips: "$50 / 月". */
  short: string;
  /** Full sentence, for the drawer and tooltips. */
  long: string;
  tone?: "warn";
}

/** A key's own limits in plain words; empty when the key only follows its group. */
export function useLimitItems() {
  const { t, locale } = useI18n();
  const money = useMoney();
  const clientLabel = useClientLabel();
  return (k: Key): LimitItem[] => {
    const s = k.settings;
    const out: LimitItem[] = [];
    if (s.budget_monthly) out.push({ id: "monthly", short: t("keys.chip.monthly", { amount: money(s.budget_monthly) }), long: t("keys.limit.monthly", { amount: money(s.budget_monthly) }) });
    if (s.budget_total) out.push({ id: "total", short: t("keys.chip.total", { amount: money(s.budget_total) }), long: t("keys.limit.total", { amount: money(s.budget_total) }) });
    if (s.rpm) out.push({ id: "rpm", short: t("keys.chip.rpm", { n: s.rpm }), long: t("keys.limit.rpm", { n: s.rpm }) });
    if (s.max_concurrency) out.push({ id: "concurrency", short: t("keys.chip.concurrency", { n: s.max_concurrency }), long: t("keys.limit.concurrency", { n: s.max_concurrency }) });
    if (s.models?.length)
      out.push({
        id: "models",
        short: s.models.length === 1 ? s.models[0]! : t("keys.chip.models", { n: s.models.length }),
        long: t("keys.limit.models", { list: s.models.join(", ") }),
      });
    if (s.clients?.length) {
      const names = s.clients.map((id) => clientLabel({ id, name: id })).join(", ");
      out.push({ id: "clients", short: s.clients.length === 1 ? t("keys.chip.client", { name: names }) : t("keys.chip.clients", { n: s.clients.length }), long: t("keys.limit.clients", { list: names }) });
    }
    if (s.ip_allowlist?.length)
      out.push({ id: "ips", short: t("keys.chip.ips", { n: s.ip_allowlist.length }), long: t("keys.limit.ips", { list: s.ip_allowlist.join(", ") }) });
    if (s.expires_at) {
      const date = new Date(s.expires_at).toLocaleDateString(locale, { year: "numeric", month: "short", day: "numeric" });
      const soon = new Date(s.expires_at).getTime() - Date.now() < 7 * 86_400_000;
      out.push({ id: "expires", short: t("keys.chip.expires", { date }), long: t("keys.limit.expires", { date }), tone: soon && k.status !== "expired" ? "warn" : undefined });
    }
    if (s.exception) out.push({ id: "exception", short: t("keys.chip.exception"), long: t("keys.exceptionHint") });
    return out;
  };
}

export function LimitChips({ k, max = 3 }: { k: Key; max?: number }) {
  const { t } = useI18n();
  const items = useLimitItems()(k);
  if (items.length === 0) return <span className="text-xs text-ink-3">{t("keys.followsGroup")}</span>;
  const shown = items.slice(0, max);
  const rest = items.slice(max);
  return (
    <div className="flex flex-wrap items-center gap-1">
      {shown.map((i) => (
        <Tooltip key={i.id} content={i.long}>
          <span
            className={cn(
              "inline-flex h-5 max-w-40 items-center truncate rounded-xs px-1.5 text-2xs",
              i.tone === "warn" ? "bg-warn-soft text-warn" : "bg-surface-2 text-ink-2",
              i.id === "models" && k.settings.models?.length === 1 && "font-mono",
            )}
          >
            {i.short}
          </span>
        </Tooltip>
      ))}
      {rest.length > 0 && (
        <Tooltip content={rest.map((i) => i.long).join(" · ")}>
          <span className="inline-flex h-5 items-center rounded-xs px-1 text-2xs text-ink-3">+{rest.length}</span>
        </Tooltip>
      )}
    </div>
  );
}

/** Rotate / enable / disable / delete, each behind its confirm where it can break clients. */
export function useKeyActions({ onSecret, onDeleted }: { onSecret: (c: KeyCreated) => void; onDeleted?: (k: Key) => void }) {
  const { t } = useI18n();
  const qc = useQueryClient();
  const confirm = useConfirm();
  const act = useMutation({
    mutationFn: async ({ k, op }: { k: Key; op: "rotate" | "delete" | "toggle" }) => {
      if (op === "delete") return void (await unwrap(api.DELETE("/keys/{id}", { params: { path: { id: k.id } } })));
      if (op === "toggle") return void (await unwrap(api.PATCH("/keys/{id}", { params: { path: { id: k.id } }, body: { enabled: k.status === "disabled" } })));
      onSecret(await unwrap(api.POST("/keys/{id}/rotate", { params: { path: { id: k.id } } })));
    },
    onSuccess: (_d, { op, k }) => {
      void qc.invalidateQueries({ queryKey: ["keys"] });
      if (op === "delete") {
        toast.success(t("keys.deleted", { name: k.name }));
        onDeleted?.(k);
      }
      if (op === "toggle") toast.success(t(k.status === "disabled" ? "keys.enabled" : "keys.disabled", { name: k.name }));
      if (op === "rotate") toast.success(t("keys.rotated", { name: k.name }));
    },
    onError: (e) => toast.error(errorMessage(e)),
  });
  return {
    pending: act.isPending,
    rotate: async (k: Key) => {
      if (await confirm({ title: t("keys.rotateTitle", { name: k.name }), body: t("keys.rotateBody"), confirm: t("keys.rotate"), danger: true })) act.mutate({ k, op: "rotate" });
    },
    toggle: async (k: Key) => {
      if (k.status !== "disabled" && !(await confirm({ title: t("keys.disableTitle", { name: k.name }), body: t("keys.disableBody"), confirm: t("keys.disable") }))) return;
      act.mutate({ k, op: "toggle" });
    },
    remove: async (k: Key) => {
      if (await confirm({ title: t("keys.deleteTitle", { name: k.name }), body: t("keys.deleteBody"), confirm: t("common.delete"), danger: true })) act.mutate({ k, op: "delete" });
    },
  };
}

/** The full key, once. Moving on fills it into the connect snippets. */
export function SecretDialog({ created, onDone }: { created: KeyCreated | null; onDone: (connect: boolean) => void }) {
  const { t } = useI18n();
  const confirm = useConfirm();
  const [copied, setCopied] = useState(false);
  const close = async (connect: boolean) => {
    if (!copied && !(await confirm({ title: t("keys.secretLeaveTitle"), body: t("keys.secretLeaveBody"), confirm: t("keys.secretLeave") }))) return;
    setCopied(false);
    onDone(connect);
  };
  return (
    <Dialog open={created !== null} onOpenChange={(o) => !o && void close(false)}>
      {created && (
        <DialogContent
          title={t("keys.secretTitle", { name: created.key.name })}
          onRequestClose={() => void close(false)}
          footer={
            <Button variant="primary" onClick={() => void close(true)}>
              {t("keys.secretNext")}
            </Button>
          }
        >
          <div className="flex gap-2 rounded-md border border-warn/40 bg-warn-soft px-3 py-2 text-sm">
            <AlertTriangle className="mt-0.5 size-4 shrink-0 text-warn" />
            {t("keys.secretOnce")}
          </div>
          <div className="mt-3" onCopy={() => setCopied(true)} onClickCapture={(e) => (e.target as HTMLElement).closest("button") && setCopied(true)}>
            <CodeBlock code={created.secret} title={t("keys.secretLabel")} />
          </div>
        </DialogContent>
      )}
    </Dialog>
  );
}
