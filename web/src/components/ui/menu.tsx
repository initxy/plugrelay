import * as M from "@radix-ui/react-dropdown-menu";
import type { ReactNode } from "react";
import { cn } from "@/lib/cn";

export const Menu = M.Root;
export const MenuTrigger = M.Trigger;

export function MenuContent({ children, align = "end" }: { children: ReactNode; align?: "start" | "end" }) {
  return (
    <M.Portal>
      <M.Content
        align={align}
        sideOffset={4}
        className="z-50 min-w-48 rounded-lg border border-line bg-surface p-1 shadow-pop animate-[pop-in_100ms_var(--ease-out-quart)]"
      >
        {children}
      </M.Content>
    </M.Portal>
  );
}

export function MenuItem({
  children,
  onSelect,
  danger,
  disabled,
}: {
  children: ReactNode;
  onSelect?: () => void;
  danger?: boolean;
  disabled?: boolean;
}) {
  return (
    <M.Item
      disabled={disabled}
      onSelect={onSelect}
      className={cn(
        "flex cursor-default select-none items-center gap-2 rounded-sm px-2 py-1.5 text-sm outline-none data-[disabled]:opacity-50 data-[highlighted]:bg-surface-2 [&_svg]:size-4 [&_svg]:text-ink-3",
        danger && "text-bad [&_svg]:text-bad",
      )}
    >
      {children}
    </M.Item>
  );
}

export function MenuSeparator() {
  return <M.Separator className="my-1 h-px bg-line" />;
}

export function MenuLabel({ children }: { children: ReactNode }) {
  return <M.Label className="px-2 pb-1 pt-1.5 text-2xs font-medium text-ink-3">{children}</M.Label>;
}

export const MenuRadioGroup = M.RadioGroup;

export function MenuRadioItem({ value, children }: { value: string; children: ReactNode }) {
  return (
    <M.RadioItem
      value={value}
      className="flex cursor-default select-none items-center gap-2 rounded-sm py-1.5 pl-7 pr-2 text-sm outline-none data-[highlighted]:bg-surface-2 relative"
    >
      <M.ItemIndicator className="absolute left-2.5 size-1.5 rounded-full bg-accent" />
      {children}
    </M.RadioItem>
  );
}
