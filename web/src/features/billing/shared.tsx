import { useInfiniteQuery, useQuery } from "@tanstack/react-query";
import { Link, useRouterState } from "@tanstack/react-router";
import type { ColumnDef } from "@tanstack/react-table";
import { CheckCircle2, KeyRound, Loader2, ReceiptText, X } from "lucide-react";
import { useCallback, useMemo, useState } from "react";
import { DataTable } from "@/components/data-table";
import { EmptyState, ErrorState } from "@/components/page";
import { Pill, type Tone } from "@/components/status";
import { Button } from "@/components/ui/button";
import { Select } from "@/components/ui/select";
import { api, unwrap } from "@/lib/api/client";
import type { Money as MoneyValue } from "@/lib/api/types";
import { formatMoney } from "@/lib/money";
import { formatDateTime, formatNumber, formatWhen } from "@/lib/format";
import { cn } from "@/lib/cn";
import { useI18n } from "@/i18n";
import { balanceApi, splitSubject, type AllowanceUnit, type OrderStatus, type PayMethod, type PayerChoice, type TxnKind, type TxnPage, type TxnView } from "./api";
import { useLogsPath } from "@/app/session";

// ---------------------------------------------------------------- words for codes

const TXN_KINDS: readonly TxnKind[] = ["topup", "charge", "refund", "redeem", "adjustment", "subscription", "allowance", "checkin", "referral"];

const TXN_TONE: Record<TxnKind, Tone> = {
  topup: "ok",
  charge: "neutral",
  refund: "info",
  redeem: "ok",
  adjustment: "warn",
  subscription: "violet",
  allowance: "neutral",
  checkin: "brand",
  referral: "brand",
};

export const ORDER_TONE: Record<OrderStatus, Tone> = { pending: "warn", paid: "ok", canceled: "neutral" };

/** "Card (Stripe)", "Alipay", "Prepaid balance" … for a provider and its method. */
export function usePayMethodLabel() {
  const { t, has } = useI18n();
  return useCallback(
    (m: PayMethod): string => {
      const key = `billing.method.${m.method || m.provider}`;
      if (has(key)) return t(key);
      const pkey = `billing.method.${m.provider}`;
      return has(pkey) ? t(pkey) : m.method ? `${m.provider} · ${m.method}` : m.provider;
    },
    [t, has],
  );
}

/** One way to pay as a single string, for a radio group's value. */
export const methodKey = (m: PayMethod) => m.provider + "|" + (m.method ?? "");

/** A plan allowance in words: a token count, or money in the plan's currency. */
export function useAllowanceText() {
  const { t, locale } = useI18n();
  return useCallback(
    (amount: number, unit: AllowanceUnit, currency: string) =>
      unit === "tokens" ? t("billing.tokens", { n: formatNumber(amount, locale) }) : formatMoney({ micro: amount, currency }, locale),
    [t, locale],
  );
}

/** A query-string value as a string, whatever the router parsed it into. */
/** Where a payment stands once the member is back from the provider. */
export type ReturnState = "waiting" | "done" | "late" | "canceled";

/** The banner over a member page after paying: spinner while waiting, check when done, a close button. */
export function PaymentReturnBanner({ state, text, onClose }: { state: ReturnState; text: string; onClose: () => void }) {
  const { t } = useI18n();
  return (
    <div
      role="status"
      className={cn(
        "mb-4 flex items-center gap-3 rounded-md border px-3 py-2 text-sm",
        state === "done" ? "border-ok/30 bg-ok-soft text-ok" : state === "late" ? "border-warn/30 bg-warn-soft text-warn" : "border-line bg-surface-2 text-ink-2",
      )}
    >
      {state === "done" && <CheckCircle2 aria-hidden className="size-4" />}
      {state === "waiting" && <Loader2 aria-hidden className="size-4 animate-spin" />}
      <span className="min-w-0 flex-1">{text}</span>
      <Button size="icon-sm" variant="ghost" aria-label={t("common.close")} onClick={onClose}>
        <X aria-hidden />
      </Button>
    </div>
  );
}

