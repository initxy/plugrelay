import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import type { ColumnDef } from "@tanstack/react-table";
import { Plus, Wallet } from "lucide-react";
import { useMemo, useState } from "react";
import { toast } from "sonner";
import { DataTable } from "@/components/data-table";
import { Drawer } from "@/components/drawer";
import { FilterBar, FilterSearch } from "@/components/filter-bar";
import { HelpText, MoneyInput } from "@/components/form-kit";
import { EmptyState, ErrorState, PageHeader, Panel } from "@/components/page";
import { StatTile } from "@/components/stat-tile";
import { Button } from "@/components/ui/button";
import { Field } from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import { Select } from "@/components/ui/select";
import { useMoney } from "@/components/money";
import { errorMessage } from "@/lib/api/errors";
import { formatRelative } from "@/lib/format";
import { cn } from "@/lib/cn";
import { useDrawerParam, useUrlSearch } from "@/lib/use-url-search";
import { useI18n } from "@/i18n";
import { balanceApi, formKey, type AccountView } from "../api";
import { SubjectCell, TxnTable, useNames } from "../shared";
import { str } from "./parts";
import { useForm } from "@/lib/use-form";

const accountsQuery = { queryKey: ["billing", "accounts"], queryFn: () => balanceApi.accounts() };

/** Prepaid balances: every account, adjustments with a reason, credit limits and each account's history. */
export function BalancesPage() {
  const { t, locale } = useI18n();
  const money = useMoney();
  const names = useNames();
  const [search, set] = useUrlSearch();
  const q = str(search.q);
  const drawer = useDrawerParam();
  const [picking, setPicking] = useState(false);
  const accounts = useQuery(accountsQuery);
  const cur = accounts.data?.currency ?? "USD";
  const items = useMemo(() => {
    const all = accounts.data?.items ?? [];
    if (!q) return all;
    const needle = q.toLowerCase();
    return all.filter((a) => {
      const n = names.subject(a.subject);
      return [n.name, n.detail ?? ""].some((s) => s.toLowerCase().includes(needle));
    });
  }, [accounts.data, q, names]);
  const all = accounts.data?.items ?? [];
  const total = all.reduce((s, a) => s + a.balance.micro, 0);
  const held = all.reduce((s, a) => s + a.held.micro, 0);
  const negative = all.filter((a) => a.available.micro < 0).length;

  const columns = useMemo<ColumnDef<AccountView>[]>(
    () => [
      { id: "account", header: t("billing.op.bal.account"), cell: ({ row }) => <SubjectCell name={names.subject(row.original.subject)} /> },
      {
        id: "available",
        header: t("billing.op.bal.available"),
        meta: { numeric: true, className: "w-[140px]" },
        cell: ({ row }) => <span className={cn("font-medium", row.original.available.micro < 0 && "text-bad")}>{money(row.original.available)}</span>,
      },
      { id: "held", header: t("billing.op.bal.held"), meta: { numeric: true, className: "w-[130px] max-md:hidden" }, cell: ({ row }) => money(row.original.held) },
      {
        id: "credit",
        header: t("billing.op.bal.credit"),
        meta: { numeric: true, className: "w-[130px] max-md:hidden" },
        cell: ({ row }) => (row.original.credit_limit.micro > 0 ? money(row.original.credit_limit) : <span className="text-ink-3">—</span>),
      },
      {
        id: "updated",
        header: t("billing.op.bal.updated"),
        meta: { className: "w-[130px] max-lg:hidden" },
        cell: ({ row }) => <span className="text-xs text-ink-3">{row.original.updated_at ? formatRelative(row.original.updated_at, locale) : "—"}</span>,
      },
    ],
    [t, locale, money, names],
  );

  return (
    <>
      <PageHeader
        title={t("nav.billing.balances")}
        description={t("billing.op.bal.description")}
        actions={
          <Button variant="primary" onClick={() => setPicking(true)}>
            <Plus aria-hidden />
            {t("billing.op.bal.adjust")}
          </Button>
        }
      />
      <div className="mb-4 grid grid-cols-2 gap-3 lg:grid-cols-4">
        <StatTile label={t("billing.op.bal.statTotal")} loading={accounts.isPending} value={money({ micro: total, currency: cur })} note={t("billing.op.bal.statAccounts", { n: all.length })} />
        <StatTile label={t("billing.op.bal.statHeld")} loading={accounts.isPending} value={money({ micro: held, currency: cur })} note={t("billing.op.bal.statHeldNote")} />
        <StatTile label={t("billing.op.bal.statNegative")} loading={accounts.isPending} value={negative} note={t("billing.op.bal.statNegativeNote")} />
        <StatTile
          label={t("billing.op.bal.statPayer")}
          loading={accounts.isPending}
          value={<span className="text-base">{t(`billing.op.payer.${accounts.data?.payer ?? "user"}`)}</span>}
          note={t("billing.op.bal.statPayerNote")}
        />
      </div>
      {accounts.isError ? (
        <ErrorState error={accounts.error} onRetry={() => void accounts.refetch()} />
      ) : (
        <DataTable
          columns={columns}
          data={accounts.data ? items : undefined}
          getRowId={(a) => a.subject}
          onRowClick={(a) => drawer.open(a.subject)}
          selectedId={drawer.id}
          toolbar={
            <FilterBar>
              <FilterSearch value={q} onChange={(v) => set({ q: v }, { replace: true })} placeholder={t("billing.op.bal.search")} />
            </FilterBar>
          }
          empty={<EmptyState icon={Wallet} title={q ? t("billing.op.bal.noMatch") : t("billing.op.bal.empty")} hint={q ? undefined : t("billing.op.bal.emptyHint")} />}
        />
      )}
      <PickAccount open={picking} onClose={() => setPicking(false)} onPick={(s) => (setPicking(false), drawer.open(s))} />
      {drawer.id && (
        <AccountDrawer
          key={drawer.id}
          subject={drawer.id}
          account={all.find((a) => a.subject === drawer.id)}
          currency={cur}
          onClose={drawer.close}
        />
      )}
    </>
  );
}

