import type { ReactNode } from "react";
import { Select } from "@/components/ui/select";
import { LOCALES, useI18n } from "@/i18n";
import { cn } from "@/lib/cn";

interface AuthBrand {
  name: string;
  logo_url?: string;
}

/**
 * The page around first-run setup, login and sign-up: the relay's name, a language switch, and
 * the content centred over a quiet backdrop.
 */
export function AuthLayout({ children, wide, brand }: { children: ReactNode; wide?: boolean; brand?: AuthBrand }) {
  const { locale, setLocale, t } = useI18n();
  const name = brand?.name || "plugrelay";
  return (
    <div className="relative isolate flex min-h-dvh flex-col overflow-hidden bg-bg">
      <div
        aria-hidden
        className="pointer-events-none absolute inset-x-0 top-0 -z-10 h-[560px] [background:radial-gradient(56rem_26rem_at_50%_-6rem,var(--accent-soft),transparent_70%)]"
      />
      <div
        aria-hidden
        className="pointer-events-none absolute inset-x-0 top-0 -z-10 h-[520px] opacity-70 [background-image:radial-gradient(var(--line-strong)_1px,transparent_1px)] [background-size:22px_22px] [mask-image:radial-gradient(ellipse_70%_60%_at_50%_0%,black,transparent)]"
      />
      <header className="flex items-center justify-between gap-4 px-5 py-4 sm:px-8 sm:py-5">
        <div className="flex min-w-0 items-center gap-2.5">
          {brand?.logo_url ? (
            <img src={brand.logo_url} alt="" className="size-7 rounded-md object-contain" />
          ) : (
            <img src="/favicon.svg" alt="" className="size-7" />
          )}
          <span className="truncate text-base font-semibold tracking-[-0.01em]">{name}</span>
        </div>
        <Select
          value={locale}
          onChange={setLocale}
          options={LOCALES.map((l) => ({ value: l.value, label: <span lang={l.value}>{l.label}</span> }))}
          aria-label={t("shell.language")}
          className="h-8 w-auto border-transparent bg-transparent px-2.5 text-xs text-ink-2 shadow-none hover:border-line hover:bg-surface"
        />
      </header>
      <main className="flex flex-1 justify-center px-4 pb-16 pt-[5vh] sm:pt-[9vh]">
        <div className={cn("w-full", wide ? "max-w-2xl" : "max-w-[400px]")}>{children}</div>
      </main>
      {brand && brand.name !== "plugrelay" && (
        <footer className="pb-6 text-center text-2xs text-ink-3">{t("login.poweredBy")}</footer>
      )}
    </div>
  );
}

/** The card the auth pages put their form in. */
export function AuthCard({ title, description, children, className }: { title: ReactNode; description?: ReactNode; children: ReactNode; className?: string }) {
  return (
    <div className={cn("rounded-xl border border-line bg-surface p-6 shadow-pop sm:p-8", className)}>
      <h1 className="text-xl font-semibold tracking-[-0.01em]">{title}</h1>
      {description && <p className="mt-1 text-sm text-ink-2">{description}</p>}
      <div className="mt-6">{children}</div>
    </div>
  );
}