export function useSearchParam(name: string): string | undefined {
  return useRouterState({
    select: (s) => {
      const v = (s.location.search as Record<string, unknown>)[name];
      return typeof v === "string" || typeof v === "number" || typeof v === "boolean" ? String(v) : undefined;
    },
  });
}

// ---------------------------------------------------------------- names of accounts (operators)

const usersQuery = { queryKey: ["users"], queryFn: () => unwrap(api.GET("/users")) };
const groupsQuery = { queryKey: ["groups"], queryFn: () => unwrap(api.GET("/groups")) };
const keysQuery = { queryKey: ["keys"], queryFn: () => unwrap(api.GET("/keys")) };

export interface SubjectName {
  /** What people call it: a user's display name, a group's or key's name. */
  name: string;
  /** Second line: the user's login, "group", "key of <owner>". */
  detail?: string;
  kind: string;
  id: string;
}

/**
 * Names ledger subjects ("user:u1", "group:g1", "key:k1") and user ids from the users, groups and
 * keys lists. Operators only; the lists load once and are shared with the other pages.
 */
export function useNames() {
  const { t } = useI18n();
  const users = useQuery(usersQuery);
  const groups = useQuery(groupsQuery);
  const keys = useQuery(keysQuery);
  const user = useCallback(
    (id: string): SubjectName => {
      const u = users.data?.items.find((x) => x.id === id);
      return { kind: "user", id, name: u ? u.display_name || u.username : t("billing.unknownUser"), detail: u?.display_name ? u.username : undefined };
    },
    [users.data, t],
  );
  const subject = useCallback(
    (s: string): SubjectName => {
      const { kind, id } = splitSubject(s);
      if (kind === "user") return user(id);
      if (kind === "group") {
        const g = groups.data?.items.find((x) => x.id === id);
        return { kind, id, name: g?.name ?? t("billing.unknownGroup"), detail: t("billing.subject.group") };
      }
      if (kind === "key") {
        const k = keys.data?.items.find((x) => x.id === id);
        const owner = k?.owner?.name;
        return { kind, id, name: k?.name ?? t("billing.unknownKey"), detail: owner ? t("billing.subject.keyOf", { name: owner }) : t("billing.subject.key") };
      }
      return { kind, id, name: s };
    },
    [user, groups.data, keys.data, t],
  );
  return { user, subject, users: users.data?.items ?? [], groups: groups.data?.items ?? [], keys: keys.data?.items ?? [] };
}

/** An account's name with its second line. */
export function SubjectCell({ name, className }: { name: SubjectName; className?: string }) {
  return (
    <span className={cn("grid min-w-0", className)}>
      <span className="truncate text-sm font-medium">{name.name}</span>
      {name.detail && <span className="truncate text-xs text-ink-3">{name.detail}</span>}
    </span>
  );
}

// ---------------------------------------------------------------- money

/** A signed amount: green with "+" for credits. */
function SignedMoney({ value, className }: { value: MoneyValue; className?: string }) {
  const { locale } = useI18n();
  const text = formatMoney(value, locale);
  return (
    <span className={cn("tabular-nums", value.micro > 0 && "text-ok", className)}>
      {value.micro > 0 ? "+" : ""}
      {text}
    </span>
  );
}

// ---------------------------------------------------------------- transactions

