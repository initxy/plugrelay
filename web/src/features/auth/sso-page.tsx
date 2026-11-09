import { usePageOn } from "@/app/session";
import { FeatureOff } from "@/features/features/feature-off";
import { useI18n } from "@/i18n";
import { SsoSettings } from "./sso-settings";

export function SsoPage() {
  const { t } = useI18n();
  const on = usePageOn("sso.providers");
  if (!on) return <FeatureOff title={t("nav.sso")} feature="sso" />;
  return <SsoSettings />;
}
