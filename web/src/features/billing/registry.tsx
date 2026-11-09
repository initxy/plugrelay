import { lazy, Suspense, type ComponentType } from "react";
import { LoadingRows } from "@/components/page";
import { pageIdAt } from "@/app/nav";
import { usePageOn } from "@/app/session";
import { FeatureOff } from "@/features/features/feature-off";
import { useI18n } from "@/i18n";

const load = <K extends string>(name: K, f: () => Promise<Record<K, ComponentType>>) =>
  lazy(() => f().then((m) => ({ default: m[name] })));

/** Billing pages by console path under /billing. Which page id gates each one comes from the menu (app/nav.ts). */
const BILLING_PAGES: Record<string, ComponentType> = {
  "": load("WalletPage", () => import("./member/wallet-page")),
  plan: load("MyPlanPage", () => import("./member/plan-page")),
  balances: load("BalancesPage", () => import("./operator/balances-page")),
  orders: load("OrdersPage", () => import("./operator/orders-page")),
  codes: load("RedeemCodesPage", () => import("./operator/codes-page")),
  coupons: load("CouponsPage", () => import("./operator/coupons-page")),
  plans: load("PlansPage", () => import("./operator/plans-page")),
  announcements: load("AnnouncementsPage", () => import("./operator/announcements-page")),
  import: load("ImportPage", () => import("./operator/import-page")),
};

/** Renders one billing page while /me lists it; the page brings its own header. */
export function BillingRoute({ page }: { page: string }) {
  const { t } = useI18n();
  const Page = BILLING_PAGES[page];
  const path = page ? `/billing/${page}` : "/billing";
  const on = usePageOn(pageIdAt(path) ?? "");
  if (!Page || !on) return <FeatureOff title={t("billing.title")} feature="billing" />;
  return (
    <Suspense fallback={<LoadingRows />}>
      <Page />
    </Suspense>
  );
}
