import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import type { ColumnDef } from "@tanstack/react-table";
import { Percent, Plus } from "lucide-react";
import { useMemo } from "react";
import { toast } from "sonner";
import { DataTable } from "@/components/data-table";
import { Drawer } from "@/components/drawer";
import { FormSection, HelpText, MoneyInput, SuffixInput, SwitchRow } from "@/components/form-kit";
import { EmptyState, ErrorState, PageHeader } from "@/components/page";
import { Pill, type Tone } from "@/components/status";
import { Button } from "@/components/ui/button";
import { Field } from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import { Segmented } from "@/components/ui/segmented";
import { useMoney } from "@/components/money";
import { ApiError, errorMessage } from "@/lib/api/errors";
import { formatDateTime } from "@/lib/format";
import { useDrawerParam } from "@/lib/use-url-search";
import { useI18n } from "@/i18n";
import { paymentApi, type Coupon, type CouponInput, type CouponState } from "../api";
import { SubjectCell, useNames } from "../shared";
import { OffBanner, fromLocalInput, toLocalInput, useWindowText } from "./parts";
import { useForm } from "@/lib/use-form";

const couponsQuery = { queryKey: ["billing", "coupons"], queryFn: () => paymentApi.coupons() };

const STATE_TONE: Record<CouponState, Tone> = { active: "ok", scheduled: "info", ended: "neutral", used_up: "neutral", disabled: "neutral" };

/** "20% off, at most $5" / "$5 off". */
function useDiscountText() {
  const { t } = useI18n();
  const money = useMoney();
  return (c: Pick<Coupon, "percent" | "amount" | "max_discount">) =>
    c.percent > 0
      ? c.max_discount.micro > 0
        ? t("billing.op.cp.percentCapped", { pct: c.percent, max: money(c.max_discount) })
        : t("billing.op.cp.percentOff", { pct: c.percent })
      : t("billing.op.cp.amountOff", { amount: money(c.amount) });
}

