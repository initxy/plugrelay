import { createContext, useCallback, useContext, useEffect, useMemo, useState, type ReactNode } from "react";
import type { LocalizedText } from "@/lib/api/types";
import { store } from "@/lib/storage";
import { en, type MessageKey } from "./en";
import { zhCN } from "./zh-CN";

export type Locale = "zh-CN" | "en";

/** Labels are each language's own name, so a reader finds theirs whatever the console shows now. */
export const LOCALES: readonly { value: Locale; label: string }[] = [
  { value: "zh-CN", label: "简体中文" },
  { value: "en", label: "English" },
];

function isLocale(v: unknown): v is Locale {
  return LOCALES.some((l) => l.value === v);
}

type Params = Record<string, string | number>;

const PLURAL = /\{(\w+), plural,((?:[^{}]|\{[^{}]*\})*)\}/g;
const BRANCH = /\s*(=\d+|zero|one|two|few|many|other)\s*\{([^{}]*)\}/g;

/** Every parameter a message uses, plural selectors included. */
export function placeholders(text: string): string[] {
  const names = new Set<string>();
  for (const m of text.matchAll(PLURAL)) names.add(m[1]!);
  for (const m of text.replace(PLURAL, "").matchAll(/\{(\w+)\}/g)) names.add(m[1]!);
  return [...names].sort();
}

/**
 * Fill `{name}` params and pick `{n, plural, one {# item} other {# items}}` branches with the
 * locale's plural rules. Numbers are formatted for the locale.
 */
export function formatMessage(locale: Locale, text: string, params?: Params): string {
  if (!params) return text;
  const num = new Intl.NumberFormat(locale);
  const show = (v: string | number) => (typeof v === "number" ? num.format(v) : v);
  const out = text.replace(PLURAL, (m, name: string, body: string) => {
    if (!(name in params)) return m;
    const value = params[name]!;
    const n = Number(value);
    const branches = new Map([...body.matchAll(BRANCH)].map((b) => [b[1]!, b[2]!]));
    const chosen = branches.get(`=${n}`) ?? branches.get(new Intl.PluralRules(locale).select(n)) ?? branches.get("other") ?? "";
    return chosen.replace(/#/g, String(show(value)));
  });
  return out.replace(/\{(\w+)\}/g, (m, name: string) => {
    const v = params[name];
    return v === undefined ? m : String(show(v));
  });
}

const DICTIONARIES: Record<Locale, Record<MessageKey, string>> = { en, "zh-CN": zhCN };

export function translate(locale: Locale, key: MessageKey, params?: Params): string {
  return formatMessage(locale, DICTIONARIES[locale][key], params);
}

/** Whether the dictionary has `key`, for keys computed from server codes. */
export function hasMessage(key: string): key is MessageKey {
  return key in en;
}

let current: Locale = "en";

/** The language the console shows now, for text made outside React (API errors). */
export function activeLocale(): Locale {
  return current;
}

interface I18nState {
  locale: Locale;
  setLocale: (l: Locale) => void;
  t: (key: MessageKey, params?: Params) => string;
  /** Whether a computed key exists, for server-provided names (setting keys, sections). */
  has: (key: string) => key is MessageKey;
  /** The caller's language from a server-provided text; English when it has no translation. */
  pick: (text: LocalizedText | undefined) => string;
}

const I18nContext = createContext<I18nState | null>(null);
const KEY = "pr.locale";

/** The first preferred language the console has: any Chinese reads zh-CN, any English en; otherwise English. */
export function matchLocale(preferred: readonly string[]): Locale {
  for (const raw of preferred) {
    const base = raw.toLowerCase().split(/[-_]/)[0];
    if (base === "zh") return "zh-CN";
    if (base === "en") return "en";
  }
  return "en";
}

/** The saved choice, else the browser's language. */
export function initialLocale(): Locale {
  const saved = store.get(KEY);
  if (isLocale(saved)) return saved;
  if (typeof navigator === "undefined") return "en";
  return matchLocale(navigator.languages?.length ? navigator.languages : [navigator.language]);
}

export function I18nProvider({ children, locale: fixed }: { children: ReactNode; locale?: Locale }) {
  const [locale, setLocaleState] = useState<Locale>(() => fixed ?? initialLocale());
  useEffect(() => {
    document.documentElement.lang = locale;
    current = locale;
  }, [locale]);
  const setLocale = useCallback((l: Locale) => {
    store.set(KEY, l);
    setLocaleState(l);
  }, []);
  const value = useMemo<I18nState>(
    () => ({
      locale,
      setLocale,
      t: (key, params) => translate(locale, key, params),
      has: hasMessage,
      pick: (text) => {
        if (!text) return "";
        return (locale === "zh-CN" && text["zh-CN"]) || text.en;
      },
    }),
    [locale, setLocale],
  );
  return <I18nContext.Provider value={value}>{children}</I18nContext.Provider>;
}

export function useI18n(): I18nState {
  const ctx = useContext(I18nContext);
  if (!ctx) throw new Error("useI18n outside I18nProvider");
  return ctx;
}
