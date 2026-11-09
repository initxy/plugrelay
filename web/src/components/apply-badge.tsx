import { Badge } from "@/components/ui/badge";
import { useI18n } from "@/i18n";

/** How a settings change takes effect: immediately, after a restart, or never here (from relay.yaml). */
export function ApplyBadge({ apply }: { apply: "live" | "restart" | "file" | undefined }) {
  const { t } = useI18n();
  if (!apply) return null;
  if (apply === "live") return <Badge tone="ok">{t("apply.live")}</Badge>;
  if (apply === "restart") return <Badge tone="warn">{t("apply.restart")}</Badge>;
  return <Badge tone="outline">{t("apply.file")}</Badge>;
}
