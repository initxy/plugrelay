import { useQuery } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { LogIn } from "lucide-react";
import type { ReactNode } from "react";
import { EmptyState, ErrorState } from "@/components/page";
import { Button } from "@/components/ui/button";
import { Select } from "@/components/ui/select";
import { CatalogTable } from "@/features/catalog/catalog-page";
import { StatusBoard } from "@/features/status/status-page";
import { authProvidersQuery } from "@/features/auth/sso-buttons";
import { api, unwrap } from "@/lib/api/client";
import { ApiError } from "@/lib/api/errors";
import { formatWhen } from "@/lib/format";
import { LOCALES, useI18n } from "@/i18n";

/** The page around the public pages: the relay's name, the two pages, language and sign-in. */
function PublicLayout({ title, description, children }: { title: string; description: ReactNode; children: ReactNode }) {
  const { t, locale, setLocale } = useI18n();
  const auth = useQuery(authProvidersQuery);
  const site = auth.data?.site;
  const pages = auth.data?.public_pages;
  const tab = (to: "/public/models" | "/public/status", label: string) => (
    <Link
      to={to}
      className="rounded-md px-2.5 py-1.5 text-sm text-ink-2 hover:bg-surface-2 hover:text-ink"
      activeProps={{ className: "bg-surface-2 font-medium text-ink" }}
    >
      {label}
    </Link>
  );
  return (
    <div className="min-h-dvh bg-bg">
      <header className="sticky top-0 z-10 border-b border-line bg-surface/95 backdrop-blur">
        <div className="mx-auto flex max-w-7xl flex-wrap items-center gap-x-4 gap-y-2 px-4 py-3 sm:px-6">
          <div className="flex min-w-0 items-center gap-2.5">
            <img src={site?.logo_url || "/favicon.svg"} alt="" className="size-7 rounded-md object-contain" />
            <span className="truncate text-base font-semibold tracking-[-0.01em]">{site?.name ?? "plugrelay"}</span>
          </div>
          <nav className="flex items-center gap-1" aria-label={t("public.nav")}>
            {tab("/public/models", t("public.models"))}
            {pages?.status !== false && tab("/public/status", t("public.status"))}
          </nav>
          <div className="ml-auto flex items-center gap-2">
            <Select
              value={locale}
              onChange={setLocale}
              options={LOCALES.map((l) => ({ value: l.value, label: <span lang={l.value}>{l.label}</span> }))}
              aria-label={t("shell.language")}
              className="h-8 w-auto border-transparent bg-transparent px-2.5 text-xs text-ink-2 shadow-none hover:border-line hover:bg-surface"
            />
            <Button size="sm" asChild>
              <Link to="/login">
                <LogIn />
                {t("public.signIn")}
              </Link>
            </Button>
          </div>
        </div>
      </header>
      <main className="mx-auto max-w-7xl px-4 py-6 sm:px-6">
        <h1 className="text-xl font-semibold tracking-[-0.01em]">{title}</h1>
        <p className="mb-5 mt-0.5 max-w-[72ch] text-sm text-ink-2">{description}</p>
        {children}
      </main>
    </div>
  );
}

function Failed({ error, onRetry }: { error: unknown; onRetry: () => void }) {
  const { t } = useI18n();
  if (error instanceof ApiError && error.code === "public_pages_off")
    return <EmptyState title={t("public.off")} hint={t("public.offHint")} action={<Button asChild><Link to="/login">{t("public.signIn")}</Link></Button>} />;
  return <ErrorState error={error} onRetry={onRetry} />;
}

/** Models and prices anyone may see, while the public pages feature is on. */
export function PublicModelsPage() {
  const { t } = useI18n();
  const q = useQuery({ queryKey: ["public", "catalog"], queryFn: () => unwrap(api.GET("/public/catalog")), retry: false });
  return (
    <PublicLayout title={t("public.models")} description={t("public.modelsHint")}>
      {q.isError ? <Failed error={q.error} onRetry={() => void q.refetch()} /> : <CatalogTable items={q.data?.items} currency={q.data?.currency ?? "USD"} />}
    </PublicLayout>
  );
}

/** Whether each public model is answering, while the public pages feature is on. */
export function PublicStatusPage() {
  const { t, locale } = useI18n();
  const q = useQuery({ queryKey: ["public", "status"], queryFn: () => unwrap(api.GET("/public/status")), retry: false, refetchInterval: 30_000 });
  return (
    <PublicLayout
      title={t("public.status")}
      description={
        <>
          {t("public.statusHint")}
          {q.data && <span className="ml-2 text-ink-3">{t("status.updated", { at: formatWhen(q.data.updated_at, locale) })}</span>}
        </>
      }
    >
      {q.isError ? <Failed error={q.error} onRetry={() => void q.refetch()} /> : <StatusBoard q={q} />}
    </PublicLayout>
  );
}
