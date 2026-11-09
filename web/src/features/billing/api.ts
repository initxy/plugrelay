import { rawFetch } from "@/lib/api/client";
import { ApiError } from "@/lib/api/errors";
import type { Money } from "@/lib/api/types";
import { activeLocale, hasMessage, translate } from "@/i18n";

/*
 * Typed calls to the APIs of the modules behind the billing and sign-up features, served under
 * /api/plugins/<module>/. Module routes are not in the OpenAPI document, so the shapes are declared
 * here to match the Go handlers in plugins/meter/balance and
 * plugins/admin/{payment,redeem,subscriptions,rewards,announcements,importnewapi,signup}.
 */

type ErrBody = ConstructorParameters<typeof ApiError>[1];

// Exported and named pluginJSON below because plugins/builtin/console_routes_test.go reads this
// file: it matches `export const X = "<module>"` and every pluginJSON call against the Go routes.
export const BALANCE = "meter.balance";
export const REDEEM = "admin.redeem";
export const PAYMENT = "admin.payment";
export const SIGNUP = "admin.signup";
export const IMPORT = "admin.import-newapi";
export const SUBSCRIPTIONS = "admin.subscriptions";
export const REWARDS = "admin.rewards";
export const ANNOUNCEMENTS = "admin.announcements";

type Params = Record<string, string | number | boolean | undefined>;

function query(params?: Params): string {
  if (!params) return "";
  const q = new URLSearchParams();
  for (const [k, v] of Object.entries(params)) if (v !== undefined && v !== "") q.set(k, String(v));
  const s = q.toString();
  return s ? "?" + s : "";
}

/**
 * Calls a module's API. Errors become an `ApiError` worded by `billing.err.<code>` (with the
 * error's params) when the console has that entry, else by the admin API's wording.
 */
export async function pluginJSON<T>(moduleId: string, path: string, init?: { method?: string; body?: unknown; query?: Params }): Promise<T> {
  const res = await rawFetch(`/api/plugins/${encodeURIComponent(moduleId)}/${path}${query(init?.query)}`, {
    method: init?.method ?? (init?.body === undefined ? "GET" : "POST"),
    headers: init?.body === undefined ? undefined : { "Content-Type": "application/json" },
    body: init?.body === undefined ? undefined : JSON.stringify(init.body),
  });
  const body: unknown = res.headers.get("content-type")?.includes("json") ? await res.json() : undefined;
  if (!res.ok) {
    const err = new ApiError(res.status, body as ErrBody, res.statusText);
    const key = "billing.err." + err.code;
    if (hasMessage(key)) err.message = translate(activeLocale(), key, err.params);
    throw err;
  }
  return body as T;
}

// ---------------------------------------------------------------- balances (meter.balance)

export type Payer = "user" | "group" | "key";

export interface AccountView {
  /** "user:<id>", "group:<id>" or "key:<id>". */
  subject: string;
  balance: Money;
  available: Money;
  held: Money;
  credit_limit: Money;
  updated_at?: string;
}

export type TxnKind = "topup" | "charge" | "refund" | "redeem" | "adjustment" | "subscription" | "allowance" | "checkin" | "referral";

export interface TxnView {
  id: string;
  subject: string;
  kind: TxnKind;
  amount: Money;
  balance_after: Money;
  /** The same in micro-units; plan allowances on token plans count tokens here. */
  amount_micro: number;
  balance_after_micro: number;
  request_id?: string;
  ref?: string;
  note?: string;
  actor?: string;
  created_at: string;
}

export interface TxnPage {
  items: TxnView[];
  next_cursor: string;
}

export interface PayerChoice {
  subject: string;
  key_id?: string;
  key_name?: string;
}

export const balanceApi = {
  me: (keyId?: string) => pluginJSON<AccountView>(BALANCE, "me", { query: { key_id: keyId } }),
  myTxns: (q: { key_id?: string; kind?: TxnKind; cursor?: string; limit?: number }) => pluginJSON<TxnPage>(BALANCE, "me/transactions", { query: q }),
  payers: () => pluginJSON<{ payer: Payer; items: PayerChoice[] }>(BALANCE, "me/payers"),
  accounts: () => pluginJSON<{ items: AccountView[]; currency: string; payer: Payer }>(BALANCE, "accounts"),
  txns: (q: { subject?: string; kind?: TxnKind; cursor?: string; limit?: number }) => pluginJSON<TxnPage>(BALANCE, "transactions", { query: q }),
  adjust: (b: { subject: string; amount_micro: number; note: string; idempotency_key: string }) =>
    pluginJSON<{ applied: boolean; account: AccountView }>(BALANCE, "adjust", { body: b }),
  creditLimit: (b: { subject: string; credit_limit_micro: number }) => pluginJSON<AccountView>(BALANCE, "credit-limit", { method: "PUT", body: b }),
};