/** Choose a user, group or key whose account to open, also one that has never been credited. */
function PickAccount({ open, onClose, onPick }: { open: boolean; onClose: () => void; onPick: (subject: string) => void }) {
  const { t } = useI18n();
  const names = useNames();
  const form = useForm({ kind: "user" as "user" | "group" | "key", id: "" });
  const { kind, id } = form.values;
  const options =
    kind === "user"
      ? names.users.map((u) => ({ value: u.id, label: u.display_name ? `${u.display_name} · ${u.username}` : u.username }))
      : kind === "group"
        ? names.groups.map((g) => ({ value: g.id, label: g.name }))
        : names.keys.map((k) => ({ value: k.id, label: k.owner?.name ? `${k.name} · ${k.owner.name}` : k.name }));
  return (
    <Drawer
      open={open}
      onOpenChange={(o) => !o && onClose()}
      title={t("billing.op.bal.pickTitle")}
      subtitle={t("billing.op.bal.pickHint")}
      size="sm"
      footer={
        <>
          <Button onClick={onClose}>{t("common.cancel")}</Button>
          <Button variant="primary" disabled={!id} onClick={() => onPick(`${kind}:${id}`)}>
            {t("common.next")}
          </Button>
        </>
      }
    >
      <div className="grid gap-4 p-5">
        <Field label={t("billing.op.bal.pickKind")}>
          <Select
            value={kind}
            onChange={(v) => form.setValues({ kind: v, id: "" })}
            options={[
              { value: "user", label: t("billing.op.bal.kindUser") },
              { value: "group", label: t("billing.op.bal.kindGroup") },
              { value: "key", label: t("billing.op.bal.kindKey") },
            ]}
          />
        </Field>
        <Field label={t(`billing.op.bal.kind${kind === "user" ? "User" : kind === "group" ? "Group" : "Key"}`)}>
          <Select value={id || undefined} onChange={(v) => form.set("id", v)} options={options} placeholder={t("billing.op.bal.pickPlaceholder")} />
        </Field>
      </div>
    </Drawer>
  );
}