/** Coupons members enter at top-up: a percent or an amount off, a time window and use limits. */
export function CouponsPage() {
  const { t } = useI18n();
  const money = useMoney();
  const windowText = useWindowText();
  const discount = useDiscountText();
  const drawer = useDrawerParam();
  const q = useQuery(couponsQuery);
  const items = q.data?.items;
  const columns = useMemo<ColumnDef<Coupon>[]>(
    () => [
      {
        id: "code",
        header: t("billing.op.cp.code"),
        meta: { className: "w-[160px]" },
        cell: ({ row }) => (
          <span className="grid min-w-0">
            <span className="font-mono text-sm font-medium" data-raw>
              {row.original.code}
            </span>
            {row.original.note && <span className="truncate text-xs text-ink-3">{row.original.note}</span>}
          </span>
        ),
      },
      { id: "discount", header: t("billing.op.cp.discount"), cell: ({ row }) => discount(row.original) },
      {
        id: "min",
        header: t("billing.op.cp.minTopup"),
        meta: { numeric: true, className: "w-[110px] max-lg:hidden" },
        cell: ({ row }) => (row.original.min_topup.micro > 0 ? money(row.original.min_topup) : <span className="text-ink-3">—</span>),
      },
      {
        id: "used",
        header: t("billing.op.cp.used"),
        meta: { numeric: true, className: "w-[130px]" },
        cell: ({ row }) => (
          <span className="grid justify-items-end">
            <span>{row.original.max_uses > 0 ? `${row.original.used} / ${row.original.max_uses}` : t("billing.op.cp.usedNoLimit", { n: row.original.used })}</span>
            {row.original.pending > 0 && <span className="text-xs text-ink-3">{t("billing.op.cp.pending", { n: row.original.pending })}</span>}
          </span>
        ),
      },
      {
        id: "perUser",
        header: t("billing.op.cp.perUser"),
        meta: { numeric: true, className: "w-[100px] max-md:hidden" },
        cell: ({ row }) => (row.original.per_user > 0 ? t("billing.op.cp.times", { n: row.original.per_user }) : t("billing.op.cp.noLimit")),
      },
      {
        id: "window",
        header: t("billing.op.cp.window"),
        meta: { className: "w-[220px] max-xl:hidden" },
        cell: ({ row }) => <span className="text-xs text-ink-2">{windowText(row.original.starts_at, row.original.ends_at)}</span>,
      },
      {
        id: "state",
        header: t("billing.op.cp.state"),
        meta: { className: "w-[110px]" },
        cell: ({ row }) => (
          <Pill tone={STATE_TONE[row.original.state]} dot>
            {t(`billing.op.cp.stateName.${row.original.state}`)}
          </Pill>
        ),
      },
    ],
    [t, money, windowText, discount],
  );
  const add = (
    <Button variant="primary" onClick={() => drawer.open("new")}>
      <Plus aria-hidden />
      {t("billing.op.cp.add")}
    </Button>
  );
  return (
    <>
      <PageHeader title={t("nav.billing.coupons")} description={t("billing.op.cp.description")} actions={add} />
      {q.data && !q.data.enabled && <OffBanner title={t("billing.op.cp.offTitle")} hint={t("billing.op.cp.offHint")} />}
      {q.isError ? (
        <ErrorState error={q.error} onRetry={() => void q.refetch()} />
      ) : (
        <DataTable
          columns={columns}
          data={items}
          getRowId={(c) => c.id}
          onRowClick={(c) => drawer.open(c.id)}
          selectedId={drawer.id}
          empty={<EmptyState icon={Percent} title={t("billing.op.cp.empty")} hint={t("billing.op.cp.emptyHint")} action={add} />}
        />
      )}
      {drawer.id && (drawer.id === "new" || items) && (
        <CouponDrawer
          key={drawer.id}
          coupon={drawer.id === "new" ? undefined : items?.find((c) => c.id === drawer.id)}
          currency={q.data?.currency ?? "USD"}
          onClose={drawer.close}
          onSaved={(c) => drawer.open(c.id)}
        />
      )}
    </>
  );
}

interface Form {
  code: string;
  type: "percent" | "fixed";
  percent: string;
  maxDiscount: number | null;
  amount: number | null;
  minTopup: number | null;
  maxUses: string;
  perUser: string;
  starts: string;
  ends: string;
  disabled: boolean;
  note: string;
}

function initial(c?: Coupon): Form {
  return {
    code: c?.code ?? "",
    type: c && c.percent === 0 ? "fixed" : "percent",
    percent: c && c.percent > 0 ? String(c.percent) : "10",
    maxDiscount: c && c.max_discount_micro > 0 ? c.max_discount_micro / 1e6 : null,
    amount: c && c.amount_micro > 0 ? c.amount_micro / 1e6 : null,
    minTopup: c && c.min_topup_micro > 0 ? c.min_topup_micro / 1e6 : null,
    maxUses: c ? String(c.max_uses) : "0",
    perUser: c ? String(c.per_user) : "1",
    starts: toLocalInput(c?.starts_at),
    ends: toLocalInput(c?.ends_at),
    disabled: c?.disabled ?? false,
    note: c?.note ?? "",
  };
}

const micro = (v: number | null) => Math.round((v ?? 0) * 1_000_000);
const whole = (s: string) => (/^\d+$/.test(s.trim()) ? Number(s) : NaN);

