import type { ReactNode } from "react";

export function Kbd({ children }: { children: ReactNode }) {
  return (
    <kbd className="rounded border border-line-strong bg-surface px-1 font-sans text-[11px] text-ink-3">
      {children}
    </kbd>
  );
}