// ---------------------------------------------------------------- redeem codes (admin.redeem)

interface RedeemUse {
  subject: string;
  created_at: string;
}

export interface RedeemCode {
  id: string;
  last4: string;
  batch?: string;
  amount_micro: number;
  amount: Money;
  max_uses: number;
  uses: number;
  expires_at?: string;
  disabled?: boolean;
  created_by?: string;
  created_at: string;
  /** Who redeemed it, when listed with `uses: true`. */
  redeemed?: RedeemUse[];
}

export interface RedeemBatch {
  batch: string;
  codes: number;
  amount: Money;
  max_uses: number;
  uses: number;
  disabled: number;
  expired: number;
  expires_at?: string;
  created_at: string;
  created_by?: string;
  redeemed: Money;
}

export const redeemApi = {
  batches: () => pluginJSON<{ items: RedeemBatch[]; currency: string }>(REDEEM, "batches"),
  codes: (batch?: string, uses?: boolean) =>
    pluginJSON<{ items: RedeemCode[]; currency: string }>(REDEEM, "codes", { query: { batch, uses: uses ? 1 : undefined } }),
  generate: (b: { count: number; amount_micro: number; max_uses: number; expires_at?: string; batch?: string }) =>
    pluginJSON<{ batch: string; codes: string[]; amount: Money }>(REDEEM, "batches", { body: b }),
  disableCode: (id: string, disabled: boolean) => pluginJSON<undefined>(REDEEM, "codes/disable", { body: { id, disabled } }),
  disableBatch: (batch: string, disabled: boolean) => pluginJSON<undefined>(REDEEM, "batches/disable", { body: { batch, disabled } }),
  redeem: (b: { code: string; key_id?: string }) => pluginJSON<{ amount: Money; balance: Money }>(REDEEM, "redeem", { body: b }),
};

// ---------------------------------------------------------------- payments, orders, coupons (admin.payment)

/** One way to pay: a provider, and for epay the channel (alipay, wxpay, …). */
export interface PayMethod {
  provider: string;
  method?: string;
}

export interface PaymentConfig {
  providers: string[];
  currency: string;
  methods: PayMethod[];
  min_micro: number;
  max_micro: number;
  presets_micro: number[];
  /** Coupons can be entered at checkout. */
  coupons: boolean;
}

export type OrderStatus = "pending" | "paid" | "canceled";
export type OrderKind = "topup" | "plan";

export interface Order {
  id: string;
  kind: OrderKind;
  user_id: string;
  subject: string;
  /** What the order is worth: credited for a top-up, the price of a plan. */
  amount: Money;
  /** What the provider charged (amount less the coupon's discount). */
  pay: Money;
  discount: Money;
  provider: string;
  method?: string;
  session_id?: string;
  coupon_id?: string;
  coupon_code?: string;
  /** The subscription a plan order paid for. */
  ref?: string;
  /** The plan's name for plan orders. */
  note?: string;
  status: OrderStatus;
  created_at: string;
  paid_at?: string;
}

interface OrderTotals {
  paid_micro: number;
  paid_count: number;
  pending_count: number;
}

export interface OrderPage {
  items: Order[];
  next_cursor: string;
  currency: string;
  /** Last 30 days; only on the first page. */
  totals?: OrderTotals;
}

export type CouponState = "active" | "scheduled" | "ended" | "used_up" | "disabled";

export interface Coupon {
  id: string;
  code: string;
  /** Percent off (1–100); 0 for a fixed amount off. */
  percent: number;
  amount: Money;
  max_discount: Money;
  min_topup: Money;
  amount_micro: number;
  max_discount_micro: number;
  min_topup_micro: number;
  /** 0 = no limit. */
  max_uses: number;
  /** 0 = no limit. */
  per_user: number;
  starts_at?: string;
  ends_at?: string;
  disabled: boolean;
  note?: string;
  created_by?: string;
  created_at: string;
  updated_at: string;
  used: number;
  pending: number;
  state: CouponState;
}

export interface CouponInput {
  code?: string;
  percent: number;
  amount_micro: number;
  max_discount_micro: number;
  min_topup_micro: number;
  max_uses: number;
  per_user: number;
  starts_at?: string;
  ends_at?: string;
  disabled: boolean;
  note?: string;
}

export interface CouponUse {
  order_id: string;
  coupon_id: string;
  user_id: string;
  discount: Money;
  status: "pending" | "paid";
  created_at: string;
}

