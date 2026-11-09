import { screen, waitFor } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { memberMe, renderPage, reply, type ApiRoutes } from "@/test/api";
import { expectPlainWords } from "@/test/plain-words";
import type { Plan, SubView } from "../api";
import { MyPlanPage } from "./plan-page";

const usd = (micro: number) => ({ micro, currency: "USD" });
const me = memberMe({ pages: ["billing.wallet", "billing.plan"], features: ["limits", "routing", "request_log", "metrics", "billing"] });

const plans: Plan[] = [
  { id: "p1", name: "Pro", description: "USD 30 of usage a month", price_micro: 20e6, currency: "USD", period: "month", allowance_unit: "money", allowance: 30e6, overage: "balance", created_at: "2026-10-01T00:00:00Z" },
  { id: "p2", name: "Starter", price_micro: 5e6, currency: "USD", period: "week", allowance_unit: "tokens", allowance: 5_000_000, overage: "block", created_at: "2026-10-01T00:00:00Z" },
];

const sub: SubView = {
  id: "s1", user_id: "u_alice", payer: "user:u_alice", plan_id: "p1", plan_name: "Pro", status: "active", current: true, period: "month",
  price_micro: 20e6, currency: "USD", allowance_unit: "money", allowance: 30e6, used: 26e6, remaining: 4e6, held: 0, overage: "balance",
  period_start: "2026-11-01T00:00:00Z", period_end: "2026-12-01T00:00:00Z", cancel_at_period_end: false, provider: "balance",
  renewal: "balance", created_at: "2026-11-01T00:00:00Z",
};

function routes(over: ApiRoutes = {}): ApiRoutes {
  return {
    "GET /api/plugins/admin.subscriptions/me": { subscription: sub, plan: plans[0] },
    "GET /api/plugins/admin.subscriptions/plans": { items: plans },
    "GET /api/plugins/admin.subscriptions/config": { currency: "USD", methods: [{ provider: "balance" }, { provider: "stripe" }] },
    "GET /api/plugins/admin.subscriptions/me/transactions": {
      items: [
        { id: "a1", subject: "sub:s1", kind: "allowance", amount: usd(-1e6), balance_after: usd(4e6), amount_micro: -1e6, balance_after_micro: 4e6, created_at: "2026-11-06T00:00:00Z" },
      ],
      next_cursor: "",
    },
    "GET /api/plugins/meter.balance/me/payers": { payer: "user", items: [{ subject: "user:u_alice" }] },
    ...over,
  };
}

describe("MyPlanPage", () => {
  it("shows the current plan, the plans on offer and the allowance history in plain words (zh-CN)", async () => {
    renderPage(<MyPlanPage />, { path: "/billing/plan", me, routes: routes(), locale: "zh-CN" });
    expect(await screen.findByText("生效中")).toBeInTheDocument();
    expect(screen.getByText("已用 $26.00，共 $30.00")).toBeInTheDocument();
    expect(screen.getByText("剩余 $4.00")).toBeInTheDocument();
    expect(await screen.findByText("Starter")).toBeInTheDocument();
    expect(screen.getByText("每期 5,000,000 token 用量")).toBeInTheDocument();
    expect(screen.getByText(/想换套餐/)).toBeInTheDocument();
    for (const b of screen.getAllByRole("button", { name: "订阅" })) expect(b).toBeDisabled();
    expect(await screen.findByText("套餐额度")).toBeInTheDocument();
    expectPlainWords();
  });

  it("cancels at the period end after confirming", async () => {
    const { user, api } = renderPage(<MyPlanPage />, {
      path: "/billing/plan",
      me,
      routes: routes({ "POST /api/plugins/admin.subscriptions/me/cancel": { ...sub, cancel_at_period_end: true } }),
    });
    await user.click(await screen.findByRole("button", { name: "Cancel plan" }));
    expect(await screen.findByText(/stays active until/)).toBeInTheDocument();
    await user.click(screen.getAllByRole("button", { name: "Cancel plan" }).at(-1)!);
    await waitFor(() => expect(api.called("POST /api/plugins/admin.subscriptions/me/cancel")).toHaveLength(1));
    expect(api.called("POST /api/plugins/admin.subscriptions/me/cancel")[0]!.body).toEqual({ cancel: true });
  });

  it("subscribes with the chosen way to pay", async () => {
    const { user, api } = renderPage(<MyPlanPage />, {
      path: "/billing/plan",
      me,
      routes: routes({
        "GET /api/plugins/admin.subscriptions/me": { subscription: null },
        "POST /api/plugins/admin.subscriptions/subscribe": { subscription_id: "s2", status: "active" },
      }),
    });
    expect(await screen.findByText("You have no plan")).toBeInTheDocument();
    await user.click(await screen.findByRole("radio", { name: "Prepaid balance" }));
    await user.click(screen.getAllByRole("button", { name: "Subscribe" })[1]!);
    expect(await screen.findByText("Your plan is active.")).toBeInTheDocument();
    expect(api.called("POST /api/plugins/admin.subscriptions/subscribe")[0]!.body).toEqual({ plan_id: "p2", provider: "balance" });
  });

  it("words a refused subscription", async () => {
    const { user } = renderPage(<MyPlanPage />, {
      path: "/billing/plan",
      me,
      routes: routes({
        "GET /api/plugins/admin.subscriptions/me": { subscription: null },
        "POST /api/plugins/admin.subscriptions/subscribe": reply(409, { error: { code: "insufficient_balance", message: "insufficient balance" } }),
      }),
    });
    await user.click((await screen.findAllByRole("button", { name: "Subscribe" }))[0]!);
    expect(await screen.findByText("Not enough balance.")).toBeInTheDocument();
  });

  it("waits for a plan payment after coming back", async () => {
    renderPage(<MyPlanPage />, {
      path: "/billing/plan?subscription=s1",
      me,
      routes: routes(),
    });
    expect(await screen.findByText("Your plan is active.")).toBeInTheDocument();
  });
});
