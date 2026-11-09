import { screen, waitFor, within } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { translate } from "@/i18n";
import { renderPage, reply, type ApiRequest, type ApiRoutes } from "@/test/api";
import { expectPlainWords } from "@/test/plain-words";
import type { Plan, SubView } from "../api";
import { PlansPage } from "./plans-page";
import { billingAdmin, nameRoutes } from "./test-fixtures";

const pro: Plan = {
  id: "pl1",
  name: "Pro",
  description: "USD 30 of usage a month",
  price_micro: 20e6,
  currency: "USD",
  period: "month",
  allowance_unit: "money",
  allowance: 30e6,
  overage: "balance",
  group_id: "g_vip",
  created_at: "2026-11-01T00:00:00Z",
};
const starter: Plan = { ...pro, id: "pl2", name: "Starter", description: undefined, price_micro: 5e6, period: "week", allowance_unit: "tokens", allowance: 5_000_000, overage: "block", group_id: undefined, disabled: true };

const sub: SubView = {
  id: "s1",
  user_id: "u_alice",
  payer: "user:u_alice",
  plan_id: "pl1",
  plan_name: "Pro",
  status: "active",
  current: true,
  period: "month",
  price_micro: 20e6,
  currency: "USD",
  allowance_unit: "money",
  allowance: 30e6,
  used: 12e6,
  remaining: 18e6,
  held: 0,
  overage: "balance",
  period_start: "2026-11-01T00:00:00Z",
  period_end: "2026-12-01T00:00:00Z",
  cancel_at_period_end: false,
  provider: "balance",
  renewal: "balance",
  created_at: "2026-11-01T00:00:00Z",
};
const flagged: SubView = { ...sub, id: "s2", user_id: "u_bob", status: "ended", current: false, refund_due: true, provider: "stripe" };

function routes() {
  const saved: unknown[] = [];
  let reauthed = false;
  const ended: string[] = [];
  const r: ApiRoutes = {
    ...nameRoutes,
    "GET /me/security": { totp_enabled: false, passkeys: [], recovery_codes_left: 0, required: false, has_password: true },
    "POST /me/reauth": () => {
      reauthed = true;
      return { until: "2026-11-07T09:00:00Z" };
    },
    "GET /api/plugins/admin.subscriptions/plans": { items: [pro, starter] },
    "GET /api/plugins/admin.subscriptions/config": { currency: "USD", methods: [{ provider: "balance" }] },
    "POST /api/plugins/admin.subscriptions/plans": ({ body }: ApiRequest) => {
      saved.push(body);
      return { ...pro, ...(body as object) };
    },
    "GET /api/plugins/admin.subscriptions/subscriptions": { items: [sub, flagged] },
    "POST /api/plugins/admin.subscriptions/subscriptions/{id}/end": ({ params }: ApiRequest) => {
      if (!reauthed) return reply(403, { error: { code: "reauth_required", message: "confirm" } });
      ended.push(params.id!);
      return { ...sub, status: "ended" };
    },
  };
  return { r, saved, ended };
}

describe("PlansPage", () => {
  it("lists plans and subscriptions in plain words (zh-CN)", async () => {
    renderPage(<PlansPage />, { locale: "zh-CN", routes: routes().r, me: billingAdmin });
    expect(await screen.findByText("Starter")).toBeInTheDocument();
    expect(await screen.findByText(translate("zh-CN", "billing.op.sub.refundDue"))).toBeInTheDocument();
    await waitFor(() => expect(screen.getAllByText("vip").length).toBeGreaterThan(0));
    expectPlainWords();
  });

  it("saves a token plan", async () => {
    const { r, saved } = routes();
    const { user } = renderPage(<PlansPage />, { routes: r, me: billingAdmin });
    await screen.findByText("Starter");
    await user.click(screen.getByRole("button", { name: "New plan" }));
    const drawer = await screen.findByRole("dialog");
    await user.type(within(drawer).getByLabelText("Name"), "Team");
    await user.type(within(drawer).getByLabelText("Price"), "50");
    await user.click(within(drawer).getByRole("radio", { name: "Tokens" }));
    await user.type(within(drawer).getByLabelText("Tokens per period"), "20000000");
    await user.click(within(drawer).getByRole("button", { name: "Save" }));
    await waitFor(() => expect(saved).toHaveLength(1));
    expect(saved[0]).toMatchObject({ name: "Team", price_micro: 50e6, period: "month", allowance_unit: "tokens", allowance: 20_000_000, overage: "block" });
  });

  it("ends a subscription after confirming it's you", async () => {
    const { r, ended } = routes();
    const { user } = renderPage(<PlansPage />, { routes: r, me: billingAdmin });
    await screen.findByText("Starter");
    const row = (await screen.findAllByText("Alice Chen"))[0]!.closest("tr")!;
    await user.click(within(row).getByRole("button", { name: "More actions" }));
    await user.click(await screen.findByRole("menuitem", { name: "End now" }));
    await user.click(await screen.findByRole("button", { name: "End now" }));
    const reauth = await screen.findByRole("dialog", { name: "Confirm it's you" });
    await user.type(within(reauth).getByLabelText("Password"), "correct-horse-1");
    await user.click(within(reauth).getByRole("button", { name: "Confirm" }));
    await waitFor(() => expect(ended).toEqual(["s1"]));
  });

  it("shows the plan drawer in plain words (zh-CN)", async () => {
    renderPage(<PlansPage />, { locale: "zh-CN", routes: routes().r, me: billingAdmin, path: "/billing/plans?id=pl1" });
    const drawer = await screen.findByRole("dialog");
    expect(within(drawer).getByDisplayValue("Pro")).toBeInTheDocument();
    expectPlainWords(drawer);
  });
});
