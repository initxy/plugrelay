import * as P from "@radix-ui/react-popover";
import type { ReactNode } from "react";
import { cn } from "@/lib/cn";

export const Popover = P.Root;
export const PopoverTrigger = P.Trigger;

export function PopoverContent({
  children,
  className,
  align = "start",
}: {
  children: ReactNode;
  className?: string;
  align?: "start" | "end" | "center";
}) {
  return (
    <P.Portal>
      <P.Content
        align={align}
        sideOffset={4}
        className={cn(
          "z-50 w-72 rounded-lg border border-line bg-surface p-3 shadow-pop animate-[pop-in_100ms_ease-out]",
          className,
        )}
      >
        {children}
      </P.Content>
    </P.Portal>
  );
}
