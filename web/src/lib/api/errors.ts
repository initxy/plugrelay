import { activeLocale, hasMessage, translate, type Locale } from "@/i18n";
import type { ApiErrorBody } from "./types";

type Params = Record<string, string | number>;

/** One refused field of a 422 answer. */
export interface FieldIssue {
  path: string;
  code: string;
  params?: Params;
  /** The server's English text. */
  message: string;
}

/**
 * The console's wording of an API error code (`apiError.<code>`), params filled in. A param
 * value with its own entry (`apiError.object.user`) is translated too. Unknown codes fall back
 * to the server's English text.
 */
function apiErrorText(locale: Locale, code: string, params: Params | undefined, english: string): string {
  const key = "apiError." + code;
  if (!hasMessage(key)) return english;
  const filled: Params = {};
  for (const [name, v] of Object.entries(params ?? {})) {
    const valueKey = `apiError.${name}.${v}`;
    filled[name] = hasMessage(valueKey) ? translate(locale, valueKey) : v;
  }
  return translate(locale, key, filled);
}

/**
 * An error the API answered with. `message` and `fields` are worded in the console's language
 * at the time of the error; `serverMessage` keeps the server's English text.
 */
export class ApiError extends Error {
  readonly status: number;
  readonly code: string;
  readonly params: Params;
  readonly serverMessage: string;
  readonly issues: FieldIssue[];
  /** Field path → message, from a 422 response. */
  readonly fields: Record<string, string>;

  constructor(status: number, body: ApiErrorBody | undefined, fallback: string, locale: Locale = activeLocale()) {
    const e = body?.error;
    const issues: FieldIssue[] = e?.fields ?? [];
    const word = (i: FieldIssue) => apiErrorText(locale, i.code, i.params, i.message);
    let message = fallback;
    if (e) message = issues.length === 1 ? word(issues[0]!) : apiErrorText(locale, e.code, e.params, e.message || fallback);
    super(message);
    this.name = "ApiError";
    this.status = status;
    this.code = e?.code ?? "http_" + status;
    this.params = e?.params ?? {};
    this.serverMessage = e?.message || fallback;
    this.issues = issues;
    this.fields = Object.fromEntries(issues.map((i) => [i.path, word(i)]));
  }
}

export function errorMessage(err: unknown): string {
  if (err instanceof Error) return err.message;
  return String(err);
}
