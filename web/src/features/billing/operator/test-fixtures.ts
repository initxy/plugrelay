import type { Group, Key, User } from "@/lib/api/types";
import { adminMe, type ApiRoutes } from "@/test/api";

/** n US dollars as Money. */
export const usd = (n: number) => ({ micro: Math.round(n * 1_000_000), currency: "USD" });

const user = (id: string, username: string, display: string): User => ({
  id,
  username,
  display_name: display,
  role: "member",
  status: "active",
  created_at: "2026-10-01T00:00:00Z",
  month_spend: usd(0),
});

const alice = user("u_alice", "alice@demo.local", "Alice Chen");
const bob = user("u_bob", "bob@demo.local", "Bob Li");
const vip: Group = { id: "g_vip", name: "vip", member_count: 1, models: ["*"] };
const ciKey: Key = {
  id: "k_ci",
  name: "ci-runner",
  last4: "abcd",
  owner: { id: "u_bob", name: "Bob Li" },
  status: "active",
  created_at: "2026-10-01T00:00:00Z",
  month_spend: usd(0),
  settings: {},
};

/** The people, groups and keys the operator pages name accounts by. */
export const nameRoutes: ApiRoutes = {
  "GET /users": { items: [alice, bob] },
  "GET /groups": { items: [vip] },
  "GET /keys": { items: [ciKey] },
};

export const billingAdmin = adminMe({
  features: ["limits", "routing", "request_log", "metrics", "billing"],
  pages: ["billing.balances", "billing.orders", "billing.codes", "billing.coupons", "billing.plans", "billing.announcements", "billing.import"],
});
