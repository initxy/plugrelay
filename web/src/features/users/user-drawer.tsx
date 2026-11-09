import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { ArrowUpRight, KeyRound, UserRound } from "lucide-react";
import { useState } from "react";
import { toast } from "sonner";
import { DetailList, Drawer } from "@/components/drawer";
import { FormSection, SwitchRow } from "@/components/form-kit";
import { LayerSettingsTab } from "@/components/layer-settings-tab";
import { useMoney } from "@/components/money";
import { EmptyState, ErrorState, LoadingRows } from "@/components/page";
import { Pill } from "@/components/status";
import { Button } from "@/components/ui/button";
import { Field } from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import { Segmented } from "@/components/ui/segmented";
import { Select } from "@/components/ui/select";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { useReauth } from "@/features/auth/use-reauth";
import { KeyStatusPill, LimitChips, SpendCell } from "@/features/keys/key-parts";
import { api, unwrap } from "@/lib/api/client";
import { errorMessage } from "@/lib/api/errors";
import type { Role, User } from "@/lib/api/types";
import { formatDateTime, formatRelative } from "@/lib/format";
import { useForm } from "@/lib/use-form";
import { useDiscardCheck } from "@/lib/use-unsaved-guard";
import { useI18n } from "@/i18n";
import { DeleteUserButton, LinkedIdentities, MfaRow, userName } from "./user-account";

export const ROLES: Role[] = ["admin", "member", "user"];
const NONE = "__none";
type Tab = "profile" | "keys" | "settings";

function initialValues(user: User | undefined) {
  return {
    username: user?.username ?? "",
    display_name: user?.display_name ?? "",
    password: "",
    role: (user?.role ?? "member") as Role,
    group_id: user?.group?.id ?? NONE,
    active: user ? user.status === "active" : true,
  };
}

function UserKeys({ user }: { user: User }) {
  const { t, locale } = useI18n();
  const q = useQuery({
    queryKey: ["keys", { user: user.id }],
    queryFn: () => unwrap(api.GET("/keys", { params: { query: { user_id: user.id } } })),
  });
  if (q.isError) return <ErrorState error={q.error} onRetry={() => void q.refetch()} />;
  if (q.isPending) return <LoadingRows rows={3} />;
  if (q.data.items.length === 0) return <EmptyState compact icon={KeyRound} title={t("users.noKeys")} hint={t("users.noKeysHint")} />;
  return (
    <ul className="divide-y divide-line-soft rounded-lg border border-line">
      {q.data.items.map((k) => (
        <li key={k.id}>
          <Link
            to="/keys"
            search={{ id: k.id }}
            className="grid grid-cols-[minmax(0,1fr)_auto] items-center gap-x-4 gap-y-1.5 px-3.5 py-2.5 outline-none hover:bg-surface-2/60 focus-visible:bg-accent-soft/60"
          >
            <div className="flex min-w-0 items-center gap-2">
              <span className="truncate font-medium">{k.name}</span>
              <KeyStatusPill k={k} />
            </div>
            <SpendCell k={k} />
            <LimitChips k={k} />
            <span className="text-right text-xs text-ink-3">{k.last_used_at ? formatRelative(k.last_used_at, locale) : t("keys.never")}</span>
          </Link>
        </li>
      ))}
    </ul>
  );
}

