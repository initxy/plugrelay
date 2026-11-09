import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useNavigate } from "@tanstack/react-router";
import { CalendarCheck, CheckCircle2, CreditCard, Gift, Loader2, Ticket, Users } from "lucide-react";
import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { toast } from "sonner";
import { CopyButton } from "@/components/copy-button";
import { HelpText, MoneyInput } from "@/components/form-kit";
import { Money } from "@/components/money";
import { EmptyState, ErrorState, LoadingRows, PageHeader, Panel } from "@/components/page";
import { StatTile } from "@/components/stat-tile";
import { Pill } from "@/components/status";
import { Button } from "@/components/ui/button";
import { Field } from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import { Segmented } from "@/components/ui/segmented";
import { ApiError, errorMessage } from "@/lib/api/errors";
import type { Money as MoneyValue } from "@/lib/api/types";
import { cn } from "@/lib/cn";
import { formatDateTime, formatNumber } from "@/lib/format";
import { formatMoney } from "@/lib/money";
import { useI18n } from "@/i18n";
import { balanceApi, paymentApi, redeemApi, rewardsApi, type PaymentConfig, type Rewards } from "../api";
import { goToCheckout, NoKeysState, PayerPicker, PaymentReturnBanner, TxnTable, type ReturnState, methodKey, useDefaultPayer, usePayMethodLabel, useSearchParam } from "../shared";

const configQuery = { queryKey: ["billing", "payment-config"], queryFn: () => paymentApi.config() };
const rewardsQuery = { queryKey: ["billing", "rewards"], queryFn: () => rewardsApi.me() };

/** Refreshes everything a balance change shows. */
function useRefreshBalance() {
  const qc = useQueryClient();
  return useCallback(() => {
    void qc.invalidateQueries({ queryKey: ["billing", "me"] });
    void qc.invalidateQueries({ queryKey: ["billing", "my-txns"] });
    void qc.invalidateQueries({ queryKey: ["billing", "rewards"] });
  }, [qc]);
}

/** A member's wallet: balance, top-up, redeem codes, daily check-in, invite link and the history of every change. */
export function WalletPage() {
  const { t } = useI18n();
  const payer = useDefaultPayer();
  const [picked, setPicked] = useState<string>();
  const keyId = picked ?? payer.keyId;
  const rewards = useQuery(rewardsQuery);

  const header = (
    <PageHeader
      title={t("nav.billing.wallet")}
      description={t("billing.my.walletDescription")}
      actions={
        <>
          <PayerPicker value={keyId} onChange={setPicked} />
          {rewards.data?.checkin.enabled && !payer.noKeys && <CheckInButton rewards={rewards.data} keyId={keyId} />}
        </>
      }
    />
  );
  if (payer.loading) return <>{header}<LoadingRows rows={4} /></>;
  if (payer.noKeys)
    return (
      <>
        {header}
        <div className="rounded-lg border border-line bg-surface">
          <NoKeysState />
        </div>
      </>
    );
  return (
    <>
      {header}
      <PaymentReturn />
      <BalanceTiles keyId={keyId} />
      <div className="mt-5 grid gap-5 lg:grid-cols-[minmax(0,3fr)_minmax(0,2fr)]">
        <TopupPanel keyId={keyId} />
        <div className="grid content-start gap-5">
          <RedeemPanel keyId={keyId} />
          {rewards.data?.referral.enabled && <InvitePanel rewards={rewards.data} />}
        </div>
      </div>
      <Panel title={t("billing.my.history")} aside={t("billing.my.historyHint")} className="mt-5" bodyClassName="p-0">
        <TxnTable queryKey={["billing", "my-txns", keyId]} load={(q) => balanceApi.myTxns({ ...q, key_id: keyId || undefined })} />
      </Panel>
    </>
  );
}

// ---------------------------------------------------------------- balance

function BalanceTiles({ keyId }: { keyId: string }) {
  const { t } = useI18n();
  const q = useQuery({ queryKey: ["billing", "me", keyId], queryFn: () => balanceApi.me(keyId || undefined) });
  if (q.isError) return <ErrorState error={q.error} onRetry={() => void q.refetch()} />;
  const a = q.data;
  const credit = a && a.credit_limit.micro > 0;
  return (
    <div className={cn("grid gap-3 sm:grid-cols-2", credit ? "lg:grid-cols-3" : "lg:grid-cols-[2fr_1fr]")}>
      <StatTile
        label={t("billing.my.available")}
        value={a ? <Money value={a.available} className={cn(a.available.micro < 0 && "text-bad")} /> : null}
        note={a && a.available.micro <= 0 ? t("billing.my.emptyBalance") : t("billing.my.availableNote")}
        loading={q.isPending}
      />
      <StatTile
        label={t("billing.my.held")}
        value={a ? <Money value={a.held} /> : null}
        note={t("billing.my.heldNote")}
        loading={q.isPending}
      />
      {credit && <StatTile label={t("billing.my.creditLimit")} value={<Money value={a.credit_limit} />} note={t("billing.my.creditLimitNote")} />}
    </div>
  );
}