/** What a transaction is about, in words: its note, the request it charged, the day of a check-in. */
function TxnDetail({ txn, operator }: { txn: TxnView; operator?: boolean }) {
  const { t } = useI18n();
  const payLabel = usePayMethodLabel();
  const logs = useLogsPath();
  if (txn.kind === "charge" && txn.request_id)
    return (
      <Link to={logs} search={{ id: txn.request_id }} className="text-accent hover:underline">
        {t("billing.txn.viewRequest")}
      </Link>
    );
  if (txn.kind === "checkin") return <span>{t("billing.txn.checkinOn", { day: txn.ref ?? "" })}</span>;
  if (txn.kind === "referral") return <span>{t("billing.txn.referralNote")}</span>;
  if (txn.kind === "redeem" && txn.note) return <span data-raw>{txn.note}</span>;
  if (txn.kind === "topup") {
    // The note is "<provider> <event id>[ · <coupon>]"; the ref "<provider>:<session>".
    const provider = (txn.ref ?? txn.note ?? "").split(/[: ]/)[0] ?? "";
    const coupon = txn.note?.split(" · ")[1];
    return (
      <span>
        {provider ? t("billing.txn.paidWith", { method: payLabel({ provider }) }) : t("billing.kind.topup")}
        {coupon && <span className="ml-1 text-ink-3">{t("billing.txn.withCoupon", { code: coupon })}</span>}
      </span>
    );
  }
  if (txn.note) return <span className={cn(!operator && "text-ink-2")} data-raw>{txn.note}</span>;
  return <span className="text-ink-3">—</span>;
}

/**
 * Balance history, newest first, with a kind filter and "load more". `load` pages it; `queryKey`
 * must identify the account so switching accounts refetches.
 */
export function TxnTable({
  queryKey,
  load,
  operator,
  tokens,
  className,
}: {
  queryKey: readonly unknown[];
  load: (q: { cursor?: string; kind?: TxnKind }) => Promise<TxnPage>;
  operator?: boolean;
  /** Plan allowance on a token plan: amounts are tokens. */
  tokens?: boolean;
  className?: string;
}) {
  const { t, locale } = useI18n();
  const [kind, setKind] = useState<TxnKind | "">("");
  const q = useInfiniteQuery({
    queryKey: [...queryKey, kind],
    queryFn: ({ pageParam }) => load({ cursor: pageParam || undefined, kind: kind || undefined }),
    initialPageParam: "",
    getNextPageParam: (last) => last.next_cursor || undefined,
  });
  const rows = useMemo(() => q.data?.pages.flatMap((p) => p.items), [q.data]);
  const columns = useMemo<ColumnDef<TxnView>[]>(
    () => [
      {
        id: "time",
        header: t("billing.txn.time"),
        meta: { className: "w-[150px]" },
        cell: ({ row }) => (
          <span className="whitespace-nowrap text-xs text-ink-2 tabular-nums" title={formatDateTime(row.original.created_at, locale)}>
            {formatWhen(row.original.created_at, locale)}
          </span>
        ),
      },
      {
        id: "kind",
        header: t("billing.txn.kind"),
        meta: { className: "w-[130px]" },
        cell: ({ row }) => (
          <Pill tone={TXN_TONE[row.original.kind] ?? "neutral"} dot>
            {t(`billing.kind.${row.original.kind}`)}
          </Pill>
        ),
      },
      {
        id: "amount",
        header: t("billing.txn.amount"),
        meta: { numeric: true, className: "w-[130px]" },
        cell: ({ row }) =>
          tokens ? (
            <span className={cn("tabular-nums", row.original.amount_micro > 0 && "text-ok")}>
              {row.original.amount_micro > 0 ? "+" : ""}
              {t("billing.tokens", { n: formatNumber(row.original.amount_micro, locale) })}
            </span>
          ) : (
            <SignedMoney value={row.original.amount} />
          ),
      },
      {
        id: "after",
        header: tokens ? t("billing.txn.left") : t("billing.txn.after"),
        meta: { numeric: true, className: "w-[130px] max-md:hidden" },
        cell: ({ row }) => (
          <span className="tabular-nums text-ink-2">
            {tokens ? formatNumber(row.original.balance_after_micro, locale) : formatMoney(row.original.balance_after, locale)}
          </span>
        ),
      },
      {
        id: "detail",
        header: t("billing.txn.detail"),
        cell: ({ row }) => (
          <span className="block min-w-0 truncate text-sm">
            <TxnDetail txn={row.original} operator={operator} />
          </span>
        ),
      },
    ],
    [t, locale, tokens, operator],
  );
  return (
    <div className={className}>
      <div className="flex items-center justify-end gap-2 border-b border-line-soft px-4 py-2">
        <Select
          aria-label={t("billing.txn.kindFilter")}
          value={kind || "all"}
          onChange={(v) => setKind(v === "all" ? "" : (v as TxnKind))}
          options={[{ value: "all", label: t("billing.txn.allKinds") }, ...TXN_KINDS.map((k) => ({ value: k, label: t(`billing.kind.${k}`) }))]}
          className="h-8 w-40"
        />
      </div>
      {q.isError ? (
        <div className="p-4">
          <ErrorState error={q.error} onRetry={() => void q.refetch()} />
        </div>
      ) : (
        <DataTable
          columns={columns}
          data={rows}
          getRowId={(r) => r.id}
          loadingRows={5}
          maxHeight="none"
          className="rounded-none border-0"
          empty={<EmptyState icon={ReceiptText} compact title={t("billing.txn.empty")} hint={kind ? t("billing.txn.emptyFiltered") : undefined} />}
        />
      )}
      {q.hasNextPage && (
        <div className="flex justify-center border-t border-line-soft p-2">
          <Button size="sm" variant="ghost" onClick={() => void q.fetchNextPage()} disabled={q.isFetchingNextPage}>
            {q.isFetchingNextPage ? t("common.loading") : t("billing.txn.more")}
          </Button>
        </div>
      )}
    </div>
  );
}

