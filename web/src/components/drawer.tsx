import * as D from "@radix-ui/react-dialog";
import { X } from "lucide-react";
import type { ReactNode } from "react";
import { cn } from "@/lib/cn";
import { useI18n } from "@/i18n";

const WIDTH = { sm: "max-w-[420px]", md: "max-w-[560px]", lg: "max-w-[680px]", xl: "max-w-[860px]" } as const;

export interface DrawerProps {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  title: ReactNode;
  /** One line under the title; also the accessible description. */
  subtitle?: ReactNode;
  /** Small things beside the title: a status pill, a badge. */
  meta?: ReactNode;
  /** Buttons in the header, left of the close button. */
  actions?: ReactNode;
  /** A strip under the header, typically `TabsList`. */
  tabs?: ReactNode;
  footer?: ReactNode;
  size?: keyof typeof WIDTH;
  children: ReactNode;
  className?: string;
  /** Called instead of closing on Esc / outside click / ✕, e.g. to ask about unsaved edits. */
  onRequestClose?: () => void;
}

/**
 * Right-side panel for details and edit forms, over the list it came from. Pair it with
 * `useDrawerParam` so the open item lives in the URL.
 */
export function Drawer({
  open,
  onOpenChange,
  title,
  subtitle,
  meta,
  actions,
  tabs,
  footer,
  size = "md",
  children,
  className,
  onRequestClose,
}: DrawerProps) {
  const { t } = useI18n();
  const guard = (e: Event) => {
    if (onRequestClose) {
      e.preventDefault();
      onRequestClose();
    }
  };
  return (
    <D.Root open={open} onOpenChange={onOpenChange}>
      <D.Portal>
        <D.Overlay className="fixed inset-0 z-40 bg-scrim/50 data-[state=closed]:animate-[fade-out_120ms_ease-in] data-[state=open]:animate-[fade-in_140ms_ease-out]" />
        <D.Content
          onOpenAutoFocus={(e) => {
            e.preventDefault();
            (e.currentTarget as HTMLElement | null)?.focus();
          }}
          onEscapeKeyDown={guard}
          onPointerDownOutside={guard}
          tabIndex={-1}
          className={cn(
            "fixed inset-y-0 right-0 z-50 flex w-full flex-col border-l border-line bg-surface shadow-drawer outline-none",
            "data-[state=closed]:animate-[drawer-out_140ms_ease-in] data-[state=open]:animate-[drawer-in_200ms_var(--ease-out-quart)]",
            WIDTH[size],
            className,
          )}
        >
          <header className={cn("flex items-start gap-3 px-5 pt-4", tabs ? "pb-0" : "border-b border-line pb-3.5")}>
            <div className="min-w-0 flex-1">
              <div className="flex min-w-0 items-center gap-2">
                <D.Title className="truncate text-lg font-semibold tracking-[-0.005em]">{title}</D.Title>
                {meta}
              </div>
              <D.Description asChild>
                <div className={subtitle ? "mt-0.5 truncate text-xs text-ink-3" : "sr-only"}>{subtitle ?? title}</div>
              </D.Description>
            </div>
            <div className="-mr-1.5 flex shrink-0 items-center gap-1">
              {actions}
              <D.Close
                className="grid size-7 place-items-center rounded-md text-ink-3 hover:bg-surface-2 hover:text-ink"
                aria-label={t("common.close")}
                onClick={(e) => {
                  if (onRequestClose) {
                    e.preventDefault();
                    onRequestClose();
                  }
                }}
              >
                <X className="size-4" />
              </D.Close>
            </div>
          </header>
          {tabs && <div className="mt-2.5 border-b border-line">{tabs}</div>}
          <div className="min-h-0 flex-1 overflow-y-auto overscroll-contain">{children}</div>
          {footer && <footer className="flex items-center justify-end gap-2 border-t border-line bg-surface px-5 py-3">{footer}</footer>}
        </D.Content>
      </D.Portal>
    </D.Root>
  );
}

/** A label / value list for drawer overviews. */
export function DetailList({ items, className }: { items: { label: ReactNode; value: ReactNode }[]; className?: string }) {
  return (
    <dl className={cn("grid grid-cols-[minmax(84px,max-content)_1fr] gap-x-4 gap-y-2 text-sm", className)}>
      {items.map((it, i) => (
        <div key={i} className="contents">
          <dt className="text-ink-3">{it.label}</dt>
          <dd className="min-w-0 break-words">{it.value}</dd>
        </div>
      ))}
    </dl>
  );
}
