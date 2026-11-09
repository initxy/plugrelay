import { screen, waitFor, within } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { translate } from "@/i18n";
import { apiError, renderPage, type ApiRequest, type ApiRoutes } from "@/test/api";
import { expectPlainWords } from "@/test/plain-words";
import type { Coupon } from "../api";
import { CouponsPage } from "./coupons-page";
import { billingAdmin, nameRoutes, usd } from "./test-fixtures";

const spring: Coupon = {
  id: "cp1",
  code: "SPRING20",
  percent: 20,
  amount: usd(0),
  max_discount: usd(5),
  min_topup: usd(10),
  amount_micro: 0,
  max_discount_micro: 5e6,
  min_topup_micro: 10e6,
  max_uses: 100,
  per_user: 1,
  ends_at: "2026-12-01T00:00:00Z",
  disabled: false,
  note: "Spring promo",
  created_at: "2026-11-01T00:00:00Z",
  updated_at: "2026-11-01T00:00:00Z",
  used: 12,
  pending: 1,
  state: "active",
};

function routes(enabled = true) {
  const created: unknown[] = [];
  const r: ApiRoutes = {
    ...nameRoutes,
    "GET /api/plugins/admin.payment/coupons": { items: [spring], currency: "USD", enabled },
    "POST /api/plugins/admin.payment/coupons": ({ body }: ApiRequest) => {
      created.push(body);
      if ((body as { code: string }).code === "SPRING20") return apiError(409, "conflict", "already exists");
      return { ...spring, id: "cp2", code: (body as { code: string }).code };
    },
    "PUT /api/plugins/admin.payment/coupons/{id}": ({ body }: ApiRequest) => ({ ...spring, ...(body as object) }),
    "GET /api/plugins/admin.payment/coupons/{id}/uses": {
      items: [{ order_id: "ord_1", coupon_id: "cp1", user_id: "u_alice", discount: usd(5), status: "paid", created_at: "2026-11-05T00:00:00Z" }],
    },
  };
  return { r, created };
}

describe("CouponsPage", () => {
  it("lists coupons and says when coupons are off (zh-CN)", async () => {
    renderPage(<CouponsPage />, { locale: "zh-CN", routes: routes(false).r, me: billingAdmin });
    expect(await screen.findByText("SPRING20")).toBeInTheDocument();
    expect(screen.getByText(translate("zh-CN", "billing.op.cp.offTitle"))).toBeInTheDocument();
    expect(screen.getByText(/^减 20%，最多 .*5\.00$/)).toBeInTheDocument();
    expectPlainWords();
  });

  it("creates a fixed-amount coupon, after a taken code", async () => {
    const { r, created } = routes();
    const { user } = renderPage(<CouponsPage />, { routes: r, me: billingAdmin });
    await screen.findByText("SPRING20");
    await user.click(screen.getByRole("button", { name: "New coupon" }));
    const drawer = await screen.findByRole("dialog");
    await user.type(within(drawer).getByLabelText("Code"), "spring20");
    await user.click(within(drawer).getByRole("radio", { name: "Amount off" }));
    await user.type(within(drawer).getByLabelText("Amount off"), "3");
    await user.click(within(drawer).getByRole("button", { name: "Save" }));
    expect(await screen.findByText("A coupon with this code exists already")).toBeInTheDocument();
    const code = within(drawer).getByLabelText("Code");
    await user.clear(code);
    await user.type(code, "welcome3");
    await user.click(within(drawer).getByRole("button", { name: "Save" }));
    await waitFor(() => expect(created).toHaveLength(2));
    expect(created[1]).toMatchObject({ code: "WELCOME3", percent: 0, amount_micro: 3e6, max_uses: 0, per_user: 1 });
  });

  it("edits a coupon and lists its uses (zh-CN)", async () => {
    renderPage(<CouponsPage />, { locale: "zh-CN", routes: routes().r, me: billingAdmin, path: "/billing/coupons?id=cp1" });
    const drawer = await screen.findByRole("dialog");
    await waitFor(() => expect(within(drawer).getByText("Alice Chen")).toBeInTheDocument());
    expectPlainWords(drawer);
  });
});
