import type { Group, PricingRule } from "@/lib/api/types";

/** Go's path.Match for model names: `*` any run, `?` one character, `[...]` a class. */
export function globMatch(pattern: string, name: string): boolean {
  if (!/[*?[]/.test(pattern)) return pattern === name;
  const re = pattern.replace(/[.+^${}()|\\]/g, "\\$&").replace(/\*/g, ".*").replace(/\?/g, ".");
  try {
    return new RegExp(`^${re}$`).test(name);
  } catch {
    return false;
  }
}

/** The rule that prices a model: an exact match wins over a pattern, a longer pattern over a shorter one. */
export function priceFor(rules: readonly PricingRule[], model: string): PricingRule | undefined {
  const exact = rules.find((r) => r.match === model);
  if (exact) return exact;
  return rules.filter((r) => globMatch(r.match, model)).sort((a, b) => b.match.length - a.match.length)[0];
}

/** Whether a group may use a model; a group without a model list may use every model. */
export function groupAllows(g: Group, model: string): boolean {
  return g.models.length === 0 || g.models.some((p) => globMatch(p, model));
}
