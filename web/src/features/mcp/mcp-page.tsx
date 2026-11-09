import { usePageOn } from "@/app/session";
import { FeatureOff } from "@/features/features/feature-off";
import { useI18n } from "@/i18n";
import { McpSettings } from "./mcp-settings";

export function McpPage() {
  const { t } = useI18n();
  const on = usePageOn("mcp.servers");
  if (!on) return <FeatureOff title={t("nav.mcp")} feature="mcp" />;
  return <McpSettings />;
}
