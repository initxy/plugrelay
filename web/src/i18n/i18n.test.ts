import { describe, expect, it } from "vitest";
import { en } from "./en";
import { formatMessage, matchLocale, placeholders, translate } from "./index";
import { zhCN } from "./zh-CN";

const PLURAL_BLOCK = /\{(\w+), plural,((?:[^{}]|\{[^{}]*\})*)\}/g;

/** Problems with one message against the same key in the other language. */
function problems(text: string | undefined, other: string): string[] {
  if (typeof text !== "string" || text.trim() === "") return ["missing or empty"];
  const out: string[] = [];
  if (placeholders(text).join() !== placeholders(other).join()) out.push(`placeholders ${placeholders(text)} ≠ ${placeholders(other)}`);
  for (const [, , body = ""] of text.matchAll(PLURAL_BLOCK)) {
    if (!/\bother\s*\{/.test(body)) out.push("plural without other");
  }
  return out;
}

describe("dictionaries", () => {
  const source = en as Record<string, string>;
  const zh = zhCN as Record<string, string>;

  it("zh-CN has every en key with matching placeholders", () => {
    const bad: Record<string, string[]> = {};
    for (const key of Object.keys(source)) {
      const p = problems(zh[key], source[key] ?? "");
      if (p.length) bad[key] = p;
    }
    expect(bad).toEqual({});
  });

  it("en has every zh-CN key", () => {
    const bad = Object.keys(zh).filter((key) => typeof source[key] !== "string" || source[key]!.trim() === "");
    expect(bad).toEqual([]);
  });
});

describe("formatMessage", () => {
  it("interpolates params", () => {
    expect(translate("en", "logs.loaded", { n: 2 })).toBe("2 loaded");
    expect(translate("zh-CN", "logs.loaded", { n: 2 })).toBe("已加载 2 条");
  });

  it("picks plural branches by the locale's rules", () => {
    const text = "{n, plural, =0 {none} one {# day} other {# days}}";
    expect(formatMessage("en", text, { n: 0 })).toBe("none");
    expect(formatMessage("en", text, { n: 1 })).toBe("1 day");
    expect(formatMessage("en", text, { n: 1200 })).toBe("1,200 days");
    expect(formatMessage("zh-CN", "{n, plural, other {# 天}}", { n: 1 })).toBe("1 天");
  });

  it("leaves unknown params alone", () => {
    expect(formatMessage("en", "{n} of {of}", { n: 12345 })).toBe("12,345 of {of}");
  });

  it("reports plural selectors as placeholders", () => {
    expect(placeholders("Hi {name}, {n, plural, one {# key} other {# keys}}")).toEqual(["n", "name"]);
  });
});

describe("matchLocale", () => {
  it("reads any Chinese as zh-CN and everything else as English", () => {
    expect(matchLocale(["zh"])).toBe("zh-CN");
    expect(matchLocale(["zh-CN"])).toBe("zh-CN");
    expect(matchLocale(["zh-Hant-TW"])).toBe("zh-CN");
    expect(matchLocale(["zh_HK"])).toBe("zh-CN");
    expect(matchLocale(["ja-JP", "zh-CN"])).toBe("zh-CN");
    expect(matchLocale(["en-GB", "zh-CN"])).toBe("en");
    expect(matchLocale(["de-DE"])).toBe("en");
    expect(matchLocale([])).toBe("en");
  });
});
