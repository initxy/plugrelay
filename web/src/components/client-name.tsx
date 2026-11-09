import { cn } from "@/lib/cn";
import { useI18n } from "@/i18n";

/** Steady colors for the clients people see most; everything else is slate. */
const COLORS: Record<string, string> = {
  "claude-code": "var(--chart-1)",
  codex: "var(--chart-2)",
  cursor: "var(--chart-3)",
  "gemini-cli": "var(--chart-5)",
};

export function clientColor(id: string): string {
  return COLORS[id] ?? "var(--chart-4)";
}

/**
 * A client's display name in the console's language. An id the dictionary doesn't know yet (a
 * newer relay) shows the relay's English name; the relay sends none for clients it doesn't recognise.
 */
export function useClientLabel(): (c: { id: string; name?: string }) => string {
  const { t, has } = useI18n();
  return (c) => {
    const key = `client.${c.id}`;
    return has(key) ? t(key) : c.name || t("client.other");
  };
}

/** A client as a colored dot and its name, optionally with its version. */
export function ClientName({
  client,
  showVersion,
  className,
}: {
  client: { id: string; name?: string; version?: string };
  showVersion?: boolean;
  className?: string;
}) {
  const label = useClientLabel();
  return (
    <span className={cn("inline-flex min-w-0 items-center gap-1.5", className)}>
      <i aria-hidden className="size-2 shrink-0 rounded-full" style={{ background: clientColor(client.id) }} />
      <span className="truncate">{label(client)}</span>
      {showVersion && client.version && <span className="shrink-0 tabular-nums text-ink-3">{client.version}</span>}
    </span>
  );
}
