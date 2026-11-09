import { expect } from "vitest";

/**
 * Words the console must not show: config keys and module ids such as `site.public_base_url`
 * or `middleware.ratelimit`, and snake_case ids such as `priority_weight` or `needs_approval`.
 * A match right after `/`, `@`, `:` or `.` is part of a path, an email or a URL and is left alone.
 */
const DOTTED = /(?<![\w@/:.-])[a-z][a-z0-9_-]*(?:\.[a-z0-9_*-]+)+(?![\w/@-])/g;
const SNAKE = /(?<![\w@/:.-])[a-z][a-z0-9]*(?:_[a-z0-9]+)+(?![\w/@.-])/g;
/** Host names and file names read as words: example.com, demo.local, relay.yaml, submit.php. */
const NAME_SUFFIX = /\.(com|net|org|io|dev|local|cn|ai|app|cloud|yaml|yml|json|md|php)$/;
const DEFAULT_ALLOW = ["e.g", "i.e"];
/** Places where showing the raw text is the point: code samples, keys, user input. */
const SKIP = "code, pre, kbd, samp, script, style, textarea, [data-raw]";
const ATTRS = ["aria-label", "title", "placeholder", "alt"];

function hits(text: string, allow: readonly (string | RegExp)[]): string[] {
  const out: string[] = [];
  for (const re of [DOTTED, SNAKE]) {
    for (const m of text.matchAll(re)) {
      const word = m[0].replace(/\.+$/, "");
      if (!word.includes(".") && !word.includes("_")) continue;
      if (NAME_SUFFIX.test(word)) continue;
      // Versions: v1.2.0, gpt-5.5.
      if (word.split(".").slice(1).some((seg) => /^\d/.test(seg))) continue;
      if ([...DEFAULT_ALLOW, ...allow].some((a) => (typeof a === "string" ? a === word : a.test(word)))) continue;
      out.push(word);
    }
  }
  return out;
}

/**
 * Every raw term rendered under `root`: text nodes and the readable attributes of elements, except
 * inside code, user input and `[data-raw]`.
 */
export function findRawTerms(root: ParentNode = document.body, { allow = [] }: { allow?: readonly (string | RegExp)[] } = {}): string[] {
  const found = new Set<string>();
  const walker = document.createTreeWalker(root as Node, NodeFilter.SHOW_TEXT | NodeFilter.SHOW_ELEMENT);
  for (let n = walker.nextNode(); n; n = walker.nextNode()) {
    if (n.nodeType === Node.TEXT_NODE) {
      const parent = n.parentElement;
      if (!parent || parent.closest(SKIP)) continue;
      for (const h of hits(n.textContent ?? "", allow)) found.add(`"${h}" in "${(n.textContent ?? "").trim().slice(0, 80)}"`);
    } else {
      const el = n as Element;
      if (el.closest(SKIP) || el instanceof HTMLInputElement) continue;
      for (const a of ATTRS) {
        const v = el.getAttribute(a);
        if (v) for (const h of hits(v, allow)) found.add(`"${h}" in ${a}="${v.slice(0, 80)}"`);
      }
    }
  }
  return [...found];
}

/** Fail the test when the page shows a config key, module id or other internal term. */
export function expectPlainWords(root: ParentNode = document.body, opts?: { allow?: readonly (string | RegExp)[] }): void {
  expect(findRawTerms(root, opts), "raw config keys or ids on the page").toEqual([]);
}
