import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useNavigate } from "@tanstack/react-router";
import { CalendarClock, Loader2 } from "lucide-react";
import { useEffect, useState } from "react";
import { toast } from "sonner";
import { useConfirm } from "@/components/confirm";
import { HelpText } from "@/components/form-kit";
import { EmptyState, ErrorState, LoadingRows, PageHeader, Panel } from "@/components/page";
import { Pill, type Tone } from "@/components/status";
import { Button } from "@/components/ui/button";
import { Segmented } from "@/components/ui/segmented";
import { errorMessage } from "@/lib/api/errors";
import { cn } from "@/lib/cn";
import { formatMoney } from "@/lib/money";
import { useI18n } from "@/i18n";
import { plansApi, type Plan, type SubView, type TxnKind } from "../api";
import { goToCheckout, NoKeysState, PayerPicker, PaymentReturnBanner, TxnTable, type ReturnState, methodKey, useAllowanceText, useDefaultPayer, usePayMethodLabel, useSearchParam } from "../shared";

const myPlanKey = ["billing", "my-plan"];
const plansQuery = { queryKey: ["billing", "plans"], queryFn: () => plansApi.plans() };
const subConfigQuery = { queryKey: ["billing", "plan-config"], queryFn: () => plansApi.config() };

const POLL_LIMIT_MS = 60_000;

function formatDate(iso: string, locale: string): string {
  return new Date(iso).toLocaleDateString(locale, { year: "numeric", month: "short", day: "numeric" });
}

const STATUS_TONE: Record<SubView["status"], Tone> = { pending: "warn", active: "ok", ended: "neutral" };

/** A member's plan: the current one with its allowance, the plans on offer, and the allowance history. */
export function MyPlanPage() {
  const { t } = useI18n();
  const me = useQuery({ queryKey: myPlanKey, queryFn: () => plansApi.me() });
  const sub = me.data?.subscription ?? null;
  return (
    <>
      <PageHeader title={t("nav.billing.plan")} description={t("billing.my.planDescription")} />
      <PlanReturn />
      {me.isError ? (
        <ErrorState error={me.error} onRetry={() => void me.refetch()} />
      ) : me.isPending ? (
        <LoadingRows rows={3} />
      ) : (
        <>
          <CurrentPlan sub={sub} plan={me.data.plan} />
          <PlanList holding={!!sub && sub.status !== "ended"} />
          {sub && (
            <Panel title={t("billing.my.allowanceHistory")} className="mt-5" bodyClassName="p-0">
              <TxnTable
                queryKey={["billing", "my-plan-txns", sub.id]}
                tokens={sub.allowance_unit === "tokens"}
                load={async (q: { cursor?: string; kind?: TxnKind }) => {
                  const page = await plansApi.myTxns(q.cursor);
                  return q.kind ? { ...page, items: page.items.filter((x) => x.kind === q.kind) } : page;
                }}
              />
            </Panel>
          )}
        </>
      )}
    </>
  );
}

/** Back from the provider: `?subscription=` waits up to a minute for the payment, `?canceled=1` says nothing was charged. */
function PlanReturn() {
  const { t } = useI18n();
  const id = useSearchParam("subscription");
  const canceled = useSearchParam("canceled") === "1";
  const navigate = useNavigate();
  const [since] = useState(() => Date.now());
  const [now, setNow] = useState(since);
  const me = useQuery({
    queryKey: myPlanKey,
    queryFn: () => plansApi.me(),
    enabled: !!id && !canceled,
    refetchInterval: (q) => {
      const s = q.state.data?.subscription;
      return s && s.id === id && s.status === "active" ? false : Date.now() - since > POLL_LIMIT_MS ? false : 2000;
    },
  });
  useEffect(() => {
    if (!id || canceled) return;
    const timer = setInterval(() => setNow(Date.now()), 2000);
    return () => clearInterval(timer);
  }, [id, canceled]);
  if (!id && !canceled) return null;
  const s = me.data?.subscription;
  const active = !!id && s?.id === id && s.status === "active";
  const late = !active && now - since > POLL_LIMIT_MS;
  const state: ReturnState = active ? "done" : canceled ? "canceled" : late ? "late" : "waiting";
  const text = canceled ? t("billing.my.canceled") : active ? t("billing.my.planActive") : late ? t("billing.my.planLate") : t("billing.my.waiting");
  return <PaymentReturnBanner state={state} text={text} onClose={() => void navigate({ to: "/billing/$page", params: { page: "plan" }, replace: true })} />;
}

