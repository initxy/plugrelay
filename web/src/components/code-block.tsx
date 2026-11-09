import { cn } from "@/lib/cn";
import { CopyButton } from "./copy-button";

export function CodeBlock({
  code,
  title,
  className,
  copy = true,
}: {
  code: string;
  title?: string;
  className?: string;
  copy?: boolean;
}) {
  return (
    <div className={cn("overflow-hidden rounded-md border border-line bg-surface-2/60", className)}>
      {(title || copy) && (
        <div className="flex items-center justify-between gap-2 border-b border-line px-3 py-1">
          <span className="truncate font-mono text-xs text-ink-3">{title}</span>
          {copy && <CopyButton text={code} size="sm" variant="ghost" />}
        </div>
      )}
      <pre className="overflow-x-auto px-3 py-2.5 font-mono text-[12.5px] leading-relaxed text-ink">
        <code>{code}</code>
      </pre>
    </div>
  );
}

export function JsonView({ value, title }: { value: unknown; title?: string }) {
  return <CodeBlock code={JSON.stringify(value, null, 2)} title={title} />;
}