function CouponDrawer({ coupon, currency, onClose, onSaved }: { coupon?: Coupon; currency: string; onClose: () => void; onSaved: (c: Coupon) => void }) {
  const { t } = useI18n();
  const qc = useQueryClient();
  const form = useForm(initial(coupon));
  const f = form.values;
  const pct = whole(f.percent);
  const errs = {
    code: !coupon && !/^\S{1,64}$/.test(f.code.trim()) ? t("billing.op.cp.codeBad") : undefined,
    percent: f.type === "percent" && !(pct >= 1 && pct <= 100) ? t("billing.op.cp.percentBad") : undefined,
    amount: f.type === "fixed" && !(f.amount && f.amount > 0) ? t("billing.op.cp.amountBad") : undefined,
    maxUses: Number.isNaN(whole(f.maxUses)) ? t("billing.op.cp.wholeBad") : undefined,
    perUser: Number.isNaN(whole(f.perUser)) ? t("billing.op.cp.wholeBad") : undefined,
    window: f.starts && f.ends && new Date(f.ends) <= new Date(f.starts) ? t("billing.err.announcement_window") : undefined,
  };
  const bad = Object.values(errs).some(Boolean);
  const save = useMutation({
    mutationFn: () => {
      const body: CouponInput = {
        percent: f.type === "percent" ? pct : 0,
        amount_micro: f.type === "fixed" ? micro(f.amount) : 0,
        max_discount_micro: f.type === "percent" ? micro(f.maxDiscount) : 0,
        min_topup_micro: micro(f.minTopup),
        max_uses: whole(f.maxUses),
        per_user: whole(f.perUser),
        starts_at: fromLocalInput(f.starts),
        ends_at: fromLocalInput(f.ends),
        disabled: f.disabled,
        note: f.note.trim() || undefined,
      };
      return coupon ? paymentApi.updateCoupon(coupon.id, body) : paymentApi.createCoupon({ ...body, code: f.code.trim() });
    },
    onSuccess: (c) => {
      toast.success(t("billing.op.cp.saved", { code: c.code }));
      void qc.invalidateQueries({ queryKey: couponsQuery.queryKey });
      onSaved(c);
    },
    onError: (e) => toast.error(e instanceof ApiError && e.code === "conflict" ? t("billing.op.cp.codeTaken") : errorMessage(e)),
  });
  return (
    <Drawer
      open
      onOpenChange={(o) => !o && onClose()}
      title={coupon ? coupon.code : t("billing.op.cp.add")}
      subtitle={coupon ? t("billing.op.cp.editHint") : t("billing.op.cp.addHint")}
      size="lg"
      footer={
        <>
          <Button onClick={onClose}>{t("common.cancel")}</Button>
          <Button variant="primary" disabled={bad || save.isPending} onClick={() => save.mutate()}>
            {save.isPending ? t("common.saving") : t("common.save")}
          </Button>
        </>
      }
    >
      <div className="p-5">
        <FormSection title={t("billing.op.cp.secDiscount")}>
          {!coupon && (
            <Field label={t("billing.op.cp.code")} hint={t("billing.op.cp.codeHint")} error={f.code && errs.code}>
              <Input value={f.code} onChange={(e) => form.set("code", e.target.value.toUpperCase())} className="font-mono" placeholder="SPRING20" maxLength={64} />
            </Field>
          )}
          <Segmented
            aria-label={t("billing.op.cp.type")}
            value={f.type}
            onChange={(v) => form.set("type", v)}
            options={[
              { value: "percent", label: t("billing.op.cp.typePercent") },
              { value: "fixed", label: t("billing.op.cp.typeFixed") },
            ]}
          />
          {f.type === "percent" ? (
            <div className="grid grid-cols-2 gap-4">
              <Field label={t("billing.op.cp.percent")} error={errs.percent}>
                <SuffixInput suffix="%" inputMode="numeric" value={f.percent} onChange={(e) => form.set("percent", e.target.value)} />
              </Field>
              <Field label={t("billing.op.cp.maxDiscount")} optional hint={t("billing.op.cp.maxDiscountHint")}>
                <MoneyInput value={f.maxDiscount} onChange={(v) => form.set("maxDiscount", v)} currency={currency} />
              </Field>
            </div>
          ) : (
            <Field label={t("billing.op.cp.amount")} error={errs.amount}>
              <MoneyInput value={f.amount} onChange={(v) => form.set("amount", v)} currency={currency} placeholder="5.00" />
            </Field>
          )}
          <Field label={t("billing.op.cp.minTopup")} optional hint={t("billing.op.cp.minTopupHint")}>
            <MoneyInput value={f.minTopup} onChange={(v) => form.set("minTopup", v)} currency={currency} />
          </Field>
        </FormSection>
        <FormSection title={t("billing.op.cp.secLimits")}>
          <div className="grid grid-cols-2 gap-4">
            <Field label={t("billing.op.cp.maxUses")} hint={t("billing.op.cp.zeroNoLimit")} error={errs.maxUses}>
              <Input inputMode="numeric" value={f.maxUses} onChange={(e) => form.set("maxUses", e.target.value)} />
            </Field>
            <Field label={t("billing.op.cp.perUser")} hint={t("billing.op.cp.zeroNoLimit")} error={errs.perUser}>
              <Input inputMode="numeric" value={f.perUser} onChange={(e) => form.set("perUser", e.target.value)} />
            </Field>
            <Field label={t("billing.op.cp.starts")} optional>
              <Input type="datetime-local" value={f.starts} onChange={(e) => form.set("starts", e.target.value)} />
            </Field>
            <Field label={t("billing.op.cp.ends")} optional error={errs.window}>
              <Input type="datetime-local" value={f.ends} onChange={(e) => form.set("ends", e.target.value)} />
            </Field>
          </div>
          <HelpText>{t("billing.op.cp.holdHelp")}</HelpText>
          <SwitchRow label={t("billing.op.cp.disabled")} description={t("billing.op.cp.disabledHint")} checked={f.disabled} onChange={(v) => form.set("disabled", v)} />
          <Field label={t("billing.op.cp.note")} optional hint={t("billing.op.cp.noteHint")}>
            <Input value={f.note} onChange={(e) => form.set("note", e.target.value)} maxLength={200} />
          </Field>
        </FormSection>
        {coupon && <CouponUses coupon={coupon} />}
      </div>
    </Drawer>
  );
}

