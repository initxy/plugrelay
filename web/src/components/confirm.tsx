import * as A from "@radix-ui/react-alert-dialog";
import { AlertTriangle } from "lucide-react";
import { createContext, useCallback, useContext, useRef, useState, type ReactNode } from "react";
import { Button } from "@/components/ui/button";
import { useI18n } from "@/i18n";

interface ConfirmOptions {
  title: string;
  body?: ReactNode;
  confirm?: string;
  danger?: boolean;
}

type ConfirmFn = (o: ConfirmOptions) => Promise<boolean>;
const ConfirmContext = createContext<ConfirmFn | null>(null);

/** Promise-based confirm dialog, for destructive actions and discarding unsaved edits. */
export function ConfirmProvider({ children }: { children: ReactNode }) {
  const { t } = useI18n();
  const [opts, setOpts] = useState<ConfirmOptions | null>(null);
  const resolver = useRef<(v: boolean) => void>(undefined);

  const confirm = useCallback<ConfirmFn>(
    (o) =>
      new Promise<boolean>((resolve) => {
        resolver.current = resolve;
        setOpts(o);
      }),
    [],
  );
  const close = (v: boolean) => {
    resolver.current?.(v);
    resolver.current = undefined;
    setOpts(null);
  };

  return (
    <ConfirmContext.Provider value={confirm}>
      {children}
      <A.Root open={opts !== null} onOpenChange={(o) => !o && close(false)}>
        <A.Portal>
          <A.Overlay className="fixed inset-0 z-[60] bg-scrim animate-[fade-in_120ms_ease-out]" />
          <A.Content className="fixed left-1/2 top-[18vh] z-[61] w-[calc(100vw-24px)] max-w-[400px] -translate-x-1/2 rounded-xl border border-line bg-surface p-5 shadow-pop animate-[pop-in_140ms_var(--ease-out-quart)]">
            <div className="flex gap-3.5">
              {opts?.danger && (
                <span className="grid size-9 shrink-0 place-items-center rounded-full bg-bad-soft text-bad">
                  <AlertTriangle className="size-[18px]" />
                </span>
              )}
              <div className="min-w-0 flex-1">
                <A.Title className="text-lg font-semibold">{opts?.title}</A.Title>
                <A.Description asChild>
                  <div className="mt-1 text-sm text-ink-2">{opts?.body}</div>
                </A.Description>
              </div>
            </div>
            <div className="mt-5 flex justify-end gap-2">
              <A.Cancel asChild>
                <Button>{t("common.cancel")}</Button>
              </A.Cancel>
              <A.Action asChild>
                <Button variant={opts?.danger ? "danger-solid" : "primary"} onClick={() => close(true)}>
                  {opts?.confirm ?? t("common.confirm")}
                </Button>
              </A.Action>
            </div>
          </A.Content>
        </A.Portal>
      </A.Root>
    </ConfirmContext.Provider>
  );
}

export function useConfirm(): ConfirmFn {
  const ctx = useContext(ConfirmContext);
  if (!ctx) throw new Error("useConfirm outside ConfirmProvider");
  return ctx;
}
