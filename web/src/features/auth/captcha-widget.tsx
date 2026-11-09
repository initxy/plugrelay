import { useEffect, useRef, useState } from "react";
import { useI18n } from "@/i18n";

export type CaptchaProvider = "turnstile" | "hcaptcha";

interface WidgetApi {
  render(el: HTMLElement, opts: Record<string, unknown>): string | number;
  remove?(id: string | number): void;
}

declare global {
  interface Window {
    turnstile?: WidgetApi;
    hcaptcha?: WidgetApi;
  }
}

const SCRIPTS: Record<CaptchaProvider, string> = {
  turnstile: "https://challenges.cloudflare.com/turnstile/v0/api.js?render=explicit",
  hcaptcha: "https://js.hcaptcha.com/1/api.js?render=explicit&recaptchacompat=off",
};

const loading = new Map<CaptchaProvider, Promise<WidgetApi>>();

/** Load the provider's script once (allowed by the console's CSP). */
function loadApi(p: CaptchaProvider): Promise<WidgetApi> {
  const ready = window[p];
  if (ready) return Promise.resolve(ready);
  let pending = loading.get(p);
  if (!pending) {
    pending = new Promise<WidgetApi>((resolve, reject) => {
      const s = document.createElement("script");
      s.src = SCRIPTS[p];
      s.async = true;
      s.onload = () => {
        const api = window[p];
        if (api) resolve(api);
        else reject(new Error("captcha script loaded without its API"));
      };
      s.onerror = () => {
        loading.delete(p);
        reject(new Error("captcha script failed to load"));
      };
      document.head.appendChild(s);
    });
    loading.set(p, pending);
  }
  return pending;
}

/**
 * Turnstile or hCaptcha widget. `onToken` gets the response token, or undefined when it expires or
 * fails; remount (change `key`) to get a fresh challenge after the server used a token.
 */
export function CaptchaWidget({ provider, siteKey, onToken }: { provider: CaptchaProvider; siteKey: string; onToken: (token: string | undefined) => void }) {
  const { t, locale } = useI18n();
  const el = useRef<HTMLDivElement>(null);
  const cb = useRef(onToken);
  const [failed, setFailed] = useState(false);
  useEffect(() => {
    cb.current = onToken;
  });
  useEffect(() => {
    let id: string | number | undefined;
    let gone = false;
    loadApi(provider)
      .then((api) => {
        if (gone || !el.current) return;
        id = api.render(el.current, {
          sitekey: siteKey,
          hl: locale,
          language: locale,
          callback: (tok: string) => cb.current(tok),
          "expired-callback": () => cb.current(undefined),
          "error-callback": () => cb.current(undefined),
        });
      })
      .catch(() => !gone && setFailed(true));
    return () => {
      gone = true;
      if (id !== undefined) window[provider]?.remove?.(id);
    };
  }, [provider, siteKey, locale]);
  return (
    <div>
      <div ref={el} data-testid="captcha-widget" data-provider={provider} />
      {failed && <p className="mt-1 text-xs text-bad">{t("captcha.loadFailed")}</p>}
    </div>
  );
}
