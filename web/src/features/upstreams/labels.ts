import type { Locale } from "@/i18n";
import type { MessageKey } from "@/i18n/en";
import type { Credential, Manifest, ManifestVariable, Upstream } from "@/lib/api/types";
import type { Tone } from "@/components/status";

/** Protocols a person can pick for a custom endpoint, in the order the picker lists them. */
export const PICKABLE_PROTOCOLS = ["openai-chat", "openai-responses", "anthropic", "gemini"] as const;

const PROTOCOL_KEYS: Record<string, MessageKey> = {
  anthropic: "proto.anthropic",
  "openai-chat": "proto.openai-chat",
  "openai-responses": "proto.openai-responses",
  "openai-completions": "proto.openai-completions",
  gemini: "proto.gemini",
  "openai-embeddings": "proto.embeddings",
  "gemini-embeddings": "proto.embeddings",
  "openai-images": "proto.images",
  "openai-audio": "proto.audio",
  rerank: "proto.rerank",
  "cohere-rerank": "proto.rerank",
  "tei-rerank": "proto.rerank",
};

/** A protocol in plain words ("Anthropic API", "Embeddings"); unknown ones fall back to a generic label. */
export function protocolKey(protocol: string): MessageKey {
  return PROTOCOL_KEYS[protocol] ?? "proto.other";
}

/** Plain-word protocol labels, de-duplicated (several rerank dialects read the same). */
export function protocolLabels(protocols: readonly string[], t: (k: MessageKey) => string): string[] {
  return [...new Set(protocols.map((p) => t(protocolKey(p))))];
}

/** The generation protocols first: they decide what a coding client can pass through. */
const PROTOCOL_ORDER = ["anthropic", "openai-responses", "openai-chat", "gemini"];
export function sortProtocols<T extends { protocol: string }>(eps: readonly T[]): T[] {
  const rank = (p: string) => (PROTOCOL_ORDER.includes(p) ? PROTOCOL_ORDER.indexOf(p) : 10);
  return [...eps].sort((a, b) => rank(a.protocol) - rank(b.protocol));
}

export function hostOf(url: string): string {
  try {
    return new URL(url).host;
  } catch {
    return url;
  }
}

export function manifestTitle(m: Pick<Manifest, "title" | "title_zh">, locale: Locale): string {
  return locale === "zh-CN" && m.title_zh ? m.title_zh : m.title;
}

export function variableDescription(v: ManifestVariable, locale: Locale): string | undefined {
  return (locale === "zh-CN" && v.description_zh) || v.description || undefined;
}

const VARIABLE_KEYS: Record<string, MessageKey> = {
  base_url: "var.base_url",
  server_url: "var.server_url",
  host: "var.host",
  region: "var.region",
  resource: "var.resource",
  project: "var.project",
  location: "var.location",
  account_id: "var.account_id",
  account: "var.account",
  workspace: "var.workspace",
  api_path: "var.api_path",
  api_version: "var.api_version",
  gateway_id: "var.gateway_id",
};

/** A manifest variable's name for people; unknown names are shown as written in the manifest. */
export function variableLabel(name: string, t: (k: MessageKey) => string): string {
  const k = VARIABLE_KEYS[name];
  return k ? t(k) : name;
}

export type Category = Manifest["category"];
export const CATEGORIES: readonly Category[] = ["vendor", "cloud", "inference", "gateway", "self_hosted", "custom"];

/** Config keys that are not manifest variables and never shown as such. */
export const RESERVED_CONFIG = new Set(["manifest", "issuer", "allow_private"]);

/**
 * What kind of secret an upstream type takes. A manifest marked `key_optional` (self-hosted
 * servers, gateways without a master key, the custom OpenAI-compatible one) may need none: such
 * an upstream without credentials is routed with no auth header, and so is a credential with an
 * empty secret. Any other upstream without credentials is not routed.
 */
export type SecretForm = "key" | "aws" | "gcp" | "optional";
export function secretForm(manifest: string | undefined, keyOptional?: boolean): SecretForm {
  if (manifest === "bedrock") return "aws";
  if (manifest === "vertex-anthropic" || manifest === "vertex-gemini") return "gcp";
  return keyOptional ? "optional" : "key";
}

