import { screen, within } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { translate } from "@/i18n";
import { renderPage, type ApiRequest, type ApiRoutes } from "@/test/api";
import { expectPlainWords } from "@/test/plain-words";
import type { Order } from "../api";
import { OrdersPage } from "./orders-page";
import { billingAdmin, nameRoutes, usd } from "./test-fixtures";

const topup: Order = {
  id: "ord_1",
  kind: "topup",
  user_id: "u_alice",
  subject: "user:u_alice",
  amount: usd(50),
  pay: usd(40),
  discount: usd(10),
  provider: "stripe",
  session_id: "cs_test_1",
  coupon_id: "cp1",
  coupon_code: "SPRING20",
  status: "paid",
  created_at: "2026-11-07T08:00:00Z",
  paid_at: "2026-11-07T08:01:00Z",
};
const plan: Order = {
  ...topup,
  id: "sord_2",
  kind: "plan",
  user_id: "u_bob",
  subject: "user:u_bob",
  amount: usd(20),
  pay: usd(20),
  discount: usd(0),
  provider: "epay",
  method: "alipay",
  coupon_id: undefined,
  coupon_code: undefined,
  ref: "sub_1",
  note: "Pro",
  status: "pending",
  paid_at: undefined,
};

function routes() {
  const r: ApiRoutes = {
    ...nameRoutes,
    "GET /api/plugins/admin.payment/orders": ({ query }: ApiRequest) => ({
      items: [topup, plan].filter((o) => !query.get("status") || o.status === query.get("status")),
      next_cursor: "",
      currency: "USD",
      totals: { paid_micro: 40e6, paid_count: 1, pending_count: 1 },
    }),
    "GET /api/plugins/admin.payment/orders/{id}": ({ params }: ApiRequest) => [topup, plan].find((o) => o.id === params.id),
  };
  return r;
}

describe("OrdersPage", () => {
  it("lists orders with totals in plain words (zh-CN)", async () => {
    renderPage(<OrdersPage />, { locale: "zh-CN", routes: routes(), me: billingAdmin });
    expect(await screen.findByText("Alice Chen")).toBeInTheDocument();
    expect(screen.getByText(translate("zh-CN", "billing.method.alipay"))).toBeInTheDocument();
    expect(screen.getAllByText(translate("zh-CN", "billing.order.pending")).length).toBeGreaterThan(0);
    expectPlainWords();
  });

  it("filters by status through the URL", async () => {
    const { user, router, api } = renderPage(<OrdersPage />, { routes: routes(), me: billingAdmin });
    await screen.findByText("Alice Chen");
    await user.click(screen.getByRole("button", { name: /Not paid yet/ }));
    expect(router.state.location.search).toMatchObject({ status: "pending" });
    expect(api.lastQuery("/api/plugins/admin.payment/orders")).toMatchObject({ status: "pending" });
  });

  it("shows an order's details with its coupon (zh-CN)", async () => {
    renderPage(<OrdersPage />, { locale: "zh-CN", routes: routes(), me: billingAdmin, path: "/billing/orders?id=ord_1" });
    const drawer = await screen.findByRole("dialog");
    expect(await within(drawer).findByText("SPRING20")).toBeInTheDocument();
    expect(within(drawer).getByText("cs_test_1")).toBeInTheDocument();
    expectPlainWords(drawer);
  });
});