// ---------------------------------------------------------------- check-in

function CheckInButton({ rewards, keyId }: { rewards: Rewards; keyId: string }) {
  const { t, locale } = useI18n();
  const refresh = useRefreshBalance();
  const c = rewards.checkin;
  const checkIn = useMutation({
    mutationFn: () => rewardsApi.checkIn(keyId || undefined),
    onSuccess: (r) => {
      toast.success(t("billing.my.checkedInToast", { amount: formatMoney(r.amount, locale) }));
      refresh();
    },
    onError: (e) => {
      // Checked in on another tab or device: just show it.
      if (e instanceof ApiError && e.code === "checked_in") refresh();
      else toast.error(errorMessage(e));
    },
  });
  if (c.done_today)
    return (
      <Button disabled>
        <CheckCircle2 aria-hidden className="text-ok" />
        {c.today_amount ? t("billing.my.checkedIn", { amount: formatMoney(c.today_amount, locale) }) : t("billing.my.checkedInPlain")}
        {c.streak > 1 && <span className="text-xs text-ink-3">· {t("billing.my.streak", { n: c.streak })}</span>}
      </Button>
    );
  const range =
    c.min.micro === c.max.micro
      ? formatMoney(c.min, locale)
      : t("billing.my.range", { min: formatMoney(c.min, locale), max: formatMoney(c.max, locale) });
  return (
    <Button variant="primary" onClick={() => checkIn.mutate()} disabled={checkIn.isPending} title={t("billing.my.checkInHint", { range })}>
      {checkIn.isPending ? <Loader2 aria-hidden className="animate-spin" /> : <CalendarCheck aria-hidden />}
      {t("billing.my.checkIn")}
      {c.streak > 0 && <span className="text-xs opacity-80">· {t("billing.my.streak", { n: c.streak })}</span>}
    </Button>
  );
}

// ---------------------------------------------------------------- top-up

function TopupPanel({ keyId }: { keyId: string }) {
  const { t } = useI18n();
  const q = useQuery(configQuery);
  return (
    <Panel title={t("billing.my.topup")}>
      {q.isError ? (
        <ErrorState error={q.error} onRetry={() => void q.refetch()} />
      ) : q.isPending ? (
        <LoadingRows rows={3} />
      ) : q.data.methods.length === 0 ? (
        <EmptyState icon={CreditCard} compact title={t("billing.my.noProvider")} hint={t("billing.my.noProviderHint")} />
      ) : (
        <TopupForm cfg={q.data} keyId={keyId} />
      )}
    </Panel>
  );
}

