import type { Tone } from "@/components/status";
import type { AuditEntry, Feature, SettingItem } from "@/lib/api/types";
import type { MessageKey } from "@/i18n/en";

type T = (key: MessageKey, params?: Record<string, string | number>) => string;
type Has = (key: string) => key is MessageKey;

/** Filter categories, each a list of action patterns the API understands (`key.*`). */
export const CATEGORIES: readonly { id: string; label: MessageKey; actions: string }[] = [
  { id: "keys", label: "audit.cat.keys", actions: "key.*" },
  { id: "upstreams", label: "audit.cat.upstreams", actions: "upstream.*,credential.*" },
  { id: "models", label: "audit.cat.models", actions: "model.*,preset.*,pricing.*" },
  { id: "members", label: "audit.cat.members", actions: "user.*,group.*" },
  { id: "signin", label: "audit.cat.signin", actions: "auth.*,sso.*" },
  { id: "features", label: "audit.cat.features", actions: "feature.*,extension.*,plugin.*,mcp.*,notify.*" },
  { id: "settings", label: "audit.cat.settings", actions: "settings.*,setup.*" },
];

/** Object types a filter offers; the API's `target_type`. */
export const TARGET_TYPES = ["key", "upstream", "credential", "model", "preset", "pricing_rule", "user", "group", "feature", "extension", "sso_provider", "mcp_server", "notify_channel", "settings"] as const;

const VERB_TONE: Record<string, Tone> = { create: "ok", delete: "bad", disable: "warn", enable: "ok", reset: "warn", rotate: "warn", login: "info" };

/** The colour of an action's dot: created green, deleted red, everything else quieter. */
export function actionTone(action: string): Tone {
  const verb = action.split(".").at(-1)!;
  return VERB_TONE[verb] ?? "neutral";
}

/** "Created a key": the action as a sentence without its object. */
export function actionText(action: string, t: T, has: Has): string {
  const k = `audit.act.${action}`;
  if (has(k)) return t(k);
  const verb = action.split(".").at(-1)!;
  const v = `audit.verb.${verb}`;
  return has(v) ? t(v) : t("audit.act.other");
}

/** "Key", "Upstream": the kind of object an entry is about. */
export function targetTypeText(type: string, t: T, has: Has): string {
  const k = `audit.type.${type}`;
  return has(k) ? t(k) : t("audit.type.other");
}

export interface Names {
  features: readonly Feature[];
  settings: readonly SettingItem[];
  pick: (text: { en: string; "zh-CN"?: string } | undefined) => string;
}

const looksLikeId = (s: string) => /^[0-9a-f]{8}-[0-9a-f-]{20,}$/i.test(s) || /^[A-Za-z0-9_-]{20,}$/.test(s);

/** The object's own name in the reader's language, or nothing when only an id is known. */
export function targetName(e: AuditEntry, names: Names): string | undefined {
  const { type, id, name } = e.target;
  if (type === "feature") {
    const f = names.features.find((x) => x.id === id);
    if (f) return names.pick(f.name);
  }
  if (type === "plugin") {
    const f = names.features.find((x) => x.modules.some((m) => m.id === id));
    return f ? names.pick(f.name) : undefined;
  }
  if (type === "settings") return undefined;
  const shown = name || id;
  return shown && !looksLikeId(shown) && shown !== "*" ? shown : undefined;
}

/** Who did it, with the special callers named. */
export function actorText(e: AuditEntry, t: T): string {
  if (e.actor.id === "cli") return t("audit.actor.cli");
  if (e.actor.id === "token") return t("audit.actor.token");
  if (e.actor.id === "anonymous") return t("audit.actor.anonymous");
  return e.actor.name || e.actor.id;
}

/**
 * A changed field's name in words: site settings and feature settings by their own titles, the
 * rest from the dictionary. Undefined when the console has no name for it (show it as code).
 */
export function fieldText(field: string, t: T, has: Has, names: Names): string | undefined {
  const site = names.settings.find((s) => s.key === field);
  if (site) return names.pick(site.title);
  const dot = field.lastIndexOf(".");
  if (dot > 0) {
    const module = field.slice(0, dot);
    const key = field.slice(dot + 1);
    for (const f of names.features) {
      const s = f.settings.find((x) => x.module === module && x.key === key);
      if (s) return names.pick(s.label);
    }
  }
  const k = `audit.field.${field}`;
  return has(k) ? t(k) : undefined;
}

interface LeafChange {
  path: string;
  from: unknown;
  to: unknown;
}

const isObj = (v: unknown): v is Record<string, unknown> => typeof v === "object" && v !== null && !Array.isArray(v);

/** The leaves that differ between two values, with dotted paths; equal leaves are left out. */
export function diffLeaves(from: unknown, to: unknown, path = ""): LeafChange[] {
  // A created or removed object lists its fields one by one too.
  if (isObj(from) && to === undefined) to = {};
  if (isObj(to) && from === undefined) from = {};
  if (isObj(from) && isObj(to)) {
    const keys = [...new Set([...Object.keys(from), ...Object.keys(to)])].sort();
    return keys.flatMap((k) => diffLeaves(from[k], to[k], path ? `${path}.${k}` : k));
  }
  if (JSON.stringify(from) === JSON.stringify(to)) return [];
  return [{ path, from, to }];
}

/** A value for a person: yes/no, lists joined, objects as compact JSON. */
export function valueText(v: unknown, t: T, has: Has): string {
  if (v === undefined || v === null || v === "") return t("audit.unset");
  if (typeof v === "boolean") return t(v ? "audit.yes" : "audit.no");
  if (typeof v === "string") {
    const role = `role.${v}`;
    if ((v === "admin" || v === "member" || v === "user") && has(role)) return t(role);
    if (v === "********") return t("audit.hidden");
    return v;
  }
  if (typeof v === "number") return String(v);
  if (Array.isArray(v) && v.every((x) => typeof x !== "object")) return v.length ? v.join(", ") : t("audit.empty.list");
  return JSON.stringify(v);
}

/** Where the object lives in the console, if it has a page. */
export function targetLink(e: AuditEntry): { to: string; search?: Record<string, string> } | undefined {
  const { type, id } = e.target;
  switch (type) {
    case "key":
      return { to: "/keys", search: { id } };
    case "upstream":
      return { to: "/upstreams", search: { id } };
    case "user":
      return { to: "/users", search: { id } };
    case "group":
      return { to: "/users", search: { tab: "groups", id } };
    case "model":
    case "preset":
      return { to: "/models", search: { id } };
    case "pricing_rule":
      return { to: "/models", search: { tab: "pricing" } };
    case "feature":
    case "extension":
      return { to: "/features", search: { id } };
    case "sso_provider":
      return { to: "/sso" };
    case "mcp_server":
      return { to: "/mcp" };
    case "settings":
      return { to: "/settings" };
    default:
      return undefined;
  }
}