function CouponUses({ coupon }: { coupon: Coupon }) {
  const { t, locale } = useI18n();
  const money = useMoney();
  const names = useNames();
  const q = useQuery({ queryKey: ["billing", "coupon-uses", coupon.id], queryFn: () => paymentApi.couponUses(coupon.id) });
  return (
    <FormSection title={t("billing.op.cp.uses")} description={t("billing.op.cp.usesHint")}>
      {q.isError ? (
        <ErrorState error={q.error} onRetry={() => void q.refetch()} />
      ) : !q.data ? (
        <p className="text-sm text-ink-3">{t("common.loading")}</p>
      ) : q.data.items.length === 0 ? (
        <p className="text-sm text-ink-3">{t("billing.op.cp.noUses")}</p>
      ) : (
        <ul className="divide-y divide-line-soft rounded-lg border border-line">
          {q.data.items.map((u) => (
            <li key={u.order_id} className="flex flex-wrap items-center gap-x-4 gap-y-1 px-3 py-2">
              <SubjectCell name={names.user(u.user_id)} className="min-w-40 flex-1" />
              <span className="text-xs text-ink-3">{formatDateTime(u.created_at, locale)}</span>
              <span className="tabular-nums text-sm">{t("billing.op.ord.discountShort", { amount: money(u.discount) })}</span>
              <Pill tone={u.status === "paid" ? "ok" : "warn"} dot>
                {t(`billing.order.${u.status}`)}
              </Pill>
              <Link to="/billing/$page" params={{ page: "orders" }} search={{ id: u.order_id }} className="text-xs text-accent hover:underline">
                {t("billing.op.cp.viewOrder")}
              </Link>
            </li>
          ))}
        </ul>
      )}
    </FormSection>
  );
}
