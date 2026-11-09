import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import type { ColumnDef } from "@tanstack/react-table";
import { CalendarClock, MoreHorizontal, Plus } from "lucide-react";
import { useMemo } from "react";
import { toast } from "sonner";
import { useConfirm } from "@/components/confirm";
import { DataTable } from "@/components/data-table";
import { Drawer } from "@/components/drawer";
import { FormSection, HelpText, MoneyInput, SwitchRow } from "@/components/form-kit";
import { EmptyState, ErrorState, PageHeader, Panel } from "@/components/page";
import { Pill } from "@/components/status";
import { Button } from "@/components/ui/button";
import { Field } from "@/components/ui/field";
import { Input, Textarea } from "@/components/ui/input";
import { Menu, MenuContent, MenuItem, MenuTrigger } from "@/components/ui/menu";
import { Segmented } from "@/components/ui/segmented";
import { Select } from "@/components/ui/select";
import { useMoney } from "@/components/money";
import { useReauth } from "@/features/auth/use-reauth";
import { api, unwrap } from "@/lib/api/client";
import { errorMessage } from "@/lib/api/errors";
import { formatDateTime } from "@/lib/format";
import { useDrawerParam, useUrlSearch } from "@/lib/use-url-search";
import { useI18n } from "@/i18n";
import { plansApi, type AllowanceUnit, type Overage, type Period, type Plan, type PlanInput, type SubView } from "../api";
import { SubjectCell, useAllowanceText, useNames, usePayMethodLabel } from "../shared";
import { str } from "./parts";
import { useForm } from "@/lib/use-form";

const plansQuery = { queryKey: ["billing", "plans", "all"], queryFn: () => plansApi.plans(true) };
const configQuery = { queryKey: ["billing", "plans", "config"], queryFn: () => plansApi.config() };
const SUB_FILTERS = ["all", "active", "pending", "ended"] as const;
type SubFilter = (typeof SUB_FILTERS)[number];

/** Subscription plans and who holds them. */
export function PlansPage() {
  const { t } = useI18n();
  const money = useMoney();
  const allowance = useAllowanceText();
  const drawer = useDrawerParam();
  const q = useQuery(plansQuery);
  const config = useQuery(configQuery);
  const names = useNames();
  const columns = useMemo<ColumnDef<Plan>[]>(
    () => [
      {
        id: "name",
        header: t("billing.op.pl.name"),
        cell: ({ row }) => (
          <span className="grid min-w-0">
            <span className="truncate font-medium">{row.original.name}</span>
            {row.original.description && <span className="truncate text-xs text-ink-3">{row.original.description}</span>}
          </span>
        ),
      },
      {
        id: "price",
        header: t("billing.op.pl.price"),
        meta: { numeric: true, className: "w-[150px]" },
        cell: ({ row }) => t(`billing.op.pl.per.${row.original.period}`, { price: money({ micro: row.original.price_micro, currency: row.original.currency }) }),
      },
      {
        id: "allowance",
        header: t("billing.op.pl.allowance"),
        meta: { numeric: true, className: "w-[160px]" },
        cell: ({ row }) => allowance(row.original.allowance, row.original.allowance_unit, row.original.currency),
      },
      {
        id: "overage",
        header: t("billing.op.pl.overage"),
        meta: { className: "w-[170px] max-md:hidden" },
        cell: ({ row }) => <span className="text-sm text-ink-2">{t(`billing.op.pl.overageShort.${row.original.overage}`)}</span>,
      },
      { id: "group", header: t("billing.op.pl.group"), meta: { className: "w-[120px] max-lg:hidden" }, cell: ({ row }) => (row.original.group_id ? names.subject(`group:${row.original.group_id}`).name : <span className="text-ink-3">—</span>) },
      {
        id: "state",
        header: t("billing.op.pl.state"),
        meta: { className: "w-[120px]" },
        cell: ({ row }) => (
          <Pill tone={row.original.disabled ? "neutral" : "ok"} dot>
            {row.original.disabled ? t("billing.op.pl.closed") : t("billing.op.pl.open")}
          </Pill>
        ),
      },
    ],
    [t, money, allowance, names],
  );
  const add = (
    <Button variant="primary" onClick={() => drawer.open("new")}>
      <Plus aria-hidden />
      {t("billing.op.pl.add")}
    </Button>
  );
  const items = q.data?.items;
  return (
    <>
      <PageHeader title={t("nav.billing.plans")} description={t("billing.op.pl.description")} actions={add} />
      {q.isError ? (
        <ErrorState error={q.error} onRetry={() => void q.refetch()} />
      ) : (
        <DataTable
          columns={columns}
          data={items}
          getRowId={(p) => p.id}
          onRowClick={(p) => drawer.open(p.id)}
          selectedId={drawer.id}
          maxHeight="none"
          empty={<EmptyState icon={CalendarClock} title={t("billing.op.pl.empty")} hint={t("billing.op.pl.emptyHint")} action={add} />}
        />
      )}
      <Subscriptions plans={items ?? []} />
      {drawer.id && (drawer.id === "new" || items) && (
        <PlanDrawer
          key={drawer.id}
          plan={drawer.id === "new" ? undefined : items?.find((p) => p.id === drawer.id)}
          currency={config.data?.currency ?? "USD"}
          groups={names.groups.map((g) => ({ value: g.id, label: g.name }))}
          onClose={drawer.close}
        />
      )}
    </>
  );
}

