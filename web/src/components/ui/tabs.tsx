import * as T from "@radix-ui/react-tabs";
import type { ReactNode } from "react";
import { cn } from "@/lib/cn";

export const Tabs = T.Root;
export const TabsContent = T.Content;

export function TabsList({ children, className }: { children: ReactNode; className?: string }) {
  return (
    <T.List className={cn("flex gap-5 overflow-x-auto border-b border-line px-5", className)}>{children}</T.List>
  );
}

export function TabsTrigger({ value, children }: { value: string; children: ReactNode }) {
  return (
    <T.Trigger
      value={value}
      className="-mb-px whitespace-nowrap border-b-2 border-transparent py-2 text-sm text-ink-3 hover:text-ink data-[state=active]:border-accent data-[state=active]:font-medium data-[state=active]:text-ink"
    >
      {children}
    </T.Trigger>
  );
}
