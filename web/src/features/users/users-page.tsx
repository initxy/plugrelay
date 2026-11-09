import { useQuery } from "@tanstack/react-query";
import { createColumnHelper } from "@tanstack/react-table";
import { Layers, Plus, SearchX, Trash2, UserPlus, Users } from "lucide-react";
import { useMemo, useState } from "react";
import { DataTable } from "@/components/data-table";
import { ClearFilters, FilterBar, FilterSearch, FilterSelect } from "@/components/filter-bar";
import { useMoney } from "@/components/money";
import { EmptyState, ErrorState, PageHeader } from "@/components/page";
import { StatTile } from "@/components/stat-tile";
import { Pill, StatusText } from "@/components/status";
import { Button } from "@/components/ui/button";
import { Tabs, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { Tooltip } from "@/components/ui/tooltip";
import { useFeatureOn } from "@/app/session";
import { api, unwrap } from "@/lib/api/client";
import type { Group, User } from "@/lib/api/types";
import { formatRelative } from "@/lib/format";
import { useDrawerParam, useUrlSearch } from "@/lib/use-url-search";
import { useI18n } from "@/i18n";
import { GroupDrawer, useDeleteGroup } from "./group-drawer";
import { MfaIcon, userName } from "./user-account";
import { ROLES, UserDrawer } from "./user-drawer";

const ucol = createColumnHelper<User>();
const gcol = createColumnHelper<Group>();
const str = (v: unknown) => (typeof v === "string" && v !== "" ? v : undefined);
const WEEK = 7 * 86_400_000;

function UsersTable({
  users,
  groups,
  loading,
}: {
  users: User[] | undefined;
  groups: Group[];
  loading: boolean;
}) {
  const { t, locale } = useI18n();
  const money = useMoney();
  const billing = useFeatureOn("billing");
  const drawer = useDrawerParam();
  const [search, set] = useUrlSearch();
  const q = str(search.q);
  const role = ROLES.find((r) => r === search.role);
  const inGroup = str(search.in_group);
  const showBalance = billing && (users ?? []).some((u) => u.balance);
  const columns = useMemo(
    () => [
      ucol.accessor((u) => userName(u), {
        id: "user",
        header: t("users.user"),
        cell: (c) => {
          const u = c.row.original;
          return (
            <div className="flex min-w-0 items-center gap-2.5 py-1">
              <span className="grid size-7 shrink-0 place-items-center rounded-full bg-surface-2 text-xs font-medium text-ink-2">
                {userName(u).slice(0, 1).toUpperCase()}
              </span>
              <div className="min-w-0">
                <div className="flex items-center gap-1.5">
                  <span className="truncate font-medium">{userName(u)}</span>
                  <MfaIcon on={u.mfa_enabled} />
                </div>
                <div className="truncate text-xs text-ink-3">{u.username}</div>
              </div>
            </div>
          );
        },
        meta: { className: "min-w-48" },
      }),
      ucol.accessor("role", {
        header: t("users.role"),
        cell: (c) => (
          <Pill tone={c.getValue() === "admin" ? "brand" : "neutral"}>{t(`role.${c.getValue()}`)}</Pill>
        ),
      }),
      ucol.accessor((u) => u.group?.name, {
        id: "group",
        header: t("users.group"),
        cell: (c) =>
          c.getValue() ? (
            <span>{c.getValue()}</span>
          ) : (
            <span className="text-ink-3">{t("users.noGroup")}</span>
          ),
      }),
      ucol.accessor("status", {
        header: t("users.status"),
        cell: (c) => (
          <StatusText tone={c.getValue() === "active" ? "ok" : "neutral"}>
            {t(`userStatus.${c.getValue()}`)}
          </StatusText>
        ),
      }),
      ucol.accessor((u) => u.key_count ?? 0, {
        id: "keys",
        header: t("users.keys"),
        meta: { numeric: true },
      }),
      ucol.accessor((u) => u.month_spend.micro, {
        id: "spend",
        header: t("users.monthSpend"),
        cell: (c) => money(c.row.original.month_spend),
        meta: { numeric: true },
      }),
      ...(showBalance
        ? [
            ucol.accessor((u) => u.balance?.micro ?? null, {
              id: "balance",
              header: t("users.balance"),
              cell: (c) => {
                const b = c.row.original.balance;
                return b ? (
                  <span className={b.micro <= 0 ? "text-bad" : undefined}>{money(b)}</span>
                ) : (
                  <span className="text-ink-3">—</span>
                );
              },
              meta: { numeric: true },
            }),
          ]
        : []),
      ucol.accessor("last_seen_at", {
        header: t("users.lastSeen"),
        cell: (c) =>
          c.getValue() ? (
            <span className="whitespace-nowrap text-ink-2">{formatRelative(c.getValue()!, locale)}</span>
          ) : (
            <span className="text-ink-3">{t("keys.never")}</span>
          ),
      }),
    ],
    [t, locale, money, showBalance],
  );
  const shown = (users ?? []).filter(
    (u) =>
      (!role || u.role === role) &&
      (!inGroup || (inGroup === "none" ? !u.group : u.group?.id === inGroup)) &&
      (!q ||
        `${u.username} ${u.display_name ?? ""} ${u.email ?? ""}`.toLowerCase().includes(q.toLowerCase())),
  );
  const filters = [q, role, inGroup].filter(Boolean).length;
  return (
    <div className="grid gap-2.5">
      <DataTable
        toolbar={
          <FilterBar>
            <FilterSearch
              value={q}
              onChange={(v) => set({ q: v }, { replace: true })}
              placeholder={t("users.search")}
            />
            <FilterSelect
              label={t("users.role")}
              allLabel={t("users.allRoles")}
              value={role}
              onChange={(v) => set({ role: v })}
              options={ROLES.map((r) => ({ value: r, label: t(`role.${r}`) }))}
            />
            <FilterSelect
              label={t("users.group")}
              allLabel={t("users.allGroups")}
              value={inGroup}
              onChange={(v) => set({ in_group: v })}
              options={[
                { value: "none", label: t("users.noGroup") },
                ...groups.map((g) => ({ value: g.id, label: g.name, search: g.name })),
              ]}
            />
            <ClearFilters
              count={filters}
              onClear={() => set({ q: undefined, role: undefined, in_group: undefined })}
            />
          </FilterBar>
        }
        id="users"
        columns={columns}
        data={loading ? undefined : shown}
        getRowId={(u) => u.id}
        onRowClick={(u) => drawer.open(u.id)}
        selectedId={drawer.id}
        loadingRows={5}
        columnPicker
        maxHeight="none"
        empty={
          (users ?? []).length === 0 ? (
            <EmptyState icon={Users} title={t("users.empty")} hint={t("users.emptyHint")} />
          ) : (
            <EmptyState compact icon={SearchX} title={t("users.noMatch")} hint={t("users.noMatchHint")} />
          )
        }
        footer={
          (users ?? []).length > 0 ? t("users.count", { n: shown.length, of: users!.length }) : undefined
        }
      />
    </div>
  );
}

function GroupsTable({
  groups,
  loading,
  onAdd,
}: {
  groups: Group[] | undefined;
  loading: boolean;
  onAdd: () => void;
}) {
  const { t } = useI18n();
  const drawer = useDrawerParam();
  const remove = useDeleteGroup();
  const columns = useMemo(
    () => [
      gcol.accessor("name", {
        header: t("groups.name"),
        cell: (c) => (
          <div className="min-w-0 py-1">
            <div className="font-medium">{c.getValue()}</div>
            {c.row.original.description && (
              <div className="truncate text-xs text-ink-3">{c.row.original.description}</div>
            )}
          </div>
        ),
        meta: { className: "min-w-48" },
      }),
      gcol.accessor("member_count", { header: t("groups.members"), meta: { numeric: true } }),
      gcol.accessor("models", {
        header: t("groups.models"),
        cell: (c) => {
          const m = c.getValue();
          if (m.length === 0) return <span className="text-ink-2">{t("groups.allModels")}</span>;
          return (
            <Tooltip content={m.join(", ")}>
              <div className="flex max-w-80 flex-wrap gap-1">
                {m.slice(0, 3).map((x) => (
                  <span
                    key={x}
                    className="inline-flex h-5 items-center rounded-xs bg-surface-2 px-1.5 font-mono text-2xs text-ink-2"
                  >
                    {x}
                  </span>
                ))}
                {m.length > 3 && <span className="text-2xs text-ink-3">+{m.length - 3}</span>}
              </div>
            </Tooltip>
          );
        },
      }),
      gcol.accessor((g) => g.price_multiplier ?? 1, {
        id: "multiplier",
        header: t("groups.multiplierShort"),
        cell: (c) => (
          <span className={c.getValue() !== 1 ? "font-medium text-accent-strong" : "text-ink-2"}>
            ×{c.getValue()}
          </span>
        ),
        meta: { numeric: true },
      }),
      gcol.accessor((g) => Boolean(g.log_bodies), {
        id: "bodies",
        header: t("groups.bodiesShort"),
        cell: (c) => (
          <StatusText tone={c.getValue() ? "info" : "neutral"}>
            {c.getValue() ? t("groups.bodiesOn") : t("groups.bodiesOff")}
          </StatusText>
        ),
      }),
      gcol.display({
        id: "actions",
        header: () => <span className="sr-only">{t("common.more")}</span>,
        enableHiding: false,
        meta: { className: "w-0" },
        cell: (c) => (
          <Button
            size="icon-sm"
            variant="ghost"
            aria-label={t("groups.deleteNamed", { name: c.row.original.name })}
            onClick={(e) => {
              e.stopPropagation();
              void remove(c.row.original);
            }}
          >
            <Trash2 />
          </Button>
        ),
      }),
    ],
    [t, remove],
  );
  return (
    <DataTable
      id="groups"
      columns={columns}
      data={loading ? undefined : groups}
      getRowId={(g) => g.id}
      onRowClick={(g) => drawer.open(g.id)}
      selectedId={drawer.id}
      loadingRows={3}
      maxHeight="none"
      empty={
        <EmptyState
          icon={Layers}
          title={t("groups.empty")}
          hint={t("groups.emptyHint")}
          action={
            <Button variant="primary" onClick={onAdd}>
              <Plus />
              {t("groups.add")}
            </Button>
          }
        />
      }
    />
  );
}

/** Who can sign in, what role they have, and the groups that set most of their limits and prices. */
export function UsersPage() {
  const { t } = useI18n();
  const money = useMoney();
  const [search, set] = useUrlSearch();
  const tab = search.tab === "groups" ? "groups" : "users";
  // `?id=` is a user on the users tab and a group on the groups tab.
  const drawer = useDrawerParam();
  const users = useQuery({ queryKey: ["users"], queryFn: () => unwrap(api.GET("/users")) });
  const groups = useQuery({ queryKey: ["groups"], queryFn: () => unwrap(api.GET("/groups")) });
  const list = users.data?.items ?? [];
  const spend = list.reduce((s, u) => s + u.month_spend.micro, 0);
  const currency = list[0]?.month_spend.currency;
  const [now] = useState(() => Date.now());
  const active = list.filter((u) => u.last_seen_at && now - new Date(u.last_seen_at).getTime() < WEEK).length;
  const admins = list.filter((u) => u.role === "admin").length;
  const disabled = list.filter((u) => u.status === "disabled").length;

  return (
    <>
      <PageHeader
        title={t("nav.users")}
        description={tab === "groups" ? t("groups.description") : t("users.description")}
        actions={
          tab === "groups" ? (
            <Button variant="primary" onClick={() => drawer.open("new")}>
              <Plus />
              {t("groups.add")}
            </Button>
          ) : (
            <Button variant="primary" onClick={() => drawer.open("new")}>
              <UserPlus />
              {t("users.add")}
            </Button>
          )
        }
      />
      <div className="mb-4 grid grid-cols-2 gap-3 lg:grid-cols-4">
        <StatTile
          label={t("users.statUsers")}
          loading={users.isPending}
          value={list.length}
          note={t("users.statAdmins", { n: admins })}
        />
        <StatTile
          label={t("users.statActive")}
          loading={users.isPending}
          value={active}
          note={disabled ? t("users.statDisabled", { n: disabled }) : t("users.statActiveNote")}
        />
        <StatTile
          label={t("users.statSpend")}
          loading={users.isPending}
          value={currency ? money({ micro: spend, currency }) : "—"}
          note={t("users.statSpendNote")}
        />
        <StatTile
          label={t("users.statGroups")}
          loading={groups.isPending}
          value={groups.data?.items.length ?? 0}
          note={t("users.statGroupsNote", { n: list.filter((u) => !u.group).length })}
          onClick={() => set({ tab: "groups", id: undefined, q: undefined, role: undefined, in_group: undefined })}
        />
      </div>
      <Tabs value={tab} onValueChange={(v) => set({ tab: v === "groups" ? "groups" : undefined, id: undefined })}>
        <TabsList className="mb-3 px-0">
          <TabsTrigger value="users">
            {t("users.tabUsers")}
            <span className="ml-1.5 text-xs text-ink-3 tabular-nums">{users.data ? list.length : ""}</span>
          </TabsTrigger>
          <TabsTrigger value="groups">
            {t("users.tabGroups")}
            <span className="ml-1.5 text-xs text-ink-3 tabular-nums">{groups.data?.items.length ?? ""}</span>
          </TabsTrigger>
        </TabsList>
      </Tabs>
      {tab === "users" ? (
        users.isError ? (
          <ErrorState error={users.error} onRetry={() => void users.refetch()} />
        ) : (
          <UsersTable users={users.data?.items} groups={groups.data?.items ?? []} loading={users.isPending} />
        )
      ) : groups.isError ? (
        <ErrorState error={groups.error} onRetry={() => void groups.refetch()} />
      ) : (
        <GroupsTable
          groups={groups.data?.items}
          loading={groups.isPending}
          onAdd={() => drawer.open("new")}
        />
      )}
      <UserDrawer
        id={tab === "users" ? drawer.id : undefined}
        users={users.data?.items}
        loading={users.isPending}
        onClose={drawer.close}
        onCreated={(u) => drawer.open(u.id)}
      />
      <GroupDrawer
        id={tab === "groups" ? drawer.id : undefined}
        groups={groups.data?.items}
        loading={groups.isPending}
        onClose={drawer.close}
        onCreated={(g) => drawer.open(g.id)}
      />
    </>
  );
}
