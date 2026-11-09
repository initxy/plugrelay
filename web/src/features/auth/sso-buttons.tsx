import { queryOptions } from "@tanstack/react-query";
import { KeyRound } from "lucide-react";
import { Button } from "@/components/ui/button";
import { api, unwrap } from "@/lib/api/client";
import type { AuthProviders } from "@/lib/api/types";
import { useI18n } from "@/i18n";
import type { MessageKey } from "@/i18n/en";

export const authProvidersQuery = queryOptions({
  queryKey: ["auth-providers"],
  queryFn: () => unwrap(api.GET("/auth/providers")),
  staleTime: 60_000,
});

/** The relay endpoint that starts single sign-on; a full-page navigation, not a fetch. */
export function ssoStartHref(id: string, next?: string): string {
  const q = next ? `?return_to=${encodeURIComponent(next)}` : "";
  return `/api/v1/auth/sso/${encodeURIComponent(id)}/start${q}`;
}

const ERROR_CODES = [
  "state_mismatch",
  "state_expired",
  "state_replayed",
  "denied",
  "invalid_token",
  "provider_error",
  "provider_unavailable",
  "domain_not_allowed",
  "tenant_not_allowed",
  "email_unverified",
  "account_exists",
  "no_account",
  "account_disabled",
  "unknown_provider",
  "public_url_missing",
  "rate_limited",
] as const;

/** Text for a `?sso_error=` code the callback redirected with. */
export function useSsoErrorText(code: string | undefined): string | undefined {
  const { t } = useI18n();
  if (!code) return undefined;
  return (ERROR_CODES as readonly string[]).includes(code) ? t(`sso.error.${code}` as MessageKey) : t("sso.error.other", { code });
}

/** A small mark per provider kind, so the buttons are told apart at a glance. */
export function ProviderMark({ preset }: { preset: string }) {
  if (preset === "microsoft")
    return (
      <svg viewBox="0 0 16 16" className="size-4" aria-hidden>
        <rect x="1" y="1" width="6.5" height="6.5" fill="#f25022" />
        <rect x="8.5" y="1" width="6.5" height="6.5" fill="#7fba00" />
        <rect x="1" y="8.5" width="6.5" height="6.5" fill="#00a4ef" />
        <rect x="8.5" y="8.5" width="6.5" height="6.5" fill="#ffb900" />
      </svg>
    );
  const letter: Record<string, [string, string]> = {
    google: ["G", "#4285f4"],
    github: ["GH", "#24292f"],
    feishu: ["飞", "#3370ff"],
    lark: ["L", "#3370ff"],
  };
  const hit = letter[preset];
  if (!hit) return <KeyRound aria-hidden className="size-4 text-ink-2" />;
  return (
    <span aria-hidden className="grid size-[18px] place-items-center rounded-[5px] text-[10px] font-bold leading-none text-white" style={{ background: hit[1] }}>
      {hit[0]}
    </span>
  );
}

export function SsoButtons({ providers, next }: { providers: AuthProviders["providers"]; next?: string }) {
  const { t } = useI18n();
  if (providers.length === 0) return null;
  return (
    <div className="grid gap-2">
      {providers.map((p) => (
        <Button key={p.id} asChild size="lg" className="h-10 justify-center gap-2.5">
          <a href={ssoStartHref(p.id, next)}>
            <ProviderMark preset={p.preset} />
            {t("sso.continueWith", { name: p.name })}
          </a>
        </Button>
      ))}
    </div>
  );
}
