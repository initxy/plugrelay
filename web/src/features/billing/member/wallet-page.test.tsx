import { screen, waitFor, within } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { memberMe, renderPage, reply, type ApiRoutes } from "@/test/api";
import { expectPlainWords } from "@/test/plain-words";
import type { Rewards, TxnView } from "../api";
import { WalletPage } from "./wallet-page";

const usd = (micro: number) => ({ micro, currency: "USD" });
const me = memberMe({ pages: ["billing.wallet", "billing.plan"], features: ["limits", "routing", "request_log", "metrics", "billing"] });

const txns: TxnView[] = [
  { id: "t1", subject: "user:u_alice", kind: "topup", amount: usd(20e6), balance_after: usd(32e6), amount_micro: 20e6, balance_after_micro: 32e6, note: "stripe evt_1", created_at: "2026-11-07T08:00:00Z" },
  { id: "t2", subject: "user:u_alice", kind: "charge", amount: usd(-120_000), balance_after: usd(12e6), amount_micro: -120_000, balance_after_micro: 12e6, request_id: "req_1", created_at: "2026-11-07T07:00:00Z" },
  { id: "t3", subject: "user:u_alice", kind: "checkin", amount: usd(300_000), balance_after: usd(12.12e6), amount_micro: 300_000, balance_after_micro: 12.12e6, ref: "2026-11-06", created_at: "2026-11-06T07:00:00Z" },
  { id: "t4", subject: "user:u_alice", kind: "referral", amount: usd(2e6), balance_after: usd(11.82e6), amount_micro: 2e6, balance_after_micro: 11.82e6, created_at: "2026-11-05T07:00:00Z" },
];

function rewards(over: Partial<Rewards["checkin"]> = {}): Rewards {
  return {
    currency: "USD",
    checkin: { enabled: true, min: usd(100_000), max: usd(500_000), today: "2026-11-07", done_today: false, streak: 3, days: ["2026-11-06"], recent_total: usd(900_000), ...over },
    referral: { enabled: true, code: "K7Q2M9XA", invited: 2, rewarded: 1, earned: usd(2e6), mode: "percent", percent: 10, fixed: usd(1e6), cap: usd(50e6) },
  };
}

function routes(over: ApiRoutes = {}): ApiRoutes {
  return {
    "GET /api/plugins/meter.balance/me/payers": { payer: "user", items: [{ subject: "user:u_alice" }] },
    "GET /api/plugins/meter.balance/me": {
      subject: "user:u_alice", balance: usd(32.4e6), available: usd(31.9e6), held: usd(500_000), credit_limit: usd(20e6),
    },
    "GET /api/plugins/meter.balance/me/transactions": { items: txns, next_cursor: "" },
    "GET /api/plugins/admin.rewards/me": rewards(),
    "GET /api/plugins/admin.rewards/me/referrals": {
      currency: "USD",
      items: [
        { inviter_id: "u_alice", created_at: "2026-11-01T10:00:00Z", reward: usd(2e6), rewarded_at: "2026-11-02T10:00:00Z" },
        { inviter_id: "u_alice", created_at: "2026-11-04T10:00:00Z", reward: usd(0) },
      ],
    },
    "GET /api/plugins/admin.payment/config": {
      providers: ["stripe", "epay"], currency: "USD", min_micro: 1e6, max_micro: 1000e6, presets_micro: [5e6, 20e6, 100e6], coupons: true,
      methods: [{ provider: "stripe" }, { provider: "epay", method: "alipay" }],
    },
    ...over,
  };
}

