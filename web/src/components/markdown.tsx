import { Fragment, type ReactNode } from "react";
import { cn } from "@/lib/cn";

/*
 * A small Markdown subset rendered to React elements (never to HTML strings), for text operators
 * write: paragraphs, line breaks, # headings, - / 1. lists, **bold**, *italic*, `code` and
 * [links](https://…). Links open in a new tab and only http(s) and mailto are linked.
 */

const INLINE = /(\*\*[^*]+\*\*|\*[^*\s][^*]*\*|`[^`]+`|\[[^\]]+\]\([^)\s]+\))/g;

function safeHref(href: string): string | undefined {
  return /^(https?:|mailto:)/i.test(href) ? href : undefined;
}

function inline(text: string, key: string): ReactNode[] {
  const out: ReactNode[] = [];
  let last = 0;
  let i = 0;
  for (const m of text.matchAll(INLINE)) {
    const at = m.index ?? 0;
    if (at > last) out.push(text.slice(last, at));
    const tok = m[0];
    const k = `${key}-${i++}`;
    if (tok.startsWith("**")) out.push(<strong key={k}>{tok.slice(2, -2)}</strong>);
    else if (tok.startsWith("`")) out.push(<code key={k} className="rounded bg-surface-2 px-1 font-mono text-[0.92em]">{tok.slice(1, -1)}</code>);
    else if (tok.startsWith("[")) {
      const label = tok.slice(1, tok.indexOf("]("));
      const href = safeHref(tok.slice(tok.indexOf("](") + 2, -1));
      out.push(
        href ? (
          <a key={k} href={href} target="_blank" rel="noreferrer noopener" className="text-accent underline-offset-2 hover:underline">
            {label}
          </a>
        ) : (
          <Fragment key={k}>{label}</Fragment>
        ),
      );
    } else out.push(<em key={k}>{tok.slice(1, -1)}</em>);
    last = at + tok.length;
  }
  if (last < text.length) out.push(text.slice(last));
  return out;
}

function lines(text: string, key: string): ReactNode[] {
  return text.split("\n").flatMap((l, i) => (i === 0 ? inline(l, `${key}.${i}`) : [<br key={`${key}.br${i}`} />, ...inline(l, `${key}.${i}`)]));
}

/** Renders `source` (Markdown subset) as blocks. */
export function Markdown({ source, className }: { source: string; className?: string }) {
  const blocks = source.replace(/\r\n?/g, "\n").trim().split(/\n{2,}/);
  return (
    <div className={cn("grid gap-2 text-sm leading-relaxed", className)}>
      {blocks.map((b, i) => {
        const key = String(i);
        const rows = b.split("\n");
        const heading = /^(#{1,3})\s+(.*)$/.exec(b);
        if (heading && rows.length === 1)
          return (
            <p key={key} className={cn("font-semibold", heading[1]!.length === 1 ? "text-base" : "text-sm")}>
              {inline(heading[2]!, key)}
            </p>
          );
        if (rows.every((r) => /^\s*[-*]\s+/.test(r)))
          return (
            <ul key={key} className="list-disc space-y-0.5 pl-5">
              {rows.map((r, j) => (
                <li key={j}>{inline(r.replace(/^\s*[-*]\s+/, ""), `${key}.${j}`)}</li>
              ))}
            </ul>
          );
        if (rows.every((r) => /^\s*\d+[.)]\s+/.test(r)))
          return (
            <ol key={key} className="list-decimal space-y-0.5 pl-5">
              {rows.map((r, j) => (
                <li key={j}>{inline(r.replace(/^\s*\d+[.)]\s+/, ""), `${key}.${j}`)}</li>
              ))}
            </ol>
          );
        return <p key={key}>{lines(b, key)}</p>;
      })}
    </div>
  );
}
