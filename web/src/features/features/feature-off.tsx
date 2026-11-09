import { Link } from "@tanstack/react-router";
import { PowerOff } from "lucide-react";
import { EmptyState, PageHeader } from "@/components/page";
import { Button } from "@/components/ui/button";
import { useI18n } from "@/i18n";

/** What a feature's page shows while the feature is off: the page's name and the way to turn it on. */
export function FeatureOff({ title, feature }: { title: string; feature: string }) {
  const { t } = useI18n();
  return (
    <>
      <PageHeader title={title} />
      <div className="rounded-lg border border-dashed border-line-strong bg-surface">
        <EmptyState
          icon={PowerOff}
          title={t("features.pageOff")}
          hint={t("features.pageOffHint")}
          action={
            <Button asChild variant="primary">
              <Link to="/features" search={{ id: feature }}>
                {t("features.pageOffAction")}
              </Link>
            </Button>
          }
        />
      </div>
    </>
  );
}
