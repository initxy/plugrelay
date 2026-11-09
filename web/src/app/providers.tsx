import { QueryClientProvider, type QueryClient } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { Toaster } from "sonner";
import { ConfirmProvider } from "@/components/confirm";
import { TooltipProvider } from "@/components/ui/tooltip";
import { ThemeProvider, useTheme } from "@/lib/theme";
import { I18nProvider } from "@/i18n";
import { TimeRangeProvider } from "./time-range";

function ThemedToaster() {
  const { dark } = useTheme();
  return (
    <Toaster
      theme={dark ? "dark" : "light"}
      position="bottom-right"
      toastOptions={{ classNames: { toast: "!rounded-md !border-line !bg-surface !text-ink !text-[13px]" } }}
    />
  );
}

export function Providers({ queryClient, children }: { queryClient: QueryClient; children: ReactNode }) {
  return (
    <ThemeProvider>
      <I18nProvider>
        <QueryClientProvider client={queryClient}>
          <TooltipProvider delayDuration={300}>
            <ConfirmProvider>
              <TimeRangeProvider>{children}</TimeRangeProvider>
            </ConfirmProvider>
          </TooltipProvider>
          <ThemedToaster />
        </QueryClientProvider>
      </I18nProvider>
    </ThemeProvider>
  );
}