function AccountDrawer({ subject, account, currency, onClose }: { subject: string; account?: AccountView; currency: string; onClose: () => void }) {
  const { t } = useI18n();
  const money = useMoney();
  const qc = useQueryClient();
  const names = useNames();
  const name = names.subject(subject);
  const zero = { micro: 0, currency };
  const blankAdjust = () => ({ amount: null as number | null, note: "", key: formKey() });
  const adjustForm = useForm(blankAdjust());
  const { amount, note, key } = adjustForm.values;
  const limitForm = useForm({ limit: account ? account.credit_limit.micro / 1_000_000 : (0 as number | null) });
  const { limit } = limitForm.values;
  const refresh = () => {
    void qc.invalidateQueries({ queryKey: accountsQuery.queryKey });
    void qc.invalidateQueries({ queryKey: ["billing", "txns", subject] });
  };
  const adjust = useMutation({
    mutationFn: () => balanceApi.adjust({ subject, amount_micro: Math.round((amount ?? 0) * 1_000_000), note: note.trim(), idempotency_key: key }),
    onSuccess: (r) => {
      toast.success(r.applied ? t("billing.op.bal.adjusted") : t("billing.op.bal.alreadyApplied"));
      adjustForm.reset(blankAdjust());
      refresh();
    },
    onError: (e) => toast.error(errorMessage(e)),
  });
  const credit = useMutation({
    mutationFn: () => balanceApi.creditLimit({ subject, credit_limit_micro: Math.round((limit ?? 0) * 1_000_000) }),
    onSuccess: () => {
      toast.success(t("billing.op.bal.limitSaved"));
      refresh();
    },
    onError: (e) => toast.error(errorMessage(e)),
  });
  const a = account;
  return (
    <Drawer open onOpenChange={(o) => !o && onClose()} title={name.name} subtitle={name.detail} size="lg">
      <div className="grid gap-5 p-5">
        <div className="grid grid-cols-3 gap-3">
          <StatTile label={t("billing.op.bal.available")} value={<span className={cn((a?.available.micro ?? 0) < 0 && "text-bad")}>{money(a?.available ?? zero)}</span>} />
          <StatTile label={t("billing.op.bal.held")} value={money(a?.held ?? zero)} note={t("billing.op.bal.heldNote")} />
          <StatTile label={t("billing.op.bal.credit")} value={money(a?.credit_limit ?? zero)} note={t("billing.op.bal.creditNote")} />
        </div>
        <div className="grid gap-4 md:grid-cols-2">
          <Panel title={t("billing.op.bal.adjustTitle")}>
            <form
              className="grid gap-3"
              onSubmit={(e) => {
                e.preventDefault();
                adjust.mutate();
              }}
            >
              <Field label={t("billing.op.bal.amount", { currency })} hint={t("billing.op.bal.amountHint")}>
                <MoneyInput value={amount} onChange={(v) => adjustForm.set("amount", v)} currency={currency} placeholder="10.00" />
              </Field>
              <Field label={t("billing.op.bal.note")} hint={t("billing.op.bal.noteHint")}>
                <Input value={note} onChange={(e) => adjustForm.set("note", e.target.value)} maxLength={200} />
              </Field>
              <Button type="submit" variant="primary" disabled={!amount || adjust.isPending} className="justify-self-start">
                {adjust.isPending ? t("common.saving") : t("billing.op.bal.adjust")}
              </Button>
            </form>
          </Panel>
          <Panel title={t("billing.op.bal.limitTitle")}>
            <form
              className="grid gap-3"
              onSubmit={(e) => {
                e.preventDefault();
                credit.mutate();
              }}
            >
              <Field label={t("billing.op.bal.credit")} hint={t("billing.op.bal.limitHint")}>
                <MoneyInput value={limit} onChange={(v) => limitForm.set("limit", v)} currency={currency} />
              </Field>
              <HelpText>{t("billing.op.bal.limitHelp")}</HelpText>
              <Button type="submit" disabled={limit === null || limit < 0 || credit.isPending} className="justify-self-start">
                {credit.isPending ? t("common.saving") : t("billing.op.bal.limitSave")}
              </Button>
            </form>
          </Panel>
        </div>
        <Panel title={t("billing.op.bal.history")} bodyClassName="p-0">
          <TxnTable queryKey={["billing", "txns", subject]} load={(q) => balanceApi.txns({ subject, ...q })} operator />
        </Panel>
      </div>
    </Drawer>
  );
}