interface Form {
  name: string;
  description: string;
  price: number | null;
  period: Period;
  unit: AllowanceUnit;
  money: number | null;
  tokens: string;
  overage: Overage;
  group: string;
  disabled: boolean;
}

function PlanDrawer({ plan, currency, groups, onClose }: { plan?: Plan; currency: string; groups: { value: string; label: string }[]; onClose: () => void }) {
  const { t } = useI18n();
  const qc = useQueryClient();
  const form = useForm<Form>({
    name: plan?.name ?? "",
    description: plan?.description ?? "",
    price: plan ? plan.price_micro / 1e6 : null,
    period: plan?.period ?? "month",
    unit: plan?.allowance_unit ?? "money",
    money: plan && plan.allowance_unit === "money" ? plan.allowance / 1e6 : null,
    tokens: plan && plan.allowance_unit === "tokens" ? String(plan.allowance) : "",
    overage: plan?.overage ?? "block",
    group: plan?.group_id ?? "",
    disabled: plan?.disabled ?? false,
  });
  const f = form.values;
  const tokens = /^\d+$/.test(f.tokens.trim()) ? Number(f.tokens) : NaN;
  const errs = {
    name: !f.name.trim() ? t("billing.op.pl.nameBad") : undefined,
    price: f.price === null || f.price < 0 ? t("billing.op.pl.priceBad") : undefined,
    allowance: f.unit === "money" ? (!(f.money && f.money > 0) ? t("billing.op.pl.allowanceBad") : undefined) : !(tokens > 0) ? t("billing.op.pl.tokensBad") : undefined,
  };
  const save = useMutation({
    mutationFn: () => {
      const body: PlanInput = {
        id: plan?.id,
        name: f.name.trim(),
        description: f.description.trim() || undefined,
        price_micro: Math.round((f.price ?? 0) * 1e6),
        period: f.period,
        allowance_unit: f.unit,
        allowance: f.unit === "money" ? Math.round((f.money ?? 0) * 1e6) : tokens,
        overage: f.overage,
        group_id: f.group || undefined,
        disabled: f.disabled,
      };
      return plansApi.savePlan(body);
    },
    onSuccess: () => {
      toast.success(t("billing.op.pl.saved"));
      void qc.invalidateQueries({ queryKey: plansQuery.queryKey });
      onClose();
    },
    onError: (e) => toast.error(errorMessage(e)),
  });
  return (
    <Drawer
      open
      onOpenChange={(o) => !o && onClose()}
      title={plan ? plan.name : t("billing.op.pl.add")}
      subtitle={plan ? t("billing.op.pl.editHint") : t("billing.op.pl.addHint")}
      footer={
        <>
          <Button onClick={onClose}>{t("common.cancel")}</Button>
          <Button variant="primary" disabled={Object.values(errs).some(Boolean) || save.isPending} onClick={() => save.mutate()}>
            {save.isPending ? t("common.saving") : t("common.save")}
          </Button>
        </>
      }
    >
      <div className="p-5">
        <FormSection title={t("billing.op.pl.secBasics")}>
          <Field label={t("billing.op.pl.name")} error={f.name ? undefined : errs.name}>
            <Input value={f.name} onChange={(e) => form.set("name", e.target.value)} maxLength={80} placeholder="Pro" />
          </Field>
          <Field label={t("billing.op.pl.descriptionField")} optional>
            <Textarea value={f.description} onChange={(e) => form.set("description", e.target.value)} rows={2} />
          </Field>
          <div className="grid grid-cols-2 gap-4">
            <Field label={t("billing.op.pl.price")} error={errs.price && f.price !== null ? errs.price : undefined}>
              <MoneyInput value={f.price} onChange={(v) => form.set("price", v)} currency={currency} placeholder="20.00" />
            </Field>
            <Field label={t("billing.op.pl.period")}>
              <Segmented
                value={f.period}
                onChange={(v) => form.set("period", v)}
                options={(["week", "month", "year"] as const).map((p) => ({ value: p, label: t(`billing.op.pl.period.${p}`) }))}
              />
            </Field>
          </div>
        </FormSection>
        <FormSection title={t("billing.op.pl.secAllowance")} description={t("billing.op.pl.allowanceHelp")}>
          <Segmented
            aria-label={t("billing.op.pl.unit")}
            value={f.unit}
            onChange={(v) => form.set("unit", v)}
            options={[
              { value: "money", label: t("billing.op.pl.unitMoney") },
              { value: "tokens", label: t("billing.op.pl.unitTokens") },
            ]}
          />
          {f.unit === "money" ? (
            <Field label={t("billing.op.pl.allowanceMoney")}>
              <MoneyInput value={f.money} onChange={(v) => form.set("money", v)} currency={currency} placeholder="30.00" />
            </Field>
          ) : (
            <Field label={t("billing.op.pl.allowanceTokens")} error={f.tokens ? errs.allowance : undefined}>
              <Input inputMode="numeric" value={f.tokens} onChange={(e) => form.set("tokens", e.target.value)} placeholder="5000000" />
            </Field>
          )}
          <Field label={t("billing.op.pl.overage")}>
            <Segmented
              value={f.overage}
              onChange={(v) => form.set("overage", v)}
              options={[
                { value: "block", label: t("billing.op.pl.overageShort.block") },
                { value: "balance", label: t("billing.op.pl.overageShort.balance") },
              ]}
            />
          </Field>
        </FormSection>
        <FormSection title={t("billing.op.pl.secAccess")}>
          <Field label={t("billing.op.pl.group")} optional hint={t("billing.op.pl.groupHint")}>
            <Select value={f.group || "none"} onChange={(v) => form.set("group", v === "none" ? "" : v)} options={[{ value: "none", label: t("billing.op.pl.noGroup") }, ...groups]} />
          </Field>
          <SwitchRow label={t("billing.op.pl.closeNew")} description={t("billing.op.pl.closeNewHint")} checked={f.disabled} onChange={(v) => form.set("disabled", v)} />
          <HelpText>{t("billing.op.pl.copyTerms")}</HelpText>
        </FormSection>
      </div>
    </Drawer>
  );
}