function CurrentPlan({ sub, plan }: { sub: SubView | null; plan?: Plan }) {
  const { t, locale } = useI18n();
  const qc = useQueryClient();
  const confirm = useConfirm();
  const allowance = useAllowanceText();
  const cancel = useMutation({
    mutationFn: (c: boolean) => plansApi.cancelMine(c),
    onSuccess: (_, c) => {
      toast.success(c ? t("billing.my.planCanceled") : t("billing.my.planResumed"));
      void qc.invalidateQueries({ queryKey: myPlanKey });
    },
    onError: (e) => toast.error(errorMessage(e)),
  });
  if (!sub)
    return (
      <div className="rounded-lg border border-line bg-surface">
        <EmptyState icon={CalendarClock} compact title={t("billing.my.noPlan")} hint={t("billing.my.noPlanHint")} />
      </div>
    );
  const unit = sub.allowance_unit;
  const ratio = sub.allowance > 0 ? Math.min(sub.used / sub.allowance, 1) : 0;
  const usedUp = sub.current && sub.remaining <= 0;
  return (
    <Panel
      title={
        <span className="flex items-center gap-2">
          {sub.plan_name}
          <Pill tone={STATUS_TONE[sub.status]} dot>
            {t(`billing.my.status.${sub.status}`)}
          </Pill>
          {sub.cancel_at_period_end && sub.status === "active" && <Pill tone="warn">{t("billing.my.endsAtPeriodEnd")}</Pill>}
        </span>
      }
      aside={
        sub.status === "active" &&
        (sub.cancel_at_period_end ? (
          <Button size="sm" onClick={() => cancel.mutate(false)} disabled={cancel.isPending}>
            {t("billing.my.resume")}
          </Button>
        ) : (
          <Button
            size="sm"
            variant="danger"
            disabled={cancel.isPending}
            onClick={async () => {
              const ok = await confirm({
                title: t("billing.my.cancelTitle"),
                body: sub.period_end ? t("billing.my.cancelBody", { date: formatDate(sub.period_end, locale) }) : t("billing.my.cancelBodyPlain"),
                confirm: t("billing.my.cancelPlan"),
                danger: true,
              });
              if (ok) cancel.mutate(true);
            }}
          >
            {t("billing.my.cancelPlan")}
          </Button>
        ))
      }
    >
      {plan?.description && <p className="mb-3 text-sm text-ink-2">{plan.description}</p>}
      <div className="flex flex-wrap items-baseline justify-between gap-x-4 gap-y-1 text-sm">
        <span>
          {t("billing.my.usedOf", { used: allowance(sub.used, unit, sub.currency), total: allowance(sub.allowance, unit, sub.currency) })}
        </span>
        <span className={cn("font-medium tabular-nums", usedUp && "text-bad")}>
          {t("billing.my.left", { left: allowance(sub.remaining, unit, sub.currency) })}
        </span>
      </div>
      <div
        className="mt-2 h-2 overflow-hidden rounded-full bg-surface-2"
        role="progressbar"
        aria-valuenow={Math.round(ratio * 100)}
        aria-valuemin={0}
        aria-valuemax={100}
        aria-label={t("billing.my.allowanceUsed")}
      >
        <div className={cn("h-full rounded-full", usedUp ? "bg-bad" : ratio >= 0.8 ? "bg-warn" : "bg-accent")} style={{ width: `${ratio * 100}%` }} />
      </div>
      <dl className="mt-4 grid gap-x-6 gap-y-2 text-sm sm:grid-cols-2">
        {sub.period_end && (
          <div>
            <dt className="text-xs text-ink-3">{sub.cancel_at_period_end || sub.status === "ended" ? t("billing.my.endsOn") : t("billing.my.renewsOn")}</dt>
            <dd>{formatDate(sub.period_end, locale)}</dd>
          </div>
        )}
        <div>
          <dt className="text-xs text-ink-3">{t("billing.my.whenUsedUp")}</dt>
          <dd>{t(`billing.my.overage.${sub.overage}`)}</dd>
        </div>
        <div>
          <dt className="text-xs text-ink-3">{t("billing.my.price")}</dt>
          <dd className="tabular-nums">
            {t("billing.my.perPeriod", { price: formatMoney({ micro: sub.price_micro, currency: sub.currency }, locale), period: t(`billing.my.period.${sub.period}`) })}
          </dd>
        </div>
        <div>
          <dt className="text-xs text-ink-3">{t("billing.my.paidWith")}</dt>
          <dd>{sub.renewal === "provider" ? t("billing.my.renewProvider") : t("billing.my.renewBalance")}</dd>
        </div>
      </dl>
    </Panel>
  );
}

