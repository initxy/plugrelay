import { useParams } from "@tanstack/react-router";
import { BillingRoute } from "./registry";

export function BillingIndex() {
  return <BillingRoute page="" />;
}

export function BillingPageRoute() {
  const { page } = useParams({ from: "/app/billing/$page" });
  return <BillingRoute page={page} />;
}