export const paymentApi = {
  config: () => pluginJSON<PaymentConfig>(PAYMENT, "config"),
  checkout: (b: { amount_micro: number; provider?: string; method?: string; key_id?: string; coupon?: string }) =>
    pluginJSON<{ order_id: string; url: string }>(PAYMENT, "checkout", { body: b }),
  order: (id: string) => pluginJSON<Order>(PAYMENT, `orders/${encodeURIComponent(id)}`),
  cancel: (id: string) => pluginJSON<Order>(PAYMENT, `orders/${encodeURIComponent(id)}/cancel`, { body: {} }),
  orders: (q: { status?: OrderStatus; provider?: string; kind?: OrderKind; user_id?: string; cursor?: string; limit?: number }) =>
    pluginJSON<OrderPage>(PAYMENT, "orders", { query: q }),
  checkCoupon: (b: { code: string; amount_micro: number }) =>
    pluginJSON<{ code: string; percent: number; discount: Money; pay: Money }>(PAYMENT, "coupon-check", { body: b }),
  coupons: () => pluginJSON<{ items: Coupon[]; currency: string; enabled: boolean }>(PAYMENT, "coupons"),
  createCoupon: (b: CouponInput & { code: string }) => pluginJSON<Coupon>(PAYMENT, "coupons", { body: b }),
  updateCoupon: (id: string, b: CouponInput) => pluginJSON<Coupon>(PAYMENT, `coupons/${encodeURIComponent(id)}`, { method: "PUT", body: b }),
  couponUses: (id: string) => pluginJSON<{ items: CouponUse[] }>(PAYMENT, `coupons/${encodeURIComponent(id)}/uses`),
};

// ---------------------------------------------------------------- plans (admin.subscriptions)

export type Period = "week" | "month" | "year";
export type AllowanceUnit = "money" | "tokens";
export type Overage = "block" | "balance";

export interface Plan {
  id: string;
  name: string;
  description?: string;
  price_micro: number;
  currency: string;
  period: Period;
  allowance_unit: AllowanceUnit;
  /** Micro-units of money for money plans, tokens for token plans. */
  allowance: number;
  overage: Overage;
  group_id?: string;
  disabled?: boolean;
  created_at: string;
  updated_at?: string;
}

export interface PlanInput {
  id?: string;
  name: string;
  description?: string;
  price_micro: number;
  period: Period;
  allowance_unit: AllowanceUnit;
  allowance: number;
  overage: Overage;
  group_id?: string;
  disabled?: boolean;
}

export interface SubView {
  id: string;
  user_id: string;
  payer: string;
  plan_id: string;
  plan_name: string;
  status: "pending" | "active" | "ended";
  /** Active and inside its period now. */
  current: boolean;
  period: Period;
  price_micro: number;
  currency: string;
  allowance_unit: AllowanceUnit;
  allowance: number;
  used: number;
  remaining: number;
  held: number;
  overage: Overage;
  period_start?: string;
  period_end?: string;
  cancel_at_period_end: boolean;
  /** "balance" or a payment provider. */
  provider: string;
  renewal: "balance" | "provider";
  /** Paid while another subscription of the user was active: never started, refund it. */
  refund_due?: boolean;
  created_at: string;
}

export const plansApi = {
  config: () => pluginJSON<{ currency: string; methods: PayMethod[] }>(SUBSCRIPTIONS, "config"),
  plans: (all?: boolean) => pluginJSON<{ items: Plan[] }>(SUBSCRIPTIONS, "plans", { query: { all: all ? 1 : undefined } }),
  savePlan: (b: PlanInput) => pluginJSON<Plan>(SUBSCRIPTIONS, "plans", { body: b }),
  me: () => pluginJSON<{ subscription: SubView | null; plan?: Plan }>(SUBSCRIPTIONS, "me"),
  subscribe: (b: { plan_id: string; provider: string; method?: string; key_id?: string }) =>
    pluginJSON<{ subscription_id: string; status: string; url?: string }>(SUBSCRIPTIONS, "subscribe", { body: b }),
  cancelMine: (cancel: boolean) => pluginJSON<SubView>(SUBSCRIPTIONS, "me/cancel", { body: { cancel } }),
  myTxns: (cursor?: string) => pluginJSON<TxnPage>(SUBSCRIPTIONS, "me/transactions", { query: { cursor } }),
  subscriptions: (status?: string) => pluginJSON<{ items: SubView[] }>(SUBSCRIPTIONS, "subscriptions", { query: { status } }),
  cancel: (id: string, cancel: boolean) => pluginJSON<SubView>(SUBSCRIPTIONS, "subscriptions/cancel", { body: { id, cancel } }),
  end: (id: string) => pluginJSON<SubView>(SUBSCRIPTIONS, `subscriptions/${encodeURIComponent(id)}/end`, { body: {} }),
  refunded: (id: string, refunded: boolean) =>
    pluginJSON<SubView>(SUBSCRIPTIONS, `subscriptions/${encodeURIComponent(id)}/refunded`, { body: { refunded } }),
};

