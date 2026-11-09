import { useQuery } from "@tanstack/react-query";
import { useFeatureOn } from "@/app/session";
import { Money } from "@/components/money";
import { Badge } from "@/components/ui/badge";
import { useI18n } from "@/i18n";
import { balanceApi } from "../api";
import { payersQuery } from "../shared";

/** The caller's available balance next to the keys page title, while billing is on and one account pays. */
export function BalanceBadge() {
  const { t } = useI18n();
  const on = useFeatureOn("billing");
  const payers = useQuery({ ...payersQuery, enabled: on });
  // When keys pay, each key has its own balance: no one figure to show.
  const keysPay = payers.data?.payer === "key";
  const q = useQuery({
    queryKey: ["billing", "me", ""],
    queryFn: () => balanceApi.me(),
    enabled: on && (payers.isSuccess || payers.isError) && !keysPay,
  });
  if (!on || keysPay || !q.data) return null;
  return (
    <Badge tone={q.data.available.micro > 0 ? "outline" : "bad"}>
      {t("billing.my.balance")}: <Money value={q.data.available} />
    </Badge>
  );
}