function TopupForm({ cfg, keyId }: { cfg: PaymentConfig; keyId: string }) {
  const { t, locale } = useI18n();
  const methodLabel = usePayMethodLabel();
  const cur = cfg.currency;
  const m = (micro: number): MoneyValue => ({ micro, currency: cur });
  const [amount, setAmount] = useState<number | null>(cfg.presets_micro[0] ? cfg.presets_micro[0] / 1e6 : null);
  const [method, setMethod] = useState(methodKey(cfg.methods[0]!));
  const [coupon, setCoupon] = useState("");
  const [checked, setChecked] = useState<{ code: string; amount: number; discount: MoneyValue; pay: MoneyValue }>();
  const micro = amount === null ? null : Math.round(amount * 1e6);
  const inRange = micro !== null && micro >= cfg.min_micro && micro <= cfg.max_micro;
  const applied = checked && checked.code === coupon.trim().toUpperCase() && checked.amount === micro ? checked : undefined;

  const check = useMutation({
    mutationFn: () => paymentApi.checkCoupon({ code: coupon.trim(), amount_micro: micro ?? 0 }),
    onSuccess: (r) => setChecked({ code: coupon.trim().toUpperCase(), amount: micro ?? 0, discount: r.discount, pay: r.pay }),
    onError: () => setChecked(undefined),
  });
  const pay = useMutation({
    mutationFn: () => {
      const chosen = cfg.methods.find((x) => methodKey(x) === method) ?? cfg.methods[0]!;
      return paymentApi.checkout({
        amount_micro: micro ?? 0,
        provider: chosen.provider,
        method: chosen.method || undefined,
        key_id: keyId || undefined,
        coupon: coupon.trim() || undefined,
      });
    },
    onSuccess: (r) => goToCheckout(r.url),
  });
  const total = applied ? applied.pay : micro !== null ? m(micro) : undefined;
  return (
    <form
      className="grid gap-4"
      onSubmit={(e) => {
        e.preventDefault();
        if (inRange) pay.mutate();
      }}
    >
      {cfg.presets_micro.length > 0 && (
        <div className="flex flex-wrap gap-2" role="group" aria-label={t("billing.my.presets")}>
          {cfg.presets_micro.map((p) => (
            <Button
              key={p}
              type="button"
              variant={micro === p ? "primary" : "default"}
              aria-pressed={micro === p}
              onClick={() => setAmount(p / 1e6)}
              className="min-w-20 tabular-nums"
            >
              {formatMoney(m(p), locale)}
            </Button>
          ))}
        </div>
      )}
      <Field
        label={t("billing.my.amount")}
        hint={t("billing.my.amountRange", { min: formatMoney(m(cfg.min_micro), locale), max: formatMoney(m(cfg.max_micro), locale) })}
        error={micro !== null && !inRange ? t("billing.my.amountOut") : undefined}
      >
        <MoneyInput value={amount} onChange={setAmount} currency={cur} className="max-w-56" />
      </Field>
      {cfg.methods.length > 1 && (
        <Field label={t("billing.my.method")}>
          <Segmented
            value={method}
            onChange={setMethod}
            aria-label={t("billing.my.method")}
            options={cfg.methods.map((x) => ({ value: methodKey(x), label: methodLabel(x) }))}
          />
        </Field>
      )}
      {cfg.coupons && (
        <div className="flex max-w-[28rem] items-start gap-2">
          <Field
            label={t("billing.my.coupon")}
            optional
            className="min-w-0 flex-1"
            error={check.isError ? errorMessage(check.error) : undefined}
            hint={
              applied
                ? t("billing.my.couponApplied", { discount: formatMoney(applied.discount, locale), pay: formatMoney(applied.pay, locale) })
                : t("billing.my.couponHint")
            }
          >
            <Input
              value={coupon}
              onChange={(e) => setCoupon(e.target.value)}
              placeholder={t("billing.my.couponPlaceholder")}
              autoComplete="off"
              className="font-mono uppercase"
            />
          </Field>
          <Button type="button" className="mt-[22px]" onClick={() => check.mutate()} disabled={!coupon.trim() || !inRange || check.isPending}>
            {t("billing.my.couponCheck")}
          </Button>
        </div>
      )}
      {pay.isError && (
        <p role="alert" className="rounded-md bg-bad-soft px-3 py-2 text-sm text-bad">
          {errorMessage(pay.error)}
        </p>
      )}
      <div className="flex flex-wrap items-center gap-3 border-t border-line-soft pt-4">
        <Button type="submit" variant="primary" disabled={!inRange || pay.isPending}>
          {pay.isPending ? <Loader2 aria-hidden className="animate-spin" /> : <CreditCard aria-hidden />}
          {total ? t("billing.my.pay", { amount: formatMoney(total, locale) }) : t("billing.my.payPlain")}
        </Button>
        <HelpText>
          {applied && micro !== null
            ? t("billing.my.payNoteCoupon", { amount: formatMoney(m(micro), locale) })
            : t("billing.my.payNote")}
        </HelpText>
      </div>
    </form>
  );
}

/** Back from the provider: `?order=` waits for the payment notice, `?canceled=1` cancels the order. */
function PaymentReturn() {
  const { t, locale } = useI18n();
  const order = useSearchParam("order");
  const canceled = useSearchParam("canceled") === "1";
  const navigate = useNavigate();
  const refresh = useRefreshBalance();
  const done = useRef(false);
  const q = useQuery({
    queryKey: ["billing", "order", order],
    queryFn: () => paymentApi.order(order!),
    enabled: !!order && !canceled,
    refetchInterval: (query) => (query.state.data?.status === "paid" ? false : 2000),
  });
  const cancel = useMutation({ mutationFn: (id: string) => paymentApi.cancel(id) });
  const cancelOrder = cancel.mutate;
  useEffect(() => {
    if (order && canceled && !done.current) {
      done.current = true;
      cancelOrder(order);
    }
  }, [order, canceled, cancelOrder]);
  const paid = q.data?.status === "paid";
  useEffect(() => {
    if (paid) refresh();
  }, [paid, refresh]);
  if (!order) return null;
  const state: ReturnState = canceled || q.data?.status === "canceled" ? "canceled" : paid ? "done" : "waiting";
  return (
    <PaymentReturnBanner
      state={state}
      text={state === "canceled" ? t("billing.my.canceled") : paid ? t("billing.my.paid", { amount: formatMoney(q.data!.amount, locale) }) : t("billing.my.waiting")}
      onClose={() => void navigate({ to: "/billing", replace: true })}
    />
  );
}

// ---------------------------------------------------------------- redeem

