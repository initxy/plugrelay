import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { ShieldCheck, Trash2, Unlink } from "lucide-react";
import { toast } from "sonner";
import { useConfirm } from "@/components/confirm";
import { LoadingRows } from "@/components/page";
import { Pill } from "@/components/status";
import { Button } from "@/components/ui/button";
import type { ReauthRun } from "@/features/auth/use-reauth";
import { api, unwrap } from "@/lib/api/client";
import { ApiError, errorMessage } from "@/lib/api/errors";
import type { Identity, User } from "@/lib/api/types";
import { formatRelative } from "@/lib/format";
import { useI18n } from "@/i18n";

/** How a user is named in the console: their display name, else their username. */
export const userName = (u: Pick<User, "display_name" | "username">) => u.display_name || u.username;

/** The user's linked single sign-on accounts, each with an unlink button behind a confirm. */
export function LinkedIdentities({ user }: { user: User }) {
  const { t, locale } = useI18n();
  const qc = useQueryClient();
  const confirm = useConfirm();
  const key = ["users", user.id, "identities"];
  const q = useQuery({ queryKey: key, queryFn: () => unwrap(api.GET("/users/{id}/identities", { params: { path: { id: user.id } } })) });
  const unlink = useMutation({
    mutationFn: (x: Identity) => unwrap(api.DELETE("/users/{id}/identities/{identity_id}", { params: { path: { id: user.id, identity_id: x.id } } })),
    onSuccess: (_, x) => {
      void qc.invalidateQueries({ queryKey: key });
      toast.success(t("users.unlinked", { provider: x.provider_name }));
    },
    onError: (e) => toast.error(errorMessage(e)),
  });
  if (q.isError) return <p className="text-xs text-bad">{errorMessage(q.error)}</p>;
  if (q.isPending) return <LoadingRows rows={1} />;
  if (q.data.items.length === 0) return <p className="text-sm text-ink-3">{t("users.ssoEmpty")}</p>;
  return (
    <ul className="grid gap-1.5">
      {q.data.items.map((x) => (
        <li key={x.id} className="flex items-center gap-3 rounded-md border border-line px-3 py-2">
          <div className="min-w-0 flex-1">
            <div className="text-sm font-medium">{x.provider_name}</div>
            <div className="truncate text-xs text-ink-2">{x.email ? `${x.email} · ${x.subject}` : x.subject}</div>
            <div className="text-xs text-ink-3">
              {[
                x.created_at && t("users.ssoLinked", { time: formatRelative(x.created_at, locale) }),
                x.last_login_at && t("users.ssoLastLogin", { time: formatRelative(x.last_login_at, locale) }),
              ]
                .filter(Boolean)
                .join(" · ")}
            </div>
          </div>
          <Button
            size="sm"
            variant="ghost"
            aria-label={t("users.unlinkNamed", { name: x.provider_name })}
            disabled={unlink.isPending}
            onClick={async () => {
              if (
                await confirm({
                  title: t("users.unlinkTitle", { provider: x.provider_name, user: userName(user) }),
                  body: t("users.unlinkBody"),
                  confirm: t("users.unlink"),
                  danger: true,
                })
              )
                unlink.mutate(x);
            }}
          >
            <Unlink />
            {t("users.unlink")}
          </Button>
        </li>
      ))}
    </ul>
  );
}

/** Two-step sign-in state, with an audited reset for a user who lost their factors. */
export function MfaRow({ user, run }: { user: User; run: ReauthRun }) {
  const { t } = useI18n();
  const qc = useQueryClient();
  const confirm = useConfirm();
  const name = userName(user);
  const reset = useMutation({
    mutationFn: () => run(() => unwrap(api.DELETE("/users/{id}/mfa", { params: { path: { id: user.id } } }))),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: ["users"] });
      toast.success(t("users.mfaResetDone", { name }));
    },
    onError: (e) => toast.error(errorMessage(e)),
  });
  const on = user.mfa_enabled && !reset.isSuccess;
  return (
    <div className="flex items-center gap-3">
      <div className="min-w-0 flex-1">
        <div className="flex items-center gap-2 text-sm font-medium">
          {t("users.mfa")}
          <Pill tone={on ? "ok" : "neutral"} dot>
            {on ? t("users.mfaOnShort") : t("users.mfaOffShort")}
          </Pill>
        </div>
        <p className="text-xs text-ink-3">{on ? t("users.mfaOn") : t("users.mfaOff")}</p>
      </div>
      {on && (
        <Button
          size="sm"
          variant="danger"
          disabled={reset.isPending}
          onClick={async () => {
            if (await confirm({ title: t("users.mfaResetTitle", { name }), body: t("users.mfaResetBody"), confirm: t("users.mfaReset"), danger: true }))
              reset.mutate();
          }}
        >
          {t("users.mfaReset")}
        </Button>
      )}
    </div>
  );
}

/**
 * Deletes the user after a confirm (re-authenticating when asked). When something the user has
 * running cannot be ended (409 user_delete_blocked, e.g. a subscription the payment provider
 * would not cancel), it shows the reason and offers to delete anyway with ?force=true.
 */
export function DeleteUserButton({ user, run, onDeleted }: { user: User; run: ReauthRun; onDeleted: () => void }) {
  const { t } = useI18n();
  const qc = useQueryClient();
  const confirm = useConfirm();
  const name = userName(user);
  const del = useMutation({
    mutationFn: (force: boolean) =>
      run(() => unwrap(api.DELETE("/users/{id}", { params: { path: { id: user.id }, query: force ? { force: true } : undefined } }))),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: ["users"] });
      toast.success(t("users.deleted", { name }));
      onDeleted();
    },
  });
  const remove = async (force: boolean) => {
    try {
      await del.mutateAsync(force);
    } catch (e) {
      if (force || !(e instanceof ApiError && e.code === "user_delete_blocked")) {
        toast.error(errorMessage(e));
        return;
      }
      const anyway = await confirm({
        title: t("users.deleteBlockedTitle", { name }),
        body: (
          <div className="grid gap-2">
            <p>{e.message}</p>
            <p className="font-medium text-bad">{t("users.deleteAnywayWarning")}</p>
          </div>
        ),
        confirm: t("users.deleteAnyway"),
        danger: true,
      });
      if (anyway) await remove(true);
    }
  };
  return (
    <Button
      variant="danger"
      className="mr-auto"
      disabled={del.isPending}
      onClick={async () => {
        if (await confirm({ title: t("users.deleteTitle", { name }), body: t("users.deleteBody"), confirm: t("common.delete"), danger: true })) await remove(false);
      }}
    >
      <Trash2 />
      {t("users.delete")}
    </Button>
  );
}

export function MfaIcon({ on }: { on?: boolean }) {
  const { t } = useI18n();
  if (!on) return null;
  return <ShieldCheck className="size-3.5 shrink-0 text-ok" aria-label={t("users.mfaOnShort")} />;
}
