/** A non-empty string search param, else undefined. */
export const str = (v: unknown): string | undefined => (typeof v === "string" && v !== "" ? v : undefined);

/** `?id=` of the open drawer. */
export const idSearch = (s: Record<string, unknown>): { id?: string } => {
  const id = str(s.id);
  return id ? { id } : {};
};

/** Filters of the models-and-prices list, shared by the signed-in catalog and the public page. */
export const catalogSearch = (s: Record<string, unknown>): { q?: string; maker?: string; reasoning?: true } => ({
  ...(str(s.q) ? { q: str(s.q) } : {}),
  ...(str(s.maker) ? { maker: str(s.maker) } : {}),
  ...(s.reasoning === true || s.reasoning === "true" ? { reasoning: true as const } : {}),
});
