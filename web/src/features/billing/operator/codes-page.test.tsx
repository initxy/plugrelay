import { screen, waitFor, within } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { translate } from "@/i18n";
import { renderPage, type ApiRequest, type ApiRoutes } from "@/test/api";
import { expectPlainWords } from "@/test/plain-words";
import type { RedeemBatch, RedeemCode } from "../api";
import { RedeemCodesPage } from "./codes-page";
import { billingAdmin, nameRoutes, usd } from "./test-fixtures";

const launch: RedeemBatch = {
  batch: "launch-week",
  codes: 2,
  amount: usd(10),
  max_uses: 2,
  uses: 1,
  disabled: 0,
  expired: 0,
  created_at: "2026-11-01T00:00:00Z",
  redeemed: usd(10),
};
const codes: RedeemCode[] = [
  {
    id: "c1",
    last4: "7KQ2",
    batch: "launch-week",
    amount_micro: 10e6,
    amount: usd(10),
    max_uses: 1,
    uses: 1,
    created_at: "2026-11-01T00:00:00Z",
    redeemed: [{ subject: "user:u_bob", created_at: "2026-11-02T00:00:00Z" }],
  },
  { id: "c2", last4: "M9XZ", batch: "launch-week", amount_micro: 10e6, amount: usd(10), max_uses: 1, uses: 0, created_at: "2026-11-01T00:00:00Z" },
];

function routes() {
  const generated: unknown[] = [];
  const disabled: unknown[] = [];
  const r: ApiRoutes = {
    ...nameRoutes,
    "GET /api/plugins/admin.redeem/batches": { items: [launch], currency: "USD" },
    "GET /api/plugins/admin.redeem/codes": { items: codes, currency: "USD" },
    "POST /api/plugins/admin.redeem/batches": ({ body }: ApiRequest) => {
      generated.push(body);
      return { batch: "spring", codes: ["RC-AAAA-BBBB-CCCC-DDDD-EEEE", "RC-FFFF-GGGG-HHHH-JJJJ-KKKK"], amount: usd(5) };
    },
    "POST /api/plugins/admin.redeem/batches/disable": ({ body }: ApiRequest) => {
      disabled.push(body);
      return undefined;
    },
    "POST /api/plugins/admin.redeem/codes/disable": () => undefined,
  };
  return { r, generated, disabled };
}

describe("RedeemCodesPage", () => {
  it("lists batches in plain words (zh-CN)", async () => {
    renderPage(<RedeemCodesPage />, { locale: "zh-CN", routes: routes().r, me: billingAdmin });
    expect(await screen.findByText("launch-week")).toBeInTheDocument();
    expectPlainWords();
  });

  it("generates codes and shows them once", async () => {
    const { r, generated } = routes();
    const { user } = renderPage(<RedeemCodesPage />, { routes: r, me: billingAdmin });
    await screen.findByText("launch-week");
    await user.click(screen.getByRole("button", { name: "Generate codes" }));
    const drawer = await screen.findByRole("dialog");
    await user.clear(within(drawer).getByLabelText("How many"));
    await user.type(within(drawer).getByLabelText("How many"), "2");
    await user.type(within(drawer).getByLabelText("Amount each"), "5");
    await user.type(within(drawer).getByLabelText(/Batch name/), "spring");
    await user.click(within(drawer).getByRole("button", { name: "Generate codes" }));
    expect(await within(drawer).findByDisplayValue(/RC-AAAA-BBBB/)).toBeInTheDocument();
    expect(generated[0]).toMatchObject({ count: 2, amount_micro: 5e6, max_uses: 1, batch: "spring" });
    await user.click(within(drawer).getByRole("button", { name: "I've saved them" }));
    await user.click(await screen.findByRole("button", { name: "Close anyway" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
  });

  it("shows a batch with who redeemed and turns it off (zh-CN)", async () => {
    const { r, disabled } = routes();
    const { user } = renderPage(<RedeemCodesPage />, { locale: "zh-CN", routes: r, me: billingAdmin, path: "/billing/codes?id=launch-week" });
    const drawer = await screen.findByRole("dialog");
    await waitFor(() => expect(within(drawer).getByText("Bob Li")).toBeInTheDocument());
    expectPlainWords(drawer);
    await user.click(within(drawer).getByRole("button", { name: translate("zh-CN", "billing.op.codes.disableBatch") }));
    await user.click(await screen.findByRole("button", { name: translate("zh-CN", "billing.op.codes.disableBatch") }));
    await waitFor(() => expect(disabled).toEqual([{ batch: "launch-week", disabled: true }]));
  });

  it("shows the generate drawer in plain words (zh-CN)", async () => {
    const { user } = renderPage(<RedeemCodesPage />, { locale: "zh-CN", routes: routes().r, me: billingAdmin });
    await screen.findByText("launch-week");
    await user.click(screen.getByRole("button", { name: translate("zh-CN", "billing.op.codes.generate") }));
    expectPlainWords(await screen.findByRole("dialog"));
  });
});
