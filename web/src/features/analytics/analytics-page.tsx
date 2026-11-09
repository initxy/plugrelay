import { PageHeader } from "@/components/page";
import { TimeRangePicker } from "@/app/time-range";
import { useI18n } from "@/i18n";
import { ClientSpendPanel } from "./client-spend";
import { FailuresPanel } from "./failures";
import { HeatmapPanel } from "./heatmap";
import { TopPanel } from "./top-table";

/** Usage analytics (operators): busy hours, failures by cause, who uses what, spend by client. */
export function AnalyticsPage() {
  const { t } = useI18n();
  return (
    <>
      <PageHeader title={t("nav.analytics")} description={t("analytics.description")} actions={<TimeRangePicker />} />
      <div className="grid gap-4">
        <div className="grid gap-4 xl:grid-cols-[minmax(0,1.4fr)_minmax(0,1fr)]">
          <HeatmapPanel />
          <ClientSpendPanel linkToAnalytics={false} />
        </div>
        <FailuresPanel />
        <TopPanel />
      </div>
    </>
  );
}