function RedeemPanel({ keyId }: { keyId: string }) {
  const { t, locale } = useI18n();
  const refresh = useRefreshBalance();
  const [code, setCode] = useState("");
  const redeem = useMutation({
    mutationFn: () => redeemApi.redeem({ code: code.trim(), key_id: keyId || undefined }),
    onSuccess: (r) => {
      toast.success(t("billing.my.redeemed", { amount: formatMoney(r.amount, locale), balance: formatMoney(r.balance, locale) }));
      setCode("");
      refresh();
    },
  });
  return (
    <Panel title={t("billing.my.redeem")}>
      <form
        className="grid gap-2"
        onSubmit={(e) => {
          e.preventDefault();
          if (code.trim()) redeem.mutate();
        }}
      >
        <div className="flex gap-2">
          <Input
            aria-label={t("billing.my.redeemCode")}
            value={code}
            onChange={(e) => setCode(e.target.value)}
            placeholder="RC-XXXX-XXXX-…"
            autoComplete="off"
            className="font-mono"
          />
          <Button type="submit" disabled={!code.trim() || redeem.isPending}>
            <Ticket aria-hidden />
            {t("billing.my.redeemAction")}
          </Button>
        </div>
        {redeem.isError ? (
          <p role="alert" className="text-xs text-bad">
            {errorMessage(redeem.error)}
          </p>
        ) : (
          <HelpText>{t("billing.my.redeemHint")}</HelpText>
        )}
      </form>
    </Panel>
  );
}

// ---------------------------------------------------------------- invite

function InvitePanel({ rewards }: { rewards: Rewards }) {
  const { t, locale } = useI18n();
  const r = rewards.referral;
  const link = `${location.origin}/signup?ref=${encodeURIComponent(r.code ?? "")}`;
  const list = useQuery({ queryKey: ["billing", "rewards", "referrals"], queryFn: () => rewardsApi.myReferrals() });
  const rule = useMemo(() => {
    const base =
      r.mode === "fixed" && r.fixed
        ? t("billing.my.inviteRuleFixed", { amount: formatMoney(r.fixed, locale) })
        : t("billing.my.inviteRulePercent", { percent: formatNumber(r.percent ?? 0, locale) });
    return r.cap && r.cap.micro > 0 ? `${base} ${t("billing.my.inviteCap", { cap: formatMoney(r.cap, locale) })}` : base;
  }, [r, t, locale]);
  const rows = list.data?.items ?? [];
  return (
    <Panel title={t("billing.my.invite")} aside={<Gift aria-hidden className="size-4" />}>
      <p className="text-sm text-ink-2">{rule}</p>
      <div className="mt-3 flex items-center gap-2">
        <code data-raw className="min-w-0 flex-1 truncate rounded-md border border-line bg-surface-2 px-2.5 py-1.5 font-mono text-xs">
          {link}
        </code>
        <CopyButton text={link} label={t("billing.my.copyLink")} />
      </div>
      <dl className="mt-4 grid grid-cols-3 gap-3 text-center">
        <div className="rounded-md bg-surface-2 px-2 py-2">
          <dt className="text-xs text-ink-3">{t("billing.my.invited")}</dt>
          <dd className="text-lg font-semibold tabular-nums">{r.invited ?? 0}</dd>
        </div>
        <div className="rounded-md bg-surface-2 px-2 py-2">
          <dt className="text-xs text-ink-3">{t("billing.my.rewarded")}</dt>
          <dd className="text-lg font-semibold tabular-nums">{r.rewarded ?? 0}</dd>
        </div>
        <div className="rounded-md bg-surface-2 px-2 py-2">
          <dt className="text-xs text-ink-3">{t("billing.my.earned")}</dt>
          <dd className="text-lg font-semibold tabular-nums">{r.earned ? <Money value={r.earned} /> : "—"}</dd>
        </div>
      </dl>
      {rows.length > 0 ? (
        <ul className="mt-4 max-h-56 divide-y divide-line-soft overflow-y-auto rounded-md border border-line-soft" aria-label={t("billing.my.invitees")}>
          {rows.map((x, i) => (
            <li key={i} className="flex items-center justify-between gap-3 px-3 py-2 text-sm">
              <span className="flex items-center gap-2 text-ink-2">
                <Users aria-hidden className="size-3.5 text-ink-3" />
                {t("billing.my.joined", { when: formatDateTime(x.created_at, locale) })}
              </span>
              {x.rewarded_at ? (
                x.reward.micro > 0 ? (
                  <span className="tabular-nums text-ok">+{formatMoney(x.reward, locale)}</span>
                ) : (
                  <Pill>{t("billing.my.capReached")}</Pill>
                )
              ) : (
                <Pill tone="warn">{t("billing.my.notToppedUp")}</Pill>
              )}
            </li>
          ))}
        </ul>
      ) : (
        list.isSuccess && <HelpText className="mt-3">{t("billing.my.noInvitees")}</HelpText>
      )}
    </Panel>
  );
}