function PlanList({ holding }: { holding: boolean }) {
  const { t, locale } = useI18n();
  const qc = useQueryClient();
  const allowance = useAllowanceText();
  const methodLabel = usePayMethodLabel();
  const plans = useQuery(plansQuery);
  const cfg = useQuery(subConfigQuery);
  const payer = useDefaultPayer();
  const [picked, setPicked] = useState<string>();
  const keyId = picked ?? payer.keyId;
  const methods = cfg.data?.methods ?? [];
  const [method, setMethod] = useState<string>();
  const chosen = methods.find((m) => methodKey(m) === method) ?? methods[0];
  const subscribe = useMutation({
    mutationFn: (planId: string) =>
      plansApi.subscribe({ plan_id: planId, provider: chosen!.provider, method: chosen!.method || undefined, key_id: keyId || undefined }),
    onSuccess: (r) => {
      if (r.url) goToCheckout(r.url);
      else {
        toast.success(t("billing.my.planActive"));
        void qc.invalidateQueries({ queryKey: myPlanKey });
        void qc.invalidateQueries({ queryKey: ["billing", "me"] });
        void qc.invalidateQueries({ queryKey: ["billing", "my-txns"] });
      }
    },
  });
  const items = plans.data?.items ?? [];
  return (
    <Panel title={t("billing.my.plans")} className="mt-5" aside={<PayerPicker value={keyId} onChange={setPicked} />}>
      {plans.isError ? (
        <ErrorState error={plans.error} onRetry={() => void plans.refetch()} />
      ) : plans.isPending || cfg.isPending ? (
        <LoadingRows rows={2} />
      ) : items.length === 0 ? (
        <EmptyState icon={CalendarClock} compact title={t("billing.my.noPlans")} />
      ) : payer.noKeys ? (
        <NoKeysState compact />
      ) : (
        <div className="grid gap-4">
          {methods.length > 1 && (
            <div className="flex flex-wrap items-center gap-3 text-sm">
              <span className="text-ink-2">{t("billing.my.method")}</span>
              <Segmented
                value={chosen ? methodKey(chosen) : ""}
                onChange={setMethod}
                aria-label={t("billing.my.method")}
                options={methods.map((m) => ({ value: methodKey(m), label: methodLabel(m) }))}
              />
            </div>
          )}
          {holding && <HelpText>{t("billing.my.switchNote")}</HelpText>}
          {subscribe.isError && (
            <p role="alert" className="rounded-md bg-bad-soft px-3 py-2 text-sm text-bad">
              {errorMessage(subscribe.error)}
            </p>
          )}
          <ul className="grid gap-3 sm:grid-cols-2 xl:grid-cols-3">
            {items.map((p) => (
              <li key={p.id} className="flex flex-col rounded-lg border border-line bg-surface-2/40 p-4">
                <p className="text-base font-semibold">{p.name}</p>
                {p.description && <p className="mt-0.5 text-sm text-ink-2">{p.description}</p>}
                <p className="mt-3 text-xl font-semibold tabular-nums">
                  {t("billing.my.perPeriod", { price: formatMoney({ micro: p.price_micro, currency: p.currency }, locale), period: t(`billing.my.period.${p.period}`) })}
                </p>
                <ul className="mt-2 grid gap-1 text-sm text-ink-2">
                  <li>{t("billing.my.allowancePer", { amount: allowance(p.allowance, p.allowance_unit, p.currency) })}</li>
                  <li>{t(`billing.my.overage.${p.overage}`)}</li>
                </ul>
                <div className="mt-auto pt-4">
                  <Button
                    variant="primary"
                    className="w-full"
                    disabled={holding || !chosen || subscribe.isPending}
                    onClick={() => subscribe.mutate(p.id)}
                  >
                    {subscribe.isPending && subscribe.variables === p.id && <Loader2 aria-hidden className="animate-spin" />}
                    {t("billing.my.subscribe")}
                  </Button>
                </div>
              </li>
            ))}
          </ul>
          {!chosen && <HelpText>{t("billing.my.noProviderHint")}</HelpText>}
        </div>
      )}
    </Panel>
  );
}
