import { useQuery } from "@tanstack/react-query";
import { createColumnHelper } from "@tanstack/react-table";
import { KeyRound, MoreHorizontal, Pencil, Plug, Plus, Power, RotateCw, SearchX, Trash2 } from "lucide-react";
import { useMemo, useRef, useState } from "react";
import { DataTable } from "@/components/data-table";
import { ClearFilters, FilterBar, FilterSearch, FilterSelect } from "@/components/filter-bar";
import { useMoney } from "@/components/money";
import { EmptyState, ErrorState, PageHeader, Panel } from "@/components/page";
import { StatTile } from "@/components/stat-tile";
import { Button } from "@/components/ui/button";
import { Menu, MenuContent, MenuItem, MenuSeparator, MenuTrigger } from "@/components/ui/menu";
import { Select } from "@/components/ui/select";
import { BalanceBadge } from "@/features/billing/member/balance-badge";
import { isOperator } from "@/app/nav";
import { useMe } from "@/app/session";
import { api, unwrap } from "@/lib/api/client";
import type { Key, KeyCreated } from "@/lib/api/types";
import { formatRelative } from "@/lib/format";
import { useDrawerParam, useUrlSearch } from "@/lib/use-url-search";
import { useI18n } from "@/i18n";
import { ConnectPanel } from "./connect-panel";
import { KeyDrawer } from "./key-drawer";
import { KeyStatusPill, LimitChips, SecretDialog, SpendCell, maskedKey, useKeyActions } from "./key-parts";

const col = createColumnHelper<Key>();
const STATUSES = ["active", "disabled", "expired", "over_budget"] as const;
type Status = (typeof STATUSES)[number];
const str = (v: unknown) => (typeof v === "string" && v !== "" ? v : undefined);
const IDLE_DAYS = 14;

function nearBudget(k: Key): boolean {
  const b = k.settings.budget_monthly;
  return (
    k.status === "over_budget" || (b !== undefined && b.micro > 0 && k.month_spend.micro / b.micro >= 0.8)
  );
}