/** An upstream that is not routed for want of a key: none stored and its type needs one. */
export const needsKey = (u: { credentials: readonly unknown[]; key_optional: boolean }) => u.credentials.length === 0 && !u.key_optional;

/** Several pasted secrets, one per line. A service-account JSON file is always one secret. */
export function splitSecrets(text: string, form: SecretForm): string[] {
  if (form === "gcp") return text.trim() ? [text.trim()] : [];
  return text
    .split(/\r?\n/)
    .map((s) => s.trim())
    .filter(Boolean);
}

/* ---------------------------------------------------------------- monogram avatars */

const BRAND: Record<string, string> = {
  anthropic: "#c96442",
  openai: "#10a37f",
  deepseek: "#4d6bfe",
  gemini: "#3b82f6",
  "vertex-gemini": "#34a853",
  "vertex-anthropic": "#34a853",
  "volcengine-ark": "#1664ff",
  "volcengine-ark-coding": "#1664ff",
  "moonshot-kimi": "#615ced",
  "moonshot-kimi-intl": "#615ced",
  "kimi-code": "#615ced",
  "zhipu-glm": "#2a5bd7",
  "zhipu-glm-coding": "#2a5bd7",
  zai: "#2a5bd7",
  "qwen-dashscope": "#6236ff",
  "qwen-dashscope-intl": "#6236ff",
  bedrock: "#d97706",
  "bedrock-openai": "#d97706",
  openrouter: "#6b7f99",
  ollama: "#3f4650",
  mistral: "#e8590c",
  xai: "#3f4650",
  groq: "#f55036",
  siliconflow: "#7c3aed",
};

const PALETTE = ["#0d6b73", "#2c6cb0", "#6d4fc2", "#b4531c", "#1f8a4c", "#a8436b", "#5b6b7f", "#8a6d1f", "#2f7f9e", "#7a4fa0"];

/** A stable color per manifest: its brand color when we know it, else one from a fixed palette. */
export function avatarColor(name: string): string {
  if (BRAND[name]) return BRAND[name];
  let h = 0;
  for (const ch of name) h = (h * 31 + ch.charCodeAt(0)) >>> 0;
  return PALETTE[h % PALETTE.length]!;
}

/** One letter (or CJK character) that stands for the name. */
export function monogram(title: string): string {
  const s = title.trim();
  const first = [...s][0] ?? "?";
  return /[a-z]/i.test(first) ? first.toUpperCase() : first;
}

/** Shown first in the gallery's "popular" row. */
export const POPULAR = [
  "anthropic",
  "openai",
  "deepseek",
  "gemini",
  "volcengine-ark",
  "moonshot-kimi",
  "zhipu-glm",
  "qwen-dashscope",
  "bedrock",
  "vertex-anthropic",
  "openrouter",
  "ollama",
];

/* ---------------------------------------------------------------- credential and upstream state */

/** Healthy and nothing for an operator to look at. */
export const credentialFine = (c: Credential) => c.state.status === "ok" && !c.state.attention;

export function credentialTone(c: Credential): Tone {
  if (c.state.status === "ok") return c.state.attention ? "warn" : "ok";
  switch (c.state.status) {
    case "cooldown":
      return "warn";
    case "circuit_open":
      return "bad";
    case "quarantined":
    case "manual":
      return "violet";
    default:
      return "neutral";
  }
}

/** Whether "put back in rotation" means something for this credential. */
export const credentialResettable = (c: Credential) =>
  (c.state.status !== "ok" && c.state.status !== "disabled" && c.state.status !== "circuit_open") || !!c.state.attention;

type UpstreamState = "ok" | "paused" | "probing" | "attention" | "off";

/** The one state an upstream row leads with. */
export function upstreamState(u: Upstream): UpstreamState {
  if (!u.enabled) return "off";
  if (u.circuit.state === "open") return "paused";
  if (u.circuit.state === "half_open") return "probing";
  if (u.credentials.some((c) => !credentialFine(c) && c.state.status !== "disabled")) return "attention";
  return "ok";
}

export const UPSTREAM_TONE: Record<UpstreamState, Tone> = {
  ok: "ok",
  paused: "bad",
  probing: "warn",
  attention: "warn",
  off: "neutral",
};
