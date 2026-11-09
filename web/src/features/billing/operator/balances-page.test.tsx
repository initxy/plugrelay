import { screen, waitFor, within } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { translate } from "@/i18n";
import { renderPage, type ApiRequest, type ApiRoutes } from "@/test/api";
import { expectPlainWords } from "@/test/plain-words";
import type { AccountView, TxnPage } from "../api";
import { BalancesPage } from "./balances-page";
import { billingAdmin, nameRoutes, usd } from "./test-fixtures";

const acct = (subject: string, bal: number, held = 0, credit = 0): AccountView => ({
  subject,
  balance: usd(bal),
  available: usd(bal - held),
  held: usd(held),
  credit_limit: usd(credit),
  updated_at: "2026-11-07T08:00:00Z",
});

const txns: TxnPage = {
  items: [
    {
      id: "adjust:1",
      subject: "user:u_alice",
      kind: "adjustment",
      amount: usd(50),
      balance_after: usd(50),
      amount_micro: 50e6,
      balance_after_micro: 50e6,
      note: "Opening balance",
      created_at: "2026-11-06T08:00:00Z",
    },
    {
      id: "charge:r1",
      subject: "user:u_alice",
      kind: "charge",
      amount: usd(-0.25),
      balance_after: usd(49.75),
      amount_micro: -250000,
      balance_after_micro: 49.75e6,
      request_id: "r1",
      created_at: "2026-11-07T08:00:00Z",
    },
  ],
  next_cursor: "",
};

function routes() {
  const adjusts: unknown[] = [];
  const r: ApiRoutes = {
    ...nameRoutes,
    "GET /api/plugins/meter.balance/accounts": {
      items: [acct("user:u_alice", 49.75, 1.5, 20), acct("user:u_bob", -2), acct("group:g_vip", 300), acct("key:k_ci", 5)],
      currency: "USD",
      payer: "user",
    },
    "GET /api/plugins/meter.balance/transactions": txns,
    "POST /api/plugins/meter.balance/adjust": ({ body }: ApiRequest) => {
      adjusts.push(body);
      return { applied: true, account: acct("user:u_alice", 59.75) };
    },
    "PUT /api/plugins/meter.balance/credit-limit": acct("user:u_alice", 49.75, 0, 30),
  };
  return { r, adjusts };
}

describe("BalancesPage", () => {
  it("lists accounts by name in plain words (zh-CN)", async () => {
    renderPage(<BalancesPage />, { locale: "zh-CN", routes: routes().r, me: billingAdmin });
    expect(await screen.findByText("Alice Chen")).toBeInTheDocument();
    expect(await screen.findByText("ci-runner")).toBeInTheDocument();
    expect(screen.getByText("vip")).toBeInTheDocument();
    expect(screen.getByText(translate("zh-CN", "billing.op.payer.user"))).toBeInTheDocument();
    expectPlainWords();
  });

  it("opens an account, adjusts it once per form and shows its history", async () => {
    const { r, adjusts } = routes();
    const { user } = renderPage(<BalancesPage />, { routes: r, me: billingAdmin, path: "/billing/balances?id=user%3Au_alice" });
    const drawer = await screen.findByRole("dialog");
    expect(await within(drawer).findByText("Opening balance")).toBeInTheDocument();
    expect(within(drawer).getByText("View the request")).toBeInTheDocument();
    await user.type(within(drawer).getByLabelText("Amount (USD)"), "10");
    await user.type(within(drawer).getByLabelText("Reason"), "Goodwill");
    await user.click(within(drawer).getByRole("button", { name: "Adjust balance" }));
    expect(await screen.findByText("Balance adjusted")).toBeInTheDocument();
    expect(adjusts[0]).toMatchObject({ subject: "user:u_alice", amount_micro: 10e6, note: "Goodwill" });
    expect((adjusts[0] as { idempotency_key: string }).idempotency_key).toBeTruthy();
  });

  it("shows the account drawer and the account picker in plain words (zh-CN)", async () => {
    const { user } = renderPage(<BalancesPage />, { locale: "zh-CN", routes: routes().r, me: billingAdmin, path: "/billing/balances?id=user%3Au_bob" });
    const drawer = await screen.findByRole("dialog");
    await within(drawer).findByText("Opening balance");
    expectPlainWords(drawer);
    await user.click(within(drawer).getByRole("button", { name: translate("zh-CN", "common.close") }));
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    await user.click(screen.getByRole("button", { name: translate("zh-CN", "billing.op.bal.adjust") }));
    expectPlainWords(await screen.findByRole("dialog"));
  });
});
