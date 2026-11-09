import { Link } from "@tanstack/react-router";
import { EmptyState } from "@/components/page";
import { Button } from "@/components/ui/button";
import { useI18n } from "@/i18n";

export function NotFound() {
  const { t } = useI18n();
  return (
    <EmptyState
      title={t("notFound.title")}
      hint={t("notFound.hint")}
      action={
        <Button asChild>
          <Link to="/">{t("notFound.home")}</Link>
        </Button>
      }
    />
  );
}
