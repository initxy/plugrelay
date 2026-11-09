import type { Endpoint, Upstream, UpstreamInput } from "@/lib/api/types";
import { RESERVED_CONFIG } from "./labels";
import { parseHeaders } from "@/lib/text";
import type { useForm } from "@/lib/use-form";

type ProbeMode = "default" | "off" | "custom";

/** The upstream drawer's form: an editable copy of an upstream; `diffDraft` turns it into a patch. */
export interface Draft {
  name: string;
  enabled: boolean;
  model_sync: boolean | null;
  config: Record<string, unknown>;
  allow_private: boolean;
  endpoints: Endpoint[];
  headers: string;
  proxy: string;
  connect: string;
  first_byte: string;
  idle: string;
  total: string;
  scheduled_test: string | null;
  probe_mode: ProbeMode;
  probe_every: string | null;
}

const secs = (ms: number | undefined) => (ms ? String(ms / 1000) : "");

export function toDraft(u: Upstream): Draft {
  const config = Object.fromEntries(Object.entries(u.config ?? {}).filter(([k]) => !RESERVED_CONFIG.has(k)));
  const qp = u.quota_probe ?? null;
  return {
    name: u.name,
    enabled: u.enabled,
    model_sync: u.model_sync ?? null,
    config,
    allow_private: u.config?.allow_private === true,
    endpoints: u.endpoints,
    headers: Object.entries(u.headers ?? {})
      .map(([k, v]) => `${k}: ${v}`)
      .join("\n"),
    proxy: u.proxy ?? "",
    connect: secs(u.timeouts?.connect_ms),
    first_byte: secs(u.timeouts?.first_byte_ms),
    idle: secs(u.timeouts?.idle_ms),
    total: secs(u.timeouts?.total_ms),
    scheduled_test: u.scheduled_test ?? null,
    probe_mode: qp === null ? "default" : qp === "off" || qp === "0" ? "off" : "custom",
    probe_every: qp && qp !== "off" && qp !== "0" ? qp : null,
  };
}

/** Changed top-level fields only, so an upstream from an official template overrides as little as possible. */
export function diffDraft(before: Draft, after: Draft): UpstreamInput {
  const out: UpstreamInput = {};
  const changed = (...ks: (keyof Draft)[]) => ks.some((k) => JSON.stringify(before[k]) !== JSON.stringify(after[k]));
  if (changed("name")) out.name = after.name.trim();
  if (changed("enabled")) out.enabled = after.enabled;
  if (changed("endpoints")) out.endpoints = after.endpoints;
  if (changed("model_sync") && after.model_sync !== null) out.model_sync = after.model_sync;
  if (changed("config", "allow_private")) {
    const cfg: Record<string, unknown> = {};
    for (const k of new Set([...Object.keys(before.config), ...Object.keys(after.config)])) {
      const v = after.config[k];
      if (JSON.stringify(v) !== JSON.stringify(before.config[k])) cfg[k] = v === undefined || v === "" ? null : v;
    }
    if (changed("allow_private")) cfg.allow_private = after.allow_private ? true : null;
    out.config = cfg;
  }
  if (changed("headers")) out.headers = parseHeaders(after.headers);
  if (changed("proxy")) out.proxy = after.proxy.trim();
  if (changed("connect", "first_byte", "idle", "total")) {
    const ms = (s: string) => (s.trim() ? Math.round(Number(s) * 1000) : 0);
    out.timeouts = { connect_ms: ms(after.connect), first_byte_ms: ms(after.first_byte), idle_ms: ms(after.idle), total_ms: ms(after.total) };
  }
  if (changed("scheduled_test")) out.scheduled_test = after.scheduled_test ?? "";
  if (changed("probe_mode", "probe_every"))
    out.quota_probe = after.probe_mode === "default" ? "" : after.probe_mode === "off" ? "off" : (after.probe_every ?? "");
  return out;
}

export function countChanges(before: Draft, after: Draft): number {
  return (Object.keys(after) as (keyof Draft)[]).filter((k) => JSON.stringify(before[k]) !== JSON.stringify(after[k])).length;
}

export type Form = ReturnType<typeof useForm<Draft>>;
