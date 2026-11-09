import { AlertTriangle, RotateCw, type LucideIcon } from "lucide-react";
import type { ReactNode } from "react";
import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import { cn } from "@/lib/cn";
import { errorMessage } from "@/lib/api/errors";
import { useI18n } from "@/i18n";

/** Title row of a page: title (with optional pills beside it), one line of description, actions on the right. */
export function PageHeader({
  title,
  description,
  meta,
  actions,
  className,
}: {
  title: ReactNode;
  description?: ReactNode;
  meta?: ReactNode;
  actions?: ReactNode;
  className?: string;
}) {
  return (
    <div className={cn("mb-5 flex flex-wrap items-start justify-between gap-x-4 gap-y-3", className)}>
      <div className="min-w-0 flex-1">
        <div className="flex min-h-8 flex-wrap items-center gap-2">
          <h1 className="text-xl font-semibold tracking-[-0.01em]">{title}</h1>
          {meta}
        </div>
        {description && <p className="mt-0.5 max-w-[72ch] text-sm text-ink-2">{description}</p>}
      </div>
      {actions && <div className="flex flex-wrap items-center gap-2">{actions}</div>}
    </div>
  );
}

/** A bordered card with an optional title row. */
export function Panel({
  title,
  aside,
  children,
  className,
  bodyClassName,
}: {
  title?: ReactNode;
  aside?: ReactNode;
  children: ReactNode;
  className?: string;
  bodyClassName?: string;
}) {
  return (
    <section className={cn("rounded-lg border border-line bg-surface", className)}>
      {title && (
        <div className="flex min-h-10 items-center justify-between gap-2 border-b border-line-soft px-4 py-2">
          <h2 className="text-sm font-semibold">{title}</h2>
          {aside && <div className="flex items-center gap-2 text-xs text-ink-3">{aside}</div>}
        </div>
      )}
      <div className={cn("p-4", bodyClassName)}>{children}</div>
    </section>
  );
}

/** What a list shows when it has nothing: what is missing, why, and the one action that fixes it. */
export function EmptyState({
  title,
  hint,
  action,
  icon: Icon,
  compact,
  className,
}: {
  title: ReactNode;
  hint?: ReactNode;
  action?: ReactNode;
  icon?: LucideIcon;
  compact?: boolean;
  className?: string;
}) {
  return (
    <div className={cn("flex flex-col items-center px-6 text-center", compact ? "gap-1.5 py-8" : "gap-2 py-14", className)}>
      {Icon && (
        <span className="mb-1.5 grid size-10 place-items-center rounded-lg border border-line bg-surface-2 text-ink-3">
          <Icon className="size-5" strokeWidth={1.75} />
        </span>
      )}
      <p className="text-base font-medium">{title}</p>
      {hint && <p className="max-w-[52ch] text-sm text-ink-2">{hint}</p>}
      {action && <div className="mt-2.5 flex flex-wrap justify-center gap-2">{action}</div>}
    </div>
  );
}

export function ErrorState({ error, onRetry }: { error: unknown; onRetry?: () => void }) {
  const { t } = useI18n();
  return (
    <div role="alert" className="flex items-start gap-3 rounded-lg border border-bad/30 bg-bad-soft px-4 py-3">
      <AlertTriangle className="mt-0.5 size-4 shrink-0 text-bad" />
      <div className="min-w-0 flex-1">
        <p className="text-sm font-medium text-bad">{t("common.loadFailed")}</p>
        <p className="mt-0.5 break-words text-sm text-ink-2">{errorMessage(error)}</p>
      </div>
      {onRetry && (
        <Button size="sm" onClick={onRetry}>
          <RotateCw />
          {t("common.retry")}
        </Button>
      )}
    </div>
  );
}

export function LoadingRows({ rows = 6 }: { rows?: number }) {
  return (
    <div className="grid gap-2" aria-busy="true">
      {Array.from({ length: rows }, (_, i) => (
        <Skeleton key={i} className="h-9" />
      ))}
    </div>
  );
}
