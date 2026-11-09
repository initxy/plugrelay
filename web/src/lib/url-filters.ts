import type { Cause, Tier } from "./api/types";

/** Request-log filters. Every field maps 1:1 to a `/logs` query parameter and to the console URL. */
export interface LogFilters {
  from?: string;
  to?: string;
  key_id?: string;
  user_id?: string;
  model?: string;
  upstream_id?: string;
  /** Client id (`/clients`), e.g. claude-code or other. */
  client?: string;
  status?: "ok" | "error";
  tier?: Tier;
  cause?: Cause;
  retried?: boolean;
  session_id?: string;
  q?: string;
}

const STRING_KEYS = ["from", "to", "key_id", "user_id", "model", "upstream_id", "client", "session_id", "q"] as const;
const TIERS: readonly Tier[] = ["passthrough", "patch", "convert"];
const CAUSES: readonly Cause[] = [
  "request",
  "transient",
  "rate_limit",
  "quota",
  "auth_expired",
  "banned",
  "content",
  "unknown",
];

const pick = <T extends string>(v: unknown, allowed: readonly T[]): T | undefined =>
  typeof v === "string" && (allowed as readonly string[]).includes(v) ? (v as T) : undefined;

/** Accepts untrusted search params (from the URL) and keeps only valid filters. */
export function parseLogFilters(search: Record<string, unknown>): LogFilters {
  const out: LogFilters = {};
  for (const k of STRING_KEYS) {
    const v = search[k];
    if (typeof v === "string" && v.trim() !== "") out[k] = v.trim();
  }
  const status = pick(search.status, ["ok", "error"] as const);
  if (status) out.status = status;
  const tier = pick(search.tier, TIERS);
  if (tier) out.tier = tier;
  const cause = pick(search.cause, CAUSES);
  if (cause) out.cause = cause;
  if (search.retried === true || search.retried === "true" || search.retried === "1") out.retried = true;
  return out;
}

/** How the request log lists: one row per request (default) or per session (`view=sessions`). */
export type LogView = "requests" | "sessions";

export function parseLogView(search: Record<string, unknown>): { view?: "sessions" } {
  return search.view === "sessions" ? { view: "sessions" } : {};
}

/** Filters → flat string map, keys sorted, empty values dropped. Used for URLs and API query strings. */
export function serializeLogFilters(f: LogFilters): Record<string, string> {
  const out: Record<string, string> = {};
  for (const [k, v] of Object.entries(f).sort(([a], [b]) => a.localeCompare(b))) {
    if (v === undefined || v === "" || v === false) continue;
    out[k] = String(v);
  }
  return out;
}

export function countActiveFilters(f: LogFilters): number {
  return Object.keys(serializeLogFilters(f)).filter((k) => k !== "from" && k !== "to").length;
}

export type SearchValue = string | number | boolean | undefined;

/** Drop empty search values (undefined, "", false, NaN) so cleared filters leave the URL. */
export function cleanSearch(search: Record<string, unknown>): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  for (const [k, v] of Object.entries(search)) {
    if (v === undefined || v === null || v === "" || v === false || (typeof v === "number" && Number.isNaN(v))) continue;
    out[k] = v;
  }
  return out;
}

/** One allowed value from an untrusted search param, else undefined. */
export function pickEnum<T extends string>(v: unknown, allowed: readonly T[]): T | undefined {
  return pick(v, allowed);
}

/** A non-empty trimmed string from an untrusted search param. */
export function pickString(v: unknown): string | undefined {
  return typeof v === "string" && v.trim() !== "" ? v.trim() : undefined;
}

/** `true` for true / "true" / "1", else undefined (so the param is dropped). */
export function pickFlag(v: unknown): true | undefined {
  return v === true || v === "true" || v === "1" || v === 1 ? true : undefined;
}
