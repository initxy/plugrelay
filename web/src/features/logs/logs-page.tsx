import { CopyButton } from "@/components/copy-button";
import { PageHeader } from "@/components/page";
import { isOperator } from "@/app/nav";
import { useMe } from "@/app/session";
import { useI18n } from "@/i18n";
import { LogActions, RequestLog } from "./request-log";

export function LogsPage() {
  const { t } = useI18n();
  const admin = isOperator(useMe());
  return (
    <>
      <PageHeader
        title={t("nav.logs")}
        description={admin ? t("logs.description") : t("logs.descriptionMine")}
        actions={
          <>
            <LogActions />
            <CopyButton text={typeof location !== "undefined" ? location.href : ""} label={t("logs.copyLink")} />
          </>
        }
      />
      <RequestLog admin={admin} tableHeight="calc(100dvh - 20rem)" />
    </>
  );
}