/** One user: profile and access, their keys, and their own settings layer. `id = "new"` adds one. */
export function UserDrawer({
  id,
  users,
  loading,
  onClose,
  onCreated,
}: {
  id: string | undefined;
  users: User[] | undefined;
  loading: boolean;
  onClose: () => void;
  onCreated: (u: User) => void;
}) {
  const { t, locale } = useI18n();
  const money = useMoney();
  const qc = useQueryClient();
  const discard = useDiscardCheck();
  const isNew = id === "new";
  const user = isNew ? undefined : users?.find((u) => u.id === id);
  const groups = useQuery({ queryKey: ["groups"], queryFn: () => unwrap(api.GET("/groups")), enabled: id !== undefined });
  // Creating an admin, password, role and status changes and two-step resets need a recent re-authentication.
  const security = useQuery({ queryKey: ["me", "security"], queryFn: () => unwrap(api.GET("/me/security")), retry: false, enabled: id !== undefined });
  const { run, dialog: reauthDialog } = useReauth(security.data);
  const form = useForm(initialValues(undefined));
  const [tab, setTab] = useState<Tab>("profile");
  const [settingsDirty, setSettingsDirty] = useState(false);
  const stamp = isNew ? "new" : user ? `${user.id}|${user.role}|${user.status}|${user.group?.id ?? ""}|${user.display_name ?? ""}` : undefined;
  const [seen, setSeen] = useState<string | undefined>();
  if (stamp !== seen) {
    const same = seen?.split("|")[0] === stamp?.split("|")[0];
    setSeen(stamp);
    if (!same) setTab("profile");
    if (!same || !form.dirty) form.reset(initialValues(user));
  }
  const v = form.values;

  const save = useMutation({
    mutationFn: () => {
      const common = {
        display_name: v.display_name.trim() || undefined,
        password: v.password || undefined,
        role: v.role,
        group_id: v.group_id === NONE ? undefined : v.group_id,
      };
      const errs: Record<string, string> = {};
      if (isNew && !v.username.trim()) errs.username = t("form.required");
      if (v.password && v.password.length < 10) errs.password = t("setup.passwordShort");
      if (Object.keys(errs).length) {
        form.setErrors(errs);
        return Promise.reject(new Error(t("form.fixErrors")));
      }
      if (isNew) return run(() => unwrap(api.POST("/users", { body: { username: v.username.trim(), ...common } })));
      return run(() => unwrap(api.PATCH("/users/{id}", { params: { path: { id: user!.id } }, body: { ...common, status: v.active ? "active" : "disabled" } })));
    },
    onSuccess: (u) => {
      void qc.invalidateQueries({ queryKey: ["users"] });
      void qc.invalidateQueries({ queryKey: ["groups"] });
      toast.success(t("users.saved", { name: userName(u) }));
      form.reset({ ...v, password: "" });
      if (isNew) {
        qc.setQueryData<{ items: User[] }>(["users"], (prev) => (prev ? { ...prev, items: [...prev.items, u] } : prev));
        onCreated(u);
      }
    },
    onError: (e) => {
      form.absorb(e);
      toast.error(errorMessage(e));
    },
  });

  const requestClose = async () => {
    if (await discard(form.dirty || settingsDirty)) {
      setSettingsDirty(false);
      onClose();
    }
  };
  const open = id !== undefined;
  const missing = open && !isNew && !user && !loading;

  const profile = (
    <form
      className="p-5"
      onSubmit={(e) => {
        e.preventDefault();
        save.mutate();
      }}
    >
      <FormSection title={t("users.sectionAccount")}>
        <div className="grid gap-4 sm:grid-cols-2">
          <Field label={t("users.username")} hint={isNew ? t("users.usernameHint") : undefined} error={form.errors.username}>
            <Input value={v.username} onChange={(e) => form.set("username", e.target.value)} disabled={!isNew} autoComplete="off" autoFocus={isNew} />
          </Field>
          <Field label={t("users.displayName")} error={form.errors.display_name} optional>
            <Input value={v.display_name} onChange={(e) => form.set("display_name", e.target.value)} />
          </Field>
        </div>
        <Field
          label={isNew ? t("users.password") : t("users.resetPassword")}
          hint={isNew ? t("users.passwordHintNew") : t("users.passwordHintEdit")}
          error={form.errors.password}
          optional={!isNew}
        >
          <Input type="password" autoComplete="new-password" value={v.password} onChange={(e) => form.set("password", e.target.value)} className="max-w-80" />
        </Field>
      </FormSection>
      <FormSection title={t("users.sectionAccess")}>
        <Field label={t("users.role")} hint={t(`roleHint.${v.role}`)}>
          <Segmented aria-label={t("users.role")} value={v.role} onChange={(r) => form.set("role", r)} options={ROLES.map((r) => ({ value: r, label: t(`role.${r}`) }))} />
        </Field>
        <Field label={t("users.group")} hint={t("users.groupHint")}>
          <Select
            value={v.group_id}
            onChange={(g) => form.set("group_id", g)}
            options={[{ value: NONE, label: t("users.noGroup") }, ...(groups.data?.items ?? []).map((g) => ({ value: g.id, label: g.name }))]}
            className="max-w-80"
          />
        </Field>
        {!isNew && <SwitchRow label={t("users.active")} description={t("users.activeHint")} checked={v.active} onChange={(c) => form.set("active", c)} />}
      </FormSection>
      {user && (
        <FormSection title={t("users.sectionSignIn")}>
          <MfaRow user={user} run={run} />
          <div className="grid gap-1.5">
            <div>
              <div className="text-sm font-medium">{t("users.sso")}</div>
              <p className="text-xs text-ink-3">{t("users.ssoHint")}</p>
            </div>
            <LinkedIdentities user={user} />
          </div>
        </FormSection>
      )}
    </form>
  );

  const formFooter = (
    <>
      {user && <DeleteUserButton user={user} run={run} onDeleted={onClose} />}
      {form.dirty && <span className="text-xs text-warn">{t("unsaved.badge")}</span>}
      <Button onClick={() => void requestClose()}>{t("common.cancel")}</Button>
      <Button variant="primary" onClick={() => save.mutate()} disabled={save.isPending || (!isNew && !form.dirty)}>
        {save.isPending ? t("common.saving") : isNew ? t("users.addSubmit") : t("common.save")}
      </Button>
    </>
  );

  return (
    <Tabs value={tab} onValueChange={(x) => setTab(x as Tab)}>
      <Drawer
        open={open}
        onOpenChange={(o) => !o && void requestClose()}
        onRequestClose={() => void requestClose()}
        size="lg"
        title={isNew ? t("users.add") : user ? userName(user) : missing ? t("users.notFound") : t("common.loading")}
        subtitle={isNew ? t("users.addHint") : user ? [user.username, user.group?.name].filter(Boolean).join(" · ") : undefined}
        meta={
          user && (
            <>
              <Pill tone={user.role === "admin" ? "brand" : "neutral"}>{t(`role.${user.role}`)}</Pill>
              {user.status === "disabled" && (
                <Pill tone="neutral" dot>
                  {t("userStatus.disabled")}
                </Pill>
              )}
            </>
          )
        }
        tabs={
          user && (
            <TabsList>
              <TabsTrigger value="profile">{t("users.tabProfile")}</TabsTrigger>
              <TabsTrigger value="keys">
                {t("users.tabKeys")}
                {user.key_count ? <span className="ml-1.5 text-xs text-ink-3 tabular-nums">{user.key_count}</span> : null}
              </TabsTrigger>
              <TabsTrigger value="settings">{t("users.tabSettings")}</TabsTrigger>
            </TabsList>
          )
        }
        footer={isNew || tab === "profile" ? formFooter : undefined}
      >
        {missing ? (
          <EmptyState icon={UserRound} title={t("users.notFound")} hint={t("users.notFoundHint")} />
        ) : isNew ? (
          profile
        ) : user ? (
          <>
            <TabsContent value="profile">
              <div className="grid grid-cols-2 gap-3 px-5 pt-5 sm:grid-cols-3">
                <div className="rounded-lg border border-line px-3.5 py-2.5">
                  <div className="text-xs text-ink-3">{t("users.monthSpend")}</div>
                  <div className="mt-0.5 text-lg font-semibold tabular-nums">{money(user.month_spend)}</div>
                </div>
                {user.balance && (
                  <div className="rounded-lg border border-line px-3.5 py-2.5">
                    <div className="text-xs text-ink-3">{t("users.balance")}</div>
                    <div className={user.balance.micro <= 0 ? "mt-0.5 text-lg font-semibold tabular-nums text-bad" : "mt-0.5 text-lg font-semibold tabular-nums"}>
                      {money(user.balance)}
                    </div>
                  </div>
                )}
                <div className="rounded-lg border border-line px-3.5 py-2.5">
                  <div className="text-xs text-ink-3">{t("users.lastSeen")}</div>
                  <div className="mt-0.5 text-lg font-semibold">{user.last_seen_at ? formatRelative(user.last_seen_at, locale) : t("keys.never")}</div>
                </div>
              </div>
              <DetailList
                className="px-5 pt-4"
                items={[
                  { label: t("users.email"), value: user.email ?? "—" },
                  { label: t("users.created"), value: formatDateTime(user.created_at, locale) },
                ]}
              />
              {profile}
            </TabsContent>
            <TabsContent value="keys" className="grid gap-3 p-5">
              <UserKeys user={user} />
              <Link to="/keys" search={{ owner: user.id }} className="inline-flex items-center gap-1 justify-self-start text-sm text-accent hover:underline">
                {t("users.keysOnPage")}
                <ArrowUpRight className="size-3.5" />
              </Link>
            </TabsContent>
            <TabsContent value="settings">
              <LayerSettingsTab layer="user" id={user.id} hint={t("users.settingsHint")} saved={t("users.saved", { name: userName(user) })} onDirty={setSettingsDirty} />
            </TabsContent>
          </>
        ) : (
          <div className="p-5">
            <LoadingRows rows={6} />
          </div>
        )}
      </Drawer>
      {reauthDialog}
    </Tabs>
  );
}

