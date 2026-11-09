import * as D from "@radix-ui/react-dialog";
import { X } from "lucide-react";
import type { ReactNode } from "react";
import { cn } from "@/lib/cn";
import { useI18n } from "@/i18n";

export const Dialog = D.Root;

interface ContentProps {
  title: ReactNode;
  description?: ReactNode;
  children: ReactNode;
  footer?: ReactNode;
  className?: string;
  /** Called instead of closing when the user presses Esc / clicks outside; return false to keep open. */
  onRequestClose?: () => void;
}

export function DialogContent({ title, description, children, footer, className, onRequestClose }: ContentProps) {
  const { t } = useI18n();
  const guard = (e: Event) => {
    if (onRequestClose) {
      e.preventDefault();
      onRequestClose();
    }
  };
  return (
    <D.Portal>
      <D.Overlay className="fixed inset-0 z-40 bg-scrim animate-[fade-in_120ms_ease-out]" />
      <D.Content
        onEscapeKeyDown={guard}
        onPointerDownOutside={guard}
        className={cn(
          "fixed left-1/2 top-[8vh] z-50 flex max-h-[84vh] w-[calc(100vw-24px)] max-w-lg -translate-x-1/2 flex-col rounded-xl border border-line bg-surface shadow-pop animate-[pop-in_140ms_var(--ease-out-quart)]",
          className,
        )}
      >
        <div className="flex items-start justify-between gap-4 border-b border-line px-5 py-3.5">
          <div>
            <D.Title className="text-lg font-semibold">{title}</D.Title>
            {description ? (
              <D.Description className="mt-0.5 text-sm text-ink-2">{description}</D.Description>
            ) : (
              <D.Description className="sr-only">{title}</D.Description>
            )}
          </div>
          <D.Close
            className="-mr-1.5 grid size-7 place-items-center rounded-md text-ink-3 hover:bg-surface-2 hover:text-ink"
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
        <div className="min-h-0 flex-1 overflow-y-auto px-5 py-4">{children}</div>
        {footer && <div className="flex justify-end gap-2 border-t border-line px-5 py-3">{footer}</div>}
      </D.Content>
    </D.Portal>
  );
}