// ---------------------------------------------------------------- payer picker (members)

export const payersQuery = { queryKey: ["billing", "payers"], queryFn: () => balanceApi.payers() };

/**
 * The account a member's top-up, redeem or check-in goes to. Shown only when there is a choice
 * (groups or keys pay); `value` is a key id, "" for the member's own account.
 */
export function PayerPicker({ value, onChange, className }: { value: string; onChange: (keyId: string) => void; className?: string }) {
  const { t } = useI18n();
  const q = useQuery(payersQuery);
  const items = q.data?.items ?? [];
  if (items.length <= 1) return null;
  const label = (p: PayerChoice) => (p.key_id ? t("billing.payer.key", { name: p.key_name ?? "" }) : t("billing.payer.own"));
  return (
    <label className={cn("flex items-center gap-2 text-sm", className)}>
      <span className="text-ink-2">{t("billing.payer.label")}</span>
      <Select
        value={value || "own"}
        onChange={(v) => onChange(v === "own" ? "" : v)}
        options={items.map((p) => ({ value: p.key_id ?? "own", label: label(p) }))}
        className="h-8 w-56"
      />
    </label>
  );
}

/** Shown instead of a payment form when each key pays and the member has no key yet. */
export function NoKeysState({ compact }: { compact?: boolean }) {
  const { t } = useI18n();
  return (
    <EmptyState
      icon={KeyRound}
      compact={compact}
      title={t("billing.my.noKeysTitle")}
      hint={t("billing.my.noKeysHint")}
      action={
        <Button asChild variant="primary">
          <Link to="/keys">{t("billing.my.createKey")}</Link>
        </Button>
      }
    />
  );
}

/** The key id members must pick when each key pays: the first key, so the page works without a choice. */
export function useDefaultPayer(): { keyId: string; needsKey: boolean; noKeys: boolean; loading: boolean } {
  const q = useQuery(payersQuery);
  const items = q.data?.items ?? [];
  const own = items.find((p) => !p.key_id);
  const first = items.find((p) => p.key_id);
  return { keyId: own ? "" : (first?.key_id ?? ""), needsKey: !own, noKeys: !!q.data && !own && !first, loading: q.isPending };
}

/** Leaves for a payment provider's checkout page; anything but http(s) is refused. */
export function goToCheckout(url: string) {
  const u = new URL(url, window.location.href);
  if (u.protocol !== "https:" && u.protocol !== "http:") throw new Error(`refusing checkout URL with scheme ${u.protocol}`);
  window.location.assign(u.href);
}
