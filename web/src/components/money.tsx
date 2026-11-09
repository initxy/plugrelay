import type { Money as MoneyValue } from "@/lib/api/types";
import { cn } from "@/lib/cn";
import { formatMoney } from "@/lib/money";
import { useI18n } from "@/i18n";

export function Money({ value, className, compact }: { value: MoneyValue; className?: string; compact?: boolean }) {
  const { locale } = useI18n();
  return <span className={cn("tabular-nums", className)}>{formatMoney(value, locale, { compact })}</span>;
}

export function useMoney() {
  const { locale } = useI18n();
  return (m: MoneyValue, compact?: boolean) => formatMoney(m, locale, { compact });
}
