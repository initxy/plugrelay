import type { MessageKey } from "@/i18n/en";
import type { SettingItem } from "@/lib/api/types";

export type SectionId = "site" | "security" | "network" | "logs" | "other";

/** The sub-nav sections, in order, and the server sections each one shows. */
export const SECTIONS: readonly { id: SectionId; label: MessageKey; description: MessageKey; from: readonly string[] }[] = [
  { id: "site", label: "settings.nav.site", description: "settings.site.description", from: ["general", "branding"] },
  { id: "security", label: "settings.nav.security", description: "settings.security.description", from: ["security"] },
  { id: "network", label: "settings.nav.network", description: "settings.network.description", from: ["network"] },
  { id: "logs", label: "settings.nav.logs", description: "settings.logs.description", from: ["logging"] },
];

/** Groups of rows inside a section, by setting key. Keys not listed land in the section's last group. */
const GROUPS: Record<SectionId, readonly { title: MessageKey; keys: readonly string[] }[]> = {
  site: [
    { title: "settings.group.basics", keys: ["site.name", "site.public_base_url", "site.currency", "site.timezone"] },
    { title: "settings.group.look", keys: ["site.logo_url", "site.accent"] },
    { title: "settings.group.members", keys: ["site.show_upstream_names"] },
  ],
  security: [
    { title: "settings.group.signin", keys: ["security.password_login", "security.local_owner_login", "security.session_ttl_hours"] },
    { title: "settings.group.mfa", keys: ["security.require_2fa_admins", "security.require_2fa_grace_days", "security.require_2fa_sso"] },
    { title: "settings.group.captcha", keys: ["security.captcha_provider", "security.login_captcha_after", "security.captcha_site_key", "security.captcha_secret"] },
    { title: "settings.group.api", keys: ["security.admin_token"] },
  ],
  network: [
    { title: "settings.group.listen", keys: ["listen", "network.listen_public"] },
    { title: "settings.group.storage", keys: ["store.driver", "state.driver"] },
  ],
  logs: [{ title: "settings.group.other", keys: [] }],
  other: [{ title: "settings.group.other", keys: [] }],
};

/** A row that only matters while another setting has a value: shown, but off until then. */
export const DEPENDS: Record<string, { on: string; active: (v: unknown) => boolean; hint: MessageKey }> = {
  "security.require_2fa_grace_days": { on: "security.require_2fa_admins", active: Boolean, hint: "settings.dependsOn2fa" },
  "security.captcha_site_key": { on: "security.captcha_provider", active: (v) => !!v && v !== "off", hint: "settings.dependsOnCaptcha" },
  "security.captcha_secret": { on: "security.captcha_provider", active: (v) => !!v && v !== "off", hint: "settings.dependsOnCaptcha" },
  "security.login_captcha_after": { on: "security.captcha_provider", active: (v) => !!v && v !== "off", hint: "settings.dependsOnCaptcha" },
};

export function sectionOf(item: SettingItem): SectionId {
  return SECTIONS.find((s) => s.from.includes(item.section))?.id ?? "other";
}

interface RowGroup {
  title: MessageKey;
  items: SettingItem[];
}

/** The rows of one section, grouped and ordered by GROUPS; empty groups are dropped. */
export function groupsOf(section: SectionId, items: readonly SettingItem[]): RowGroup[] {
  const defs = GROUPS[section];
  const mine = items.filter((i) => sectionOf(i) === section);
  const placed = new Set<string>();
  const out = defs.map((g) => {
    const rows = g.keys.map((k) => mine.find((i) => i.key === k)).filter((i): i is SettingItem => i !== undefined);
    rows.forEach((r) => placed.add(r.key));
    return { title: g.title, items: rows };
  });
  const rest = mine.filter((i) => !placed.has(i.key));
  if (rest.length) out[out.length - 1]!.items.push(...rest);
  return out.filter((g) => g.items.length > 0);
}

/** The unit shown after a number setting's box, by setting key. */
export const UNIT: Record<string, MessageKey> = {
  "security.session_ttl_hours": "settings.unit.hours",
  "security.require_2fa_grace_days": "settings.unit.days",
  "security.login_captcha_after": "settings.unit.failures",
};
