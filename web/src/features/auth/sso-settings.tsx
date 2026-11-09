import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { AlertTriangle, LogIn, Plus, Trash2 } from "lucide-react";
import { useState } from "react";
import { toast } from "sonner";
import { useConfirm } from "@/components/confirm";
import { CopyButton } from "@/components/copy-button";
import { EmptyState, ErrorState, LoadingRows, PageHeader } from "@/components/page";
import { StatusText } from "@/components/status";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { api, unwrap } from "@/lib/api/client";
import { errorMessage } from "@/lib/api/errors";
import { useI18n } from "@/i18n";
import { authProvidersQuery, ProviderMark } from "./sso-buttons";
import type { components } from "@/lib/api/schema.gen";
import { ProviderDrawer, type SsoProvider } from "./sso-provider-drawer";

type SsoProviders = Omit<components["schemas"]["SsoProviders"], "items"> & { items: SsoProvider[] };
const ssoQuery = {
  queryKey: ["sso"],
  queryFn: () => unwrap(api.GET("/sso/providers")) as Promise<SsoProviders>,
};

/** The sign-in providers page: one card per provider, editor in a drawer. Password sign-in is a site setting. */
export function SsoSettings() {
  const { t } = useI18n();
  const qc = useQueryClient();
  const confirm = useConfirm();
  const q = useQuery(ssoQuery);
  const [editing, setEditing] = useState<SsoProvider | "new" | null>(null);
  const refresh = () => {
    void qc.invalidateQueries({ queryKey: ssoQuery.queryKey });
    void qc.invalidateQueries({ queryKey: authProvidersQuery.queryKey });
  };
  const remove = useMutation({
    mutationFn: (id: string) => unwrap(api.DELETE("/sso/providers/{id}", { params: { path: { id } } })),
    onSuccess: () => refresh(),
    onError: (e) => toast.error(errorMessage(e)),
  });
  const add = (
    <Button variant="primary" onClick={() => setEditing("new")} disabled={!q.data}>
      <Plus aria-hidden />
      {t("sso.add")}
    </Button>
  );

  return (
    <>
      <PageHeader
        title={t("nav.sso")}
        description={t("sso.description")}
        actions={q.data && q.data.items.length > 0 ? add : undefined}
      />
      {q.isError ? (
        <ErrorState error={q.error} onRetry={() => void q.refetch()} />
      ) : q.isPending ? (
        <LoadingRows rows={3} />
      ) : (
        <div className="grid gap-4">
          {q.data.redirect_problem && (
            <div
              role="alert"
              className="flex gap-2.5 rounded-lg border border-warn/30 bg-warn-soft px-4 py-3 text-sm"
            >
              <AlertTriangle className="mt-0.5 size-4 shrink-0 text-warn" aria-hidden />
              <div className="min-w-0">
                <p className="font-medium text-warn">{t("sso.redirectProblem")}</p>
                <p className="mt-0.5 text-ink-2">
                  {t("sso.redirectProblemHint")}{" "}
                  <Link to="/settings" className="text-accent hover:underline">
                    {t("sso.openSettings")}
                  </Link>
                </p>
                <code className="mt-1 block break-words font-mono text-xs text-ink-3">
                  {q.data.redirect_problem}
                </code>
              </div>
            </div>
          )}
          {q.data.items.length === 0 ? (
            <div className="rounded-lg border border-dashed border-line-strong bg-surface">
              <EmptyState icon={LogIn} title={t("sso.empty")} hint={t("sso.emptyHint")} action={add} />
            </div>
          ) : (
            <ul className="grid gap-3 lg:grid-cols-2">
              {q.data.items.map((p) => (
                <li
                  key={p.id}
                  className="flex flex-col gap-3 rounded-lg border border-line bg-surface p-4 shadow-card"
                >
                  <div className="flex items-start gap-3">
                    <ProviderMark preset={p.preset} />
                    <div className="min-w-0 flex-1">
                      <div className="flex flex-wrap items-center gap-2">
                        <span className="text-base font-semibold">{p.name}</span>
                        {p.name !== t(`sso.preset.${p.preset}`) && <Badge>{t(`sso.preset.${p.preset}`)}</Badge>}
                      </div>
                      <StatusText tone={p.enabled ? "ok" : "neutral"} className="mt-0.5 text-xs">
                        {t(p.enabled ? "sso.enabled" : "sso.disabled")}
                      </StatusText>
                    </div>
                    <div className="flex gap-1">
                      <Button size="sm" onClick={() => setEditing(p)}>
                        {t("common.edit")}
                      </Button>
                      <Button
                        size="icon-sm"
                        variant="ghost"
                        aria-label={t("sso.deleteNamed", { name: p.name })}
                        onClick={async () => {
                          const ok = await confirm({
                            title: t("sso.deleteTitle", { name: p.name }),
                            body: t("sso.deleteBody"),
                            confirm: t("common.delete"),
                            danger: true,
                          });
                          if (ok) remove.mutate(p.id);
                        }}
                      >
                        <Trash2 aria-hidden />
                      </Button>
                    </div>
                  </div>
                  {p.redirect_uri && (
                    <div className="grid gap-1 border-t border-line-soft pt-3">
                      <span className="text-xs text-ink-3">{t("sso.redirectUri")}</span>
                      <span className="flex min-w-0 items-center gap-1">
                        <code
                          className="min-w-0 flex-1 truncate rounded-md bg-surface-2 px-2 py-1 font-mono text-xs text-ink-2"
                          title={p.redirect_uri}
                        >
                          {p.redirect_uri}
                        </code>
                        <CopyButton
                          text={p.redirect_uri}
                          size="icon-sm"
                          variant="ghost"
                          label={t("sso.copyRedirect")}
                        />
                      </span>
                    </div>
                  )}
                </li>
              ))}
            </ul>
          )}
        </div>
      )}
      {editing && q.data && (
        <ProviderDrawer
          key={editing === "new" ? "new" : editing.id}
          provider={editing}
          onClose={() => setEditing(null)}
          onSaved={() => {
            refresh();
            setEditing(null);
          }}
        />
      )}
    </>
  );
}