// ---------------------------------------------------------------- rewards (admin.rewards)

export interface Rewards {
  currency: string;
  checkin: {
    enabled: boolean;
    min: Money;
    max: Money;
    /** Today in the site's time zone, YYYY-MM-DD. */
    today: string;
    done_today: boolean;
    today_amount?: Money;
    streak: number;
    /** Checked-in days, newest first (last 60). */
    days: string[];
    recent_total: Money;
  };
  referral: {
    enabled: boolean;
    code?: string;
    invited?: number;
    rewarded?: number;
    earned?: Money;
    mode?: "percent" | "fixed";
    percent?: number;
    fixed?: Money;
    /** Zero = no cap. */
    cap?: Money;
  };
}

/** One person the caller invited (the member's own list, which leaves out who they are). */
export interface ReferralRow {
  inviter_id: string;
  created_at: string;
  reward: Money;
  rewarded_at?: string;
  order_id?: string;
}

export const rewardsApi = {
  me: () => pluginJSON<Rewards>(REWARDS, "me"),
  checkIn: (keyId?: string) => pluginJSON<{ day: string; amount: Money; balance: Money }>(REWARDS, "checkin", { body: { key_id: keyId } }),
  myReferrals: () => pluginJSON<{ items: ReferralRow[]; currency: string }>(REWARDS, "me/referrals"),
};

// ---------------------------------------------------------------- announcements (admin.announcements)

export type Audience = "all" | "members" | "admins";

export interface Announcement {
  id: string;
  title: string;
  /** Markdown. */
  body: string;
  audience: Audience;
  starts_at?: string;
  ends_at?: string;
  created_by?: string;
  created_at: string;
  updated_at: string;
  /** Operators' list only. */
  state?: "scheduled" | "live" | "ended";
}

export interface AnnouncementInput {
  title: string;
  body: string;
  audience: Audience;
  starts_at?: string;
  ends_at?: string;
}

export const announcementsApi = {
  all: () => pluginJSON<{ items: Announcement[]; enabled: boolean }>(ANNOUNCEMENTS, "announcements"),
  create: (b: AnnouncementInput) => pluginJSON<Announcement>(ANNOUNCEMENTS, "announcements", { body: b }),
  update: (id: string, b: AnnouncementInput) =>
    pluginJSON<Announcement>(ANNOUNCEMENTS, `announcements/${encodeURIComponent(id)}`, { method: "PUT", body: b }),
  remove: (id: string) => pluginJSON<undefined>(ANNOUNCEMENTS, `announcements/${encodeURIComponent(id)}`, { method: "DELETE" }),
  mine: () => pluginJSON<{ items: Announcement[] }>(ANNOUNCEMENTS, "me"),
  dismiss: (id: string) => pluginJSON<undefined>(ANNOUNCEMENTS, "me/dismiss", { body: { id } }),
};

// ---------------------------------------------------------------- new-api import (admin.import-newapi)

export interface ImportItem {
  kind: string;
  source?: string;
  id: string;
  op: "create" | "update" | "unchanged" | "skip";
  /** The importer's English reason; shown as data. */
  reason?: string;
}

export interface ImportReport {
  dry_run: boolean;
  counts: Record<string, number>;
  items: ImportItem[];
  warnings: string[];
}

export type ImportSource =
  | { driver: "mysql" | "postgres" | "sqlite"; dsn: string; confirm_local_file?: boolean }
  | { data: unknown };

export const importApi = {
  run: (src: ImportSource, opts: { rate?: number; dry_run: boolean }) => pluginJSON<ImportReport>(IMPORT, "run", { body: { ...src, ...opts } }),
};

// ---------------------------------------------------------------- helpers

/** An idempotency key for one submitted form. */
export function formKey(): string {
  return typeof crypto !== "undefined" && "randomUUID" in crypto ? crypto.randomUUID() : String(Math.random()).slice(2);
}

/** Splits a ledger subject ("user:u1") into its kind and id. */
export function splitSubject(subject: string): { kind: "user" | "group" | "key" | "sub" | string; id: string } {
  const i = subject.indexOf(":");
  return i < 0 ? { kind: "", id: subject } : { kind: subject.slice(0, i), id: subject.slice(i + 1) };
}
