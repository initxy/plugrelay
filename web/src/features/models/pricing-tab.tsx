import { useMutation, useQueryClient } from "@tanstack/react-query";
import { createColumnHelper } from "@tanstack/react-table";
import { MoreHorizontal, Plus, Tag, Trash2 } from "lucide-react";
import { useMemo } from "react";
import { toast } from "sonner";
import { useConfirm } from "@/components/confirm";
import { DataTable } from "@/components/data-table";
import { Drawer } from "@/components/drawer";
import { FormSection, MoneyInput, SuffixInput, SwitchRow } from "@/components/form-kit";
import { EmptyState, ErrorState, LoadingRows } from "@/components/page";
import { Pill } from "@/components/status";
import { Button } from "@/components/ui/button";
import { Field } from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import { Menu, MenuContent, MenuItem, MenuTrigger } from "@/components/ui/menu";
import { useMe } from "@/app/session";
import { api, unwrap } from "@/lib/api/client";
import { errorMessage } from "@/lib/api/errors";
import type { PricingRule, TokenPrices } from "@/lib/api/types";
import { formatDateTime } from "@/lib/format";
import { formatPrice } from "@/lib/money";
import { useForm } from "@/lib/use-form";
import { useDiscardCheck, useUnsavedGuard } from "@/lib/use-unsaved-guard";
import { useDrawerParam } from "@/lib/use-url-search";
import { useI18n } from "@/i18n";
import { useModels, usePricing } from "./queries";
import { SuggestionList, SyncBanner, usePriceSync } from "./price-sync";
import { globMatch } from "./lookup";

const BUCKETS = ["input", "cache_read", "cache_write_5m", "cache_write_1h", "output"] as const;
type Bucket = (typeof BUCKETS)[number];

/** A batch discount typed as percent off ("50", "50%", "12,5"); null unless 0 ≤ x < 100. Returns the fraction. */
export function parseDiscountPercent(text: string): number | null {
  const n = Number(text.trim().replace("%", "").replace(",", "."));
  return text.trim() !== "" && Number.isFinite(n) && n >= 0 && n < 100 ? n / 100 : null;
}

function Price({ micro, currency }: { micro: number; currency: string }) {
  const { locale } = useI18n();
  return micro ? <span className="tabular-nums">{formatPrice(micro, currency, locale)}</span> : <span className="text-ink-3">—</span>;
}

const col = createColumnHelper<PricingRule>();

export function PricingTab() {
  const { t, locale } = useI18n();
  const drawer = useDrawerParam();
  const pricing = usePricing();
  const columns = useMemo(
    () => [
      col.accessor("match", {
        header: t("pricing.match"),
        enableHiding: false,
        cell: ({ row: { original: r } }) => <span className="font-mono text-[13px] font-medium">{r.match}</span>,
      }),
      ...BUCKETS.map((b) =>
        col.display({
          id: b,
          header: t(`pricing.bucket.${b}`),
          meta: { numeric: true, className: b === "cache_write_5m" || b === "cache_write_1h" ? "hidden xl:table-cell" : b === "cache_read" ? "hidden lg:table-cell" : undefined, label: t(`pricing.bucket.${b}`) },
          cell: ({ row: { original: r } }) => <Price micro={r.prices[b]} currency={r.prices.currency} />,
        }),
      ),
      col.accessor("source", {
        header: t("pricing.source"),
        meta: { className: "hidden lg:table-cell", label: t("pricing.source") },
        cell: ({ row: { original: r } }) => (
          <span className="text-xs text-ink-2">
            <span className="inline-flex items-center gap-1.5">
              {t(`pricing.src.${r.source}`)}
              {r.follow && <Pill tone="brand">{t("pricing.following")}</Pill>}
            </span>
            {r.updated_at && <span className="block text-ink-3">{formatDateTime(r.updated_at, locale)}</span>}
          </span>
        ),
      }),
    ],
    [t, locale],
  );
  return (
    <>
      {pricing.data && <SyncBanner sync={pricing.data.sync} />}
      {pricing.data && <SuggestionList items={pricing.data.suggestions} />}
      {pricing.isError ? (
        <ErrorState error={pricing.error} onRetry={() => void pricing.refetch()} />
      ) : (
        <DataTable
          id="pricing"
          columns={columns}
          data={pricing.data?.items}
          getRowId={(r) => r.id}
          onRowClick={(r) => drawer.open(r.id)}
          selectedId={drawer.id}
          columnPicker
          empty={
            <EmptyState
              icon={Tag}
              title={t("pricing.empty")}
              hint={t("pricing.emptyHint")}
              action={
                <Button variant="primary" onClick={() => drawer.open("new")}>
                  <Plus />
                  {t("pricing.add")}
                </Button>
              }
            />
          }
          footer={pricing.data && t("pricing.footer")}
        />
      )}
      {drawer.id && <PriceDrawer id={drawer.id} onClose={drawer.close} />}
    </>
  );
}

