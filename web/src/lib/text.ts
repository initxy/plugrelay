/** A list typed one per line or comma-separated: trimmed, empty entries dropped. */
export function splitList(s: string): string[] {
  return s
    .split(/[\n,]/)
    .map((x) => x.trim())
    .filter(Boolean);
}

/** "Name: value" lines to a header map; lines without a name before a colon are ignored. */
export function parseHeaders(s: string): Record<string, string> {
  const out: Record<string, string> = {};
  for (const line of s.split("\n")) {
    const i = line.indexOf(":");
    if (i > 0) out[line.slice(0, i).trim()] = line.slice(i + 1).trim();
  }
  return out;
}