/** Keys of the caller (or, for admins, everyone's), what each may do, and copy-ready client setup. */
export function KeysPage() {
  const { t, locale } = useI18n();
  const me = useMe();
  const admin = isOperator(me);
  const money = useMoney();
  const drawer = useDrawerParam();
  const [search, set] = useUrlSearch();
  const q = str(search.q);
  const status = STATUSES.find((s) => s === search.status) as Status | undefined;
  const owner = str(search.owner);
  const keys = useQuery({ queryKey: ["keys"], queryFn: () => unwrap(api.GET("/keys")) });
  const items = useMemo(() => keys.data?.items ?? [], [keys.data]);

  const [created, setCreated] = useState<KeyCreated | null>(null);
  const [secret, setSecret] = useState<{ id: string; secret: string } | null>(null);
  const connectRef = useRef<HTMLDivElement>(null);
  const usable = items.filter((k) => k.status === "active" || k.status === "over_budget");
  const connectId = str(search.connect) ?? usable[0]?.id;
  const connectKey = items.find((k) => k.id === connectId) ?? usable[0];

  const goConnect = (k: Key) => {
    drawer.close();
    set({ connect: k.id, id: undefined }, { replace: true });
    requestAnimationFrame(() => connectRef.current?.scrollIntoView({ behavior: "smooth", block: "start" }));
  };
  const actions = useKeyActions({ onSecret: setCreated });

  const shown = items.filter(
    (k) =>
      (!status || k.status === status) &&
      (!owner || k.owner?.id === owner) &&
      (!q ||
        `${k.name} ${k.last4} ${k.owner?.name ?? ""} ${k.group?.name ?? ""}`
          .toLowerCase()
          .includes(q.toLowerCase())),
  );
  const owners = [
    ...new Map(items.filter((k) => k.owner).map((k) => [k.owner!.id, k.owner!.name])).entries(),
  ];
  const filters = [q, status, owner].filter(Boolean).length;

  const totalSpend = items.reduce((s, k) => s + k.month_spend.micro, 0);
  const [now] = useState(() => Date.now());
  const idle = items.filter(
    (k) =>
      k.status === "active" &&
      (!k.last_used_at || now - new Date(k.last_used_at).getTime() > IDLE_DAYS * 86_400_000),
  );
  const near = items.filter(nearBudget);

  const columns = useMemo(
    () => [
      col.accessor("name", {
        header: t("keys.name"),
        cell: (c) => (
          <div className="min-w-0 py-1">
            <div className="truncate font-medium">{c.getValue()}</div>
            <div className="font-mono text-[11.5px] text-ink-3">{maskedKey(c.row.original)}</div>
          </div>
        ),
        meta: { className: "min-w-40" },
      }),
      ...(admin
        ? [
            col.accessor((k) => k.owner?.name ?? "—", {
              id: "owner",
              header: t("keys.owner"),
              cell: (c) => (
                <div className="min-w-0">
                  <div className="truncate">{c.getValue()}</div>
                  <div className="truncate text-xs text-ink-3">
                    {c.row.original.group?.name ?? t("keys.noGroup")}
                  </div>
                </div>
              ),
            }),
          ]
        : []),
      col.accessor("status", { header: t("keys.status"), cell: (c) => <KeyStatusPill k={c.row.original} /> }),
      col.display({
        id: "limits",
        header: t("keys.limits"),
        cell: (c) => <LimitChips k={c.row.original} />,
        meta: { label: t("keys.limits"), className: "max-lg:hidden" },
      }),
      col.accessor("last_used_at", {
        header: t("keys.lastUsed"),
        meta: { className: "max-lg:hidden" },
        cell: (c) =>
          c.getValue() ? (
            <span className="whitespace-nowrap text-ink-2">{formatRelative(c.getValue()!, locale)}</span>
          ) : (
            <span className="text-ink-3">{t("keys.never")}</span>
          ),
      }),
      col.display({
        id: "spend",
        header: t("keys.monthSpend"),
        cell: (c) => <SpendCell k={c.row.original} />,
        meta: { numeric: true, label: t("keys.monthSpend") },
      }),
      col.display({
        id: "actions",
        header: () => <span className="sr-only">{t("common.more")}</span>,
        enableHiding: false,
        meta: { className: "w-0" },
        cell: (c) => {
          const k = c.row.original;
          return (
            <div
              className="flex items-center justify-end gap-0.5"
              onClick={(e) => e.stopPropagation()}
              onKeyDown={(e) => e.stopPropagation()}
            >
              <Button
                size="sm"
                variant="ghost"
                onClick={() => goConnect(k)}
                disabled={k.status === "disabled" || k.status === "expired"}
              >
                <Plug />
                {t("keys.connect")}
              </Button>
              <Menu>
                <MenuTrigger asChild>
                  <Button size="icon-sm" variant="ghost" aria-label={t("keys.moreFor", { name: k.name })}>
                    <MoreHorizontal />
                  </Button>
                </MenuTrigger>
                <MenuContent>
                  <MenuItem onSelect={() => drawer.open(k.id)}>
                    <Pencil />
                    {t("keys.openDetails")}
                  </MenuItem>
                  <MenuItem onSelect={() => void actions.rotate(k)}>
                    <RotateCw />
                    {t("keys.rotate")}
                  </MenuItem>
                  <MenuItem onSelect={() => void actions.toggle(k)} disabled={k.status === "expired"}>
                    <Power />
                    {k.status === "disabled" ? t("keys.enable") : t("keys.disable")}
                  </MenuItem>
                  <MenuSeparator />
                  <MenuItem danger onSelect={() => void actions.remove(k)}>
                    <Trash2 />
                    {t("common.delete")}
                  </MenuItem>
                </MenuContent>
              </Menu>
            </div>
          );
        },
      }),
    ],
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [t, locale, admin],
  );

  return (
    <>
      <PageHeader
        title={t("nav.keys")}
        description={admin ? t("keys.description") : t("keys.descriptionMember")}
        actions={
          <>
            <BalanceBadge />
            <Button variant="primary" onClick={() => drawer.open("new")}>
              <Plus />
              {t("keys.create")}
            </Button>
          </>
        }
      />

      <div className="mb-4 grid grid-cols-2 gap-3 lg:grid-cols-4">
        <StatTile
          label={t("keys.statActive")}
          loading={keys.isPending}
          value={usable.length}
          note={t("keys.statOf", { n: items.length })}
          onClick={() => set({ status: status === "active" ? undefined : "active" })}
        />
        <StatTile
          label={t("keys.statSpend")}
          loading={keys.isPending}
          value={money({ micro: totalSpend, currency: me.site.currency })}
          note={admin ? t("keys.statSpendAll") : t("keys.statSpendMine")}
        />
        <StatTile
          label={t("keys.statNear")}
          loading={keys.isPending}
          value={<span className={near.length ? "text-warn" : undefined}>{near.length}</span>}
          note={t("keys.statNearNote")}
          onClick={near.length ? () => set({ status: "over_budget" }) : undefined}
        />
        <StatTile
          label={t("keys.statIdle")}
          loading={keys.isPending}
          value={idle.length}
          note={t("keys.statIdleNote", { n: IDLE_DAYS })}
        />
      </div>

      {keys.isError ? (
        <ErrorState error={keys.error} onRetry={() => void keys.refetch()} />
      ) : (
        <div className="grid gap-2.5">
          <DataTable
            toolbar={
              <FilterBar>
                <FilterSearch
                  value={q}
                  onChange={(v) => set({ q: v }, { replace: true })}
                  placeholder={admin ? t("keys.searchAdmin") : t("keys.search")}
                />
                <FilterSelect
                  label={t("keys.status")}
                  allLabel={t("keys.allStatuses")}
                  value={status}
                  onChange={(v) => set({ status: v })}
                  options={STATUSES.map((s) => ({ value: s, label: t(`keyStatus.${s}`) }))}
                />
                {admin && owners.length > 1 && (
                  <FilterSelect
                    label={t("keys.owner")}
                    allLabel={t("keys.allOwners")}
                    value={owner}
                    onChange={(v) => set({ owner: v })}
                    options={owners.map(([id, name]) => ({ value: id, label: name, search: name }))}
                  />
                )}
                <ClearFilters
                  count={filters}
                  onClear={() => set({ q: undefined, status: undefined, owner: undefined })}
                />
              </FilterBar>
            }
            id="keys"
            columns={columns}
            data={keys.isPending ? undefined : shown}
            getRowId={(k) => k.id}
            onRowClick={(k) => drawer.open(k.id)}
            selectedId={drawer.id}
            loadingRows={5}
            columnPicker
            maxHeight="none"
            empty={
              items.length === 0 ? (
                <EmptyState
                  icon={KeyRound}
                  title={t("keys.empty")}
                  hint={t("keys.emptyHint")}
                  action={
                    <Button variant="primary" onClick={() => drawer.open("new")}>
                      <Plus />
                      {t("keys.create")}
                    </Button>
                  }
                />
              ) : (
                <EmptyState compact icon={SearchX} title={t("keys.noMatch")} hint={t("keys.noMatchHint")} />
              )
            }
            footer={items.length > 0 ? t("keys.count", { n: shown.length, of: items.length }) : undefined}
          />
        </div>
      )}

      <div ref={connectRef} className="mt-6 scroll-mt-4">
        <Panel title={t("keys.connectTitle")} aside={t("keys.connectAside")} bodyClassName="p-4 sm:p-5">
          {keys.isPending ? (
            <div className="h-40" aria-busy="true" />
          ) : connectKey ? (
            <ConnectPanel
              key={connectKey.id}
              keyId={connectKey.id}
              secret={secret?.id === connectKey.id ? secret.secret : undefined}
              keyPicker={
                <Select
                  aria-label={t("connect.stepKey")}
                  value={connectKey.id}
                  onChange={(id) => set({ connect: id }, { replace: true })}
                  options={(usable.some((k) => k.id === connectKey.id)
                    ? usable
                    : [connectKey, ...usable]
                  ).map((k) => ({
                    value: k.id,
                    label: admin && k.owner ? `${k.name} · ${k.owner.name}` : `${k.name} · …${k.last4}`,
                  }))}
                />
              }
            />
          ) : (
            <EmptyState
              compact
              icon={Plug}
              title={t("keys.connectNoKey")}
              hint={t("keys.connectNoKeyHint")}
              action={
                <Button variant="primary" onClick={() => drawer.open("new")}>
                  <Plus />
                  {t("keys.create")}
                </Button>
              }
            />
          )}
        </Panel>
      </div>

      <KeyDrawer
        id={drawer.id}
        keys={keys.data?.items}
        loading={keys.isPending}
        onClose={drawer.close}
        onSecret={setCreated}
        onConnect={goConnect}
      />
      <SecretDialog
        created={created}
        onDone={(connect) => {
          if (created) {
            setSecret({ id: created.key.id, secret: created.secret });
            if (connect) goConnect(created.key);
          }
          setCreated(null);
        }}
      />
    </>
  );
}