describe("WalletPage", () => {
  it("shows balance, top-up, redeem, invite link and history in plain words (zh-CN)", async () => {
    renderPage(<WalletPage />, { path: "/billing", me, routes: routes(), locale: "zh-CN" });
    expect(await screen.findByText("可用余额")).toBeInTheDocument();
    await screen.findByText("$31.90");
    expect(screen.getByText("可透支额度")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /签到/ })).toBeEnabled();
    expect(await screen.findByText(/signup\?ref=K7Q2M9XA/)).toBeInTheDocument();
    expect(screen.getByText(/实付金额的 10%/)).toBeInTheDocument();
    expect(await screen.findByText("还没充值")).toBeInTheDocument();
    expect(await screen.findByText("2026-11-06 签到")).toBeInTheDocument();
    expect(screen.getByRole("radio", { name: "支付宝" })).toBeInTheDocument();
    expectPlainWords();
  });

  it("links a charge to the request on the member's usage page", async () => {
    renderPage(<WalletPage />, { path: "/billing", me, routes: routes() });
    expect(await screen.findByRole("link", { name: "View the request" })).toHaveAttribute("href", "/usage?id=req_1");
  });

  it("checks in once and says so", async () => {
    let done = false;
    const { user, api } = renderPage(<WalletPage />, {
      path: "/billing",
      me,
      routes: routes({
        "GET /api/plugins/admin.rewards/me": () => (done ? rewards({ done_today: true, today_amount: usd(300_000), streak: 4 }) : rewards()),
        "POST /api/plugins/admin.rewards/checkin": () => {
          done = true;
          return { day: "2026-11-07", amount: usd(300_000), balance: usd(32.2e6) };
        },
      }),
    });
    await user.click(await screen.findByRole("button", { name: /Check in/ }));
    expect(await screen.findByText(/Checked in: \$0.30 added/)).toBeInTheDocument();
    expect(await screen.findByRole("button", { name: /Checked in today · \+\$0.30/ })).toBeDisabled();
    expect(api.called("POST /api/plugins/admin.rewards/checkin")).toHaveLength(1);
  });

  it("checks a coupon and pays the discounted amount", async () => {
    const { user, api } = renderPage(<WalletPage />, {
      path: "/billing",
      me,
      routes: routes({
        "POST /api/plugins/admin.payment/coupon-check": { code: "SPRING20", percent: 20, discount: usd(4e6), pay: usd(16e6) },
        "POST /api/plugins/admin.payment/checkout": { order_id: "ord_1", url: "#pay" },
      }),
    });
    await user.click(await screen.findByRole("button", { name: "$20.00" }));
    await user.type(screen.getByLabelText(/Coupon/), "spring20");
    await user.click(screen.getByRole("button", { name: "Check" }));
    expect(await screen.findByText("$4.00 off: you pay $16.00.")).toBeInTheDocument();
    await user.click(screen.getByRole("radio", { name: "Alipay" }));
    await user.click(screen.getByRole("button", { name: "Pay $16.00" }));
    await waitFor(() => expect(api.called("POST /api/plugins/admin.payment/checkout")).toHaveLength(1));
    expect(api.called("POST /api/plugins/admin.payment/checkout")[0]!.body).toEqual({
      amount_micro: 20e6, provider: "epay", method: "alipay", coupon: "spring20",
    });
  });

  it("shows a refused coupon in words", async () => {
    const { user } = renderPage(<WalletPage />, {
      path: "/billing",
      me,
      routes: routes({
        "POST /api/plugins/admin.payment/coupon-check": reply(422, { error: { code: "coupon_used_up", message: "coupon has been used up" } }),
      }),
    });
    await user.type(await screen.findByLabelText(/Coupon/), "gone");
    await user.click(screen.getByRole("button", { name: "Check" }));
    expect(await screen.findByText("This coupon has been used up.")).toBeInTheDocument();
  });

  it("waits for the payment after coming back from the provider", async () => {
    let polls = 0;
    renderPage(<WalletPage />, {
      path: "/billing?order=ord_1",
      me,
      routes: routes({
        "GET /api/plugins/admin.payment/orders/{id}": () => ({
          id: "ord_1", kind: "topup", user_id: "u_alice", subject: "user:u_alice", amount: usd(20e6), pay: usd(20e6), discount: usd(0),
          provider: "stripe", status: ++polls > 1 ? "paid" : "pending", created_at: "2026-11-07T08:00:00Z",
        }),
      }),
    });
    expect(await screen.findByText("Waiting for the payment to be confirmed…")).toBeInTheDocument();
    expect(await screen.findByText("Payment received: $20.00 added to your balance.", undefined, { timeout: 4000 })).toBeInTheDocument();
  });

  it("cancels the order when the payment was abandoned", async () => {
    const { api } = renderPage(<WalletPage />, {
      path: "/billing?order=ord_2&canceled=1",
      me,
      routes: routes({ "POST /api/plugins/admin.payment/orders/{id}/cancel": { id: "ord_2", status: "canceled" } }),
    });
    expect(await screen.findByText("Payment canceled. Nothing was charged.")).toBeInTheDocument();
    await waitFor(() => expect(api.called("POST", "/api/plugins/admin.payment/orders/ord_2/cancel")).toHaveLength(1));
  });

  it("redeems a code into the chosen key when keys pay", async () => {
    const { user, api } = renderPage(<WalletPage />, {
      path: "/billing",
      me,
      routes: routes({
        "GET /api/plugins/meter.balance/me/payers": {
          payer: "key",
          items: [
            { subject: "key:k1", key_id: "k1", key_name: "laptop" },
            { subject: "key:k2", key_id: "k2", key_name: "ci" },
          ],
        },
        "POST /api/plugins/admin.redeem/redeem": { amount: usd(10e6), balance: usd(42e6) },
      }),
    });
    await user.type(await screen.findByLabelText("Redeem code"), "RC-AAAA");
    const panel = screen.getByRole("button", { name: "Redeem" });
    await user.click(panel);
    expect(await screen.findByText("$10.00 added. Balance: $42.00.")).toBeInTheDocument();
    expect(api.called("POST /api/plugins/admin.redeem/redeem")[0]!.body).toEqual({ code: "RC-AAAA", key_id: "k1" });
    expect(api.lastQuery("/api/plugins/meter.balance/me")).toEqual({ key_id: "k1" });
  });

  it("asks for a key first when each key pays and there is none", async () => {
    renderPage(<WalletPage />, {
      path: "/billing",
      me,
      routes: routes({ "GET /api/plugins/meter.balance/me/payers": { payer: "key", items: [] } }),
      locale: "zh-CN",
    });
    const empty = await screen.findByText("先创建一个密钥");
    expect(within(empty.parentElement!).getByRole("link", { name: "创建密钥" })).toHaveAttribute("href", "/keys");
    expectPlainWords();
  });
});