function PriceDrawer({ id, onClose }: { id: string; onClose: () => void }) {
  const { t } = useI18n();
  const pricing = usePricing();
  const rule = pricing.data?.items.find((r) => r.id === id);
  if (!pricing.data || (id !== "new" && !rule))
    return (
      <Drawer open onOpenChange={(o) => !o && onClose()} title={pricing.data ? t("pricing.notFound") : t("common.loading")}>
        <div className="p-5">{pricing.error ? <ErrorState error={pricing.error} /> : pricing.data ? null : <LoadingRows rows={6} />}</div>
      </Drawer>
    );
  return <PriceBody key={id} rule={rule} onClose={onClose} />;
}

type PriceDraft = Record<Bucket, number | null> & { match: string; discount: string };

function PriceBody({ rule, onClose }: { rule: PricingRule | undefined; onClose: () => void }) {
  const { t } = useI18n();
  const me = useMe();
  const qc = useQueryClient();
  const confirm = useConfirm();
  const discard = useDiscardCheck();
  const models = useModels();
  const isNew = !rule;
  const currency = rule?.prices.currency ?? me.site.currency;
  const form = useForm<PriceDraft>({
    match: rule?.match ?? "",
    discount: rule?.prices.batch_discount ? String(Math.round(rule.prices.batch_discount * 1000) / 10) : "",
    ...(Object.fromEntries(BUCKETS.map((b) => [b, rule ? rule.prices[b] / 1_000_000 : null])) as Record<Bucket, number | null>),
  });
  const d = form.values;
  const matched = (models.data?.items ?? []).filter((m) => d.match.trim() && globMatch(d.match.trim(), m.name)).map((m) => m.name);

  const save = useMutation({
    mutationFn: () => {
      const errs: Record<string, string> = {};
      if (!d.match.trim()) errs.match = t("form.required");
      const prices = { currency, ...(rule?.prices.units ? { units: rule.prices.units } : {}) } as TokenPrices;
      for (const b of BUCKETS) prices[b] = Math.round((d[b] ?? 0) * 1_000_000);
      if (d.discount.trim()) {
        const f = parseDiscountPercent(d.discount);
        if (f === null) errs["prices.batch_discount"] = t("pricing.discountInvalid");
        else if (f > 0) prices.batch_discount = f;
      }
      if (Object.keys(errs).length) {
        form.setErrors(errs);
        return Promise.reject(new Error(t("form.fixErrors")));
      }
      const body = { match: d.match.trim(), prices };
      return isNew ? unwrap(api.POST("/pricing-rules", { body })) : unwrap(api.PATCH("/pricing-rules/{id}", { params: { path: { id: rule.id } }, body }));
    },
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: ["pricing-rules"] });
      toast.success(t("pricing.saved"));
      onClose();
    },
    onError: (e) => {
      form.absorb(e);
      toast.error(errorMessage(e));
    },
  });
  const remove = useMutation({
    mutationFn: () => unwrap(api.DELETE("/pricing-rules/{id}", { params: { path: { id: rule!.id } } })),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: ["pricing-rules"] });
      toast.success(t("pricing.deleted"));
      onClose();
    },
    onError: (e) => toast.error(errorMessage(e)),
  });
  useUnsavedGuard(form.dirty);
  const close = async () => {
    if (await discard(form.dirty)) onClose();
  };

  return (
    <Drawer
      open
      onOpenChange={(o) => !o && void close()}
      onRequestClose={() => void close()}
      title={isNew ? t("pricing.add") : <span className="font-mono">{rule.match}</span>}
      subtitle={t("pricing.unitHint", { currency })}
      actions={
        rule && (
          <Menu>
            <MenuTrigger asChild>
              <Button size="icon-sm" variant="ghost" aria-label={t("common.more")}>
                <MoreHorizontal />
              </Button>
            </MenuTrigger>
            <MenuContent>
              <MenuItem
                danger
                onSelect={async () => {
                  if (await confirm({ title: t("pricing.deleteTitle", { match: rule.match }), body: t("pricing.deleteBody"), confirm: t("common.delete"), danger: true }))
                    remove.mutate();
                }}
              >
                <Trash2 />
                {t("pricing.delete")}
              </MenuItem>
            </MenuContent>
          </Menu>
        )
      }
      footer={
        <>
          {form.dirty && <span className="mr-auto text-xs text-warn">{t("unsaved.badge")}</span>}
          <Button onClick={() => void close()}>{t("common.cancel")}</Button>
          <Button variant="primary" onClick={() => save.mutate()} disabled={save.isPending || (!isNew && !form.dirty)}>
            {save.isPending ? t("common.saving") : t("common.save")}
          </Button>
        </>
      }
    >
      <div className="px-5 py-4">
        <FormSection title={t("pricing.which")}>
          <Field label={t("pricing.match")} hint={t("pricing.matchHint")} error={form.errors.match}>
            <Input value={d.match} onChange={(e) => form.set("match", e.target.value)} disabled={!isNew} className="font-mono" spellCheck={false} autoFocus={isNew} />
          </Field>
          {d.match.trim() && (
            <p className="text-xs text-ink-3">{matched.length ? t("pricing.matches", { models: matched.slice(0, 6).join("、"), n: matched.length }) : t("pricing.matchesNone")}</p>
          )}
        </FormSection>
        <FormSection title={t("pricing.perMillion")} description={t("pricing.perMillionHint")}>
          <div className="grid items-start gap-3 sm:grid-cols-2">
            {BUCKETS.map((b) => (
              <Field key={b} label={t(`pricing.bucket.${b}`)} error={form.errors[`prices.${b}`]}>
                <MoneyInput value={d[b]} onChange={(v) => form.set(b, v)} currency={currency} placeholder="0" />
              </Field>
            ))}
            <Field label={t("pricing.discount")} hint={t("pricing.discountHint")} error={form.errors["prices.batch_discount"]} optional>
              <SuffixInput suffix="%" inputMode="decimal" value={d.discount} onChange={(e) => form.set("discount", e.target.value)} placeholder="50" />
            </Field>
          </div>
          {rule?.prices.units && Object.keys(rule.prices.units).length > 0 && <p className="text-xs text-ink-3">{t("pricing.unitsKept")}</p>}
        </FormSection>
        {rule && rule.follow !== undefined && <FollowSection rule={rule} />}
      </div>
    </Drawer>
  );
}

/** Following models.dev for a model whose own price sync found. */
function FollowSection({ rule }: { rule: PricingRule }) {
  const { t } = useI18n();
  const ps = usePriceSync();
  return (
    <FormSection title={t("pricing.followTitle")}>
      <SwitchRow
        label={t("pricing.follow")}
        description={rule.source === "manual" ? t("pricing.followManual") : t("pricing.followHint")}
        checked={Boolean(rule.follow)}
        onChange={(follow) => ps.follow.mutate({ model: rule.match, follow })}
        disabled={ps.follow.isPending}
      />
    </FormSection>
  );
}