function Subscriptions({ plans }: { plans: Plan[] }) {
  const { t, locale } = useI18n();
  const names = useNames();
  const method = usePayMethodLabel();
  const allowance = useAllowanceText();
  const qc = useQueryClient();
  const confirm = useConfirm();
  const [search, set] = useUrlSearch();
  const raw = str(search.subs);
  const filter: SubFilter = (SUB_FILTERS as readonly string[]).includes(raw ?? "") ? (raw as SubFilter) : "all";
  const q = useQuery({ queryKey: ["billing", "subscriptions", filter], queryFn: () => plansApi.subscriptions(filter === "all" ? undefined : filter) });
  const security = useQuery({ queryKey: ["me", "security"], queryFn: () => unwrap(api.GET("/me/security")), retry: false });
  const { run, dialog } = useReauth(security.data);
  const refresh = () => void qc.invalidateQueries({ queryKey: ["billing", "subscriptions"] });
  const act = useMutation({
    mutationFn: ({ s, what }: { s: SubView; what: "cancel" | "resume" | "end" | "refunded" }) => {
      if (what === "cancel" || what === "resume") return plansApi.cancel(s.id, what === "cancel");
      if (what === "end") return run(() => plansApi.end(s.id));
      return run(() => plansApi.refunded(s.id, true));
    },
    onSuccess: (_, v) => {
      toast.success(t(`billing.op.sub.done.${v.what}`));
      refresh();
    },
    onError: (e) => toast.error(errorMessage(e)),
  });
  const columns = useMemo<ColumnDef<SubView>[]>(
    () => [
      { id: "user", header: t("billing.op.sub.user"), cell: ({ row }) => <SubjectCell name={names.user(row.original.user_id)} /> },
      { id: "plan", header: t("billing.op.sub.plan"), meta: { className: "w-[140px]" }, cell: ({ row }) => row.original.plan_name },
      {
        id: "status",
        header: t("billing.op.sub.status"),
        meta: { className: "w-[220px]" },
        cell: ({ row }) => {
          const s = row.original;
          return (
            <span className="flex flex-wrap gap-1">
              <Pill tone={s.status === "active" ? "ok" : s.status === "pending" ? "warn" : "neutral"} dot>
                {t(`billing.op.sub.state.${s.status}`)}
              </Pill>
              {s.cancel_at_period_end && s.status === "active" && <Pill tone="warn">{t("billing.op.sub.endsAtPeriodEnd")}</Pill>}
              {s.refund_due && (
                <Pill tone="bad" title={t("billing.op.sub.refundDueHint")}>
                  {t("billing.op.sub.refundDue")}
                </Pill>
              )}
            </span>
          );
        },
      },
      {
        id: "used",
        header: t("billing.op.sub.used"),
        meta: { numeric: true, className: "w-[200px] max-md:hidden" },
        cell: ({ row }) => `${allowance(row.original.used, row.original.allowance_unit, row.original.currency)} / ${allowance(row.original.allowance, row.original.allowance_unit, row.original.currency)}`,
      },
      {
        id: "end",
        header: t("billing.op.sub.periodEnd"),
        meta: { className: "w-[150px] max-lg:hidden" },
        cell: ({ row }) => <span className="text-xs text-ink-2">{row.original.period_end ? formatDateTime(row.original.period_end, locale) : "—"}</span>,
      },
      {
        id: "paid",
        header: t("billing.op.sub.paidWith"),
        meta: { className: "w-[130px] max-lg:hidden" },
        cell: ({ row }) => <span className="text-sm text-ink-2">{method({ provider: row.original.provider })}</span>,
      },
      {
        id: "act",
        header: "",
        meta: { className: "w-[52px] text-right" },
        cell: ({ row }) => {
          const s = row.original;
          if (s.status === "ended" && !s.refund_due) return null;
          return (
            <Menu>
              <MenuTrigger asChild>
                <Button size="icon-sm" variant="ghost" aria-label={t("common.more")}>
                  <MoreHorizontal />
                </Button>
              </MenuTrigger>
              <MenuContent>
                {s.status === "active" &&
                  (s.cancel_at_period_end ? (
                    <MenuItem onSelect={() => act.mutate({ s, what: "resume" })}>{t("billing.op.sub.resume")}</MenuItem>
                  ) : (
                    <MenuItem onSelect={() => act.mutate({ s, what: "cancel" })}>{t("billing.op.sub.cancelAtEnd")}</MenuItem>
                  ))}
                {s.status !== "ended" && (
                  <MenuItem
                    danger
                    onSelect={async () => {
                      if (await confirm({ title: t("billing.op.sub.endTitle"), body: t("billing.op.sub.endBody"), confirm: t("billing.op.sub.endNow"), danger: true }))
                        act.mutate({ s, what: "end" });
                    }}
                  >
                    {t("billing.op.sub.endNow")}
                  </MenuItem>
                )}
                {s.refund_due && <MenuItem onSelect={() => act.mutate({ s, what: "refunded" })}>{t("billing.op.sub.markRefunded")}</MenuItem>}
              </MenuContent>
            </Menu>
          );
        },
      },
    ],
    [t, locale, names, method, allowance, act, confirm],
  );
  return (
    <Panel
      title={t("billing.op.sub.title")}
      className="mt-5"
      bodyClassName="p-0"
      aside={
        <Segmented
          size="sm"
          aria-label={t("billing.op.sub.filter")}
          value={filter}
          onChange={(v) => set({ subs: v === "all" ? undefined : v })}
          options={SUB_FILTERS.map((f) => ({ value: f, label: t(`billing.op.sub.filter.${f}`) }))}
        />
      }
    >
      {q.isError ? (
        <div className="p-4">
          <ErrorState error={q.error} onRetry={() => void q.refetch()} />
        </div>
      ) : (
        <DataTable
          columns={columns}
          data={q.data?.items}
          getRowId={(s) => s.id}
          maxHeight="none"
          className="rounded-none border-0"
          empty={<EmptyState compact title={t("billing.op.sub.empty")} hint={plans.length ? undefined : t("billing.op.sub.emptyNoPlans")} />}
        />
      )}
      {dialog}
    </Panel>
  );
}
