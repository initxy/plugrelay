import { Link } from "@tanstack/react-router";
import { ArrowLeft, Search, SquareDashed, X } from "lucide-react";
import { useMemo } from "react";
import { Drawer } from "@/components/drawer";
import { EmptyState, ErrorState, PageHeader } from "@/components/page";
import { Skeleton } from "@/components/ui/skeleton";
import type { Manifest } from "@/lib/api/types";
import { cn } from "@/lib/cn";
import { pickEnum, pickString } from "@/lib/url-filters";
import { useMediaQuery } from "@/lib/use-media";
import { useUrlSearch } from "@/lib/use-url-search";
import { useI18n, type Locale } from "@/i18n";
import type { MessageKey } from "@/i18n/en";
import { CATEGORIES, manifestTitle, POPULAR, type Category } from "./labels";
import { ProviderAvatar } from "./provider-avatar";
import { useManifests } from "@/lib/api/queries";
import { AddPanel, CUSTOM, useTagline } from "./add-upstream-form";

function searchText(m: Manifest, locale: Locale) {
  return [m.name, m.title, m.title_zh ?? "", manifestTitle(m, locale)].join(" ").toLowerCase();
}

export function AddUpstreamPage() {
  const { t } = useI18n();
  const [search, setSearch] = useUrlSearch();
  const q = pickString(search.q) ?? "";
  const cat = pickEnum(search.cat, CATEGORIES);
  const selected = pickString(search.m);
  const wide = useMediaQuery("(min-width: 1180px)");
  const manifests = useManifests();
  const items = useMemo(() => manifests.data?.items ?? [], [manifests.data]);
  const chosen = selected === CUSTOM ? CUSTOM : items.find((m) => m.name === selected);

  const counts = useMemo(() => {
    const c = Object.fromEntries(CATEGORIES.map((k) => [k, 0])) as Record<Category, number>;
    for (const m of items) c[m.category] += 1;
    c.custom += 1; // the custom endpoint card
    return c;
  }, [items]);

  const select = (name: string | undefined) => setSearch({ m: name }, { replace: true });
  const panel = chosen ? <AddPanel key={selected} manifest={chosen} onClose={() => select(undefined)} /> : null;

  return (
    <>
      <PageHeader
        title={
          <span className="flex items-center gap-2">
            <Link to="/upstreams" className="grid size-7 place-items-center rounded-md text-ink-3 hover:bg-surface-2 hover:text-ink" aria-label={t("gallery.back")}>
              <ArrowLeft className="size-4" />
            </Link>
            {t("upstreams.add")}
          </span>
        }
        description={t("gallery.description", { n: items.length })}
      />
      <div className={cn("grid items-start gap-5", wide && "grid-cols-[minmax(0,1fr)_380px]")}>
        <div className="min-w-0">
          <label className="flex h-10 items-center gap-2 rounded-lg border border-line-strong bg-surface px-3 shadow-xs focus-within:border-accent focus-within:shadow-[0_0_0_3px_var(--ring-soft)]">
            <Search className="size-4 text-ink-3" />
            <input
              type="search"
              autoFocus
              value={q}
              onChange={(e) => setSearch({ q: e.target.value }, { replace: true })}
              placeholder={t("gallery.search")}
              aria-label={t("gallery.search")}
              className="h-full min-w-0 flex-1 bg-transparent text-sm outline-none placeholder:text-ink-3 [&::-webkit-search-cancel-button]:hidden"
            />
            {q && (
              <button type="button" onClick={() => setSearch({ q: undefined }, { replace: true })} aria-label={t("filter.clear")} className="text-ink-3 hover:text-ink">
                <X className="size-4" />
              </button>
            )}
          </label>
          <div className="mb-4 mt-3 flex flex-wrap gap-1.5" role="group" aria-label={t("gallery.categories")}>
            <CategoryChip on={!cat} onClick={() => setSearch({ cat: undefined }, { replace: true })} label={t("gallery.all")} n={items.length + 1} />
            {CATEGORIES.map((c) => (
              <CategoryChip key={c} on={cat === c} onClick={() => setSearch({ cat: c }, { replace: true })} label={t(`gallery.cat.${c}` as MessageKey)} n={counts[c]} />
            ))}
          </div>
          {manifests.isError ? (
            <ErrorState error={manifests.error} onRetry={() => void manifests.refetch()} />
          ) : manifests.isPending ? (
            <CardGrid>
              {Array.from({ length: 12 }, (_, i) => (
                <Skeleton key={i} className="h-[58px] rounded-lg" />
              ))}
            </CardGrid>
          ) : (
            <Gallery items={items} q={q} cat={cat} selected={selected} onSelect={select} />
          )}
        </div>
        {wide ? (
          <aside className="sticky top-6 max-h-[calc(100dvh-3rem)] overflow-y-auto rounded-xl border border-line bg-surface shadow-card">
            {panel ?? (
              <EmptyState compact icon={SquareDashed} title={t("gallery.pickTitle")} hint={t("gallery.pickHint")} className="py-16" />
            )}
          </aside>
        ) : (
          <Drawer open={Boolean(chosen)} onOpenChange={(o) => !o && select(undefined)} title={t("upstreams.add")} size="md">
            {panel}
          </Drawer>
        )}
      </div>
    </>
  );
}

function CategoryChip({ on, onClick, label, n }: { on: boolean; onClick: () => void; label: string; n: number }) {
  return (
    <button
      type="button"
      aria-pressed={on}
      onClick={onClick}
      className={cn(
        "inline-flex h-7 items-center gap-1.5 rounded-full border px-3 text-xs transition-colors",
        on ? "border-ink bg-ink font-medium text-bg" : "border-line bg-surface text-ink-2 hover:border-line-strong hover:text-ink",
      )}
    >
      {label}
      <span className={cn("tabular-nums", on ? "text-bg/70" : "text-ink-3")}>{n}</span>
    </button>
  );
}

function CardGrid({ children }: { children: React.ReactNode }) {
  return <div className="grid grid-cols-[repeat(auto-fill,minmax(176px,1fr))] gap-2">{children}</div>;
}

function Gallery({
  items,
  q,
  cat,
  selected,
  onSelect,
}: {
  items: Manifest[];
  q: string;
  cat: Category | undefined;
  selected: string | undefined;
  onSelect: (name: string) => void;
}) {
  const { t, locale } = useI18n();
  const needle = q.trim().toLowerCase();
  const hits = items.filter((m) => (!cat || m.category === cat) && (!needle || searchText(m, locale).includes(needle)));
  const byTitle = (a: Manifest, b: Manifest) => manifestTitle(a, locale).localeCompare(manifestTitle(b, locale), locale);
  const customMatches = (!cat || cat === "custom") && (!needle || t("gallery.customTitle").toLowerCase().includes(needle) || "custom".includes(needle));
  const card = (m: Manifest) => <ProviderCard key={m.name} m={m} on={selected === m.name} onSelect={onSelect} />;
  const customCard = customMatches && <CustomCard key={CUSTOM} on={selected === CUSTOM} onSelect={onSelect} />;

  if (hits.length === 0 && !customMatches)
    return (
      <div className="rounded-lg border border-dashed border-line-strong">
        <EmptyState compact title={t("gallery.noMatch", { q })} hint={t("gallery.noMatchHint")} />
      </div>
    );

  // Searching or filtered: one flat grid.
  if (needle || cat)
    return (
      <CardGrid>
        {[...hits].sort(byTitle).map(card)}
        {customCard}
      </CardGrid>
    );

  const popular = POPULAR.map((n) => items.find((m) => m.name === n)).filter((m): m is Manifest => Boolean(m));
  return (
    <div className="grid gap-6">
      <GallerySection title={t("gallery.popular")}>{popular.map(card)}</GallerySection>
      {CATEGORIES.map((c) => {
        const group = items.filter((m) => m.category === c && !POPULAR.includes(m.name)).sort(byTitle);
        if (group.length === 0 && c !== "custom") return null;
        return (
          <GallerySection key={c} title={t(`gallery.cat.${c}` as MessageKey)}>
            {group.map(card)}
            {c === "custom" && customCard}
          </GallerySection>
        );
      })}
    </div>
  );
}

function GallerySection({ title, children }: { title: string; children: React.ReactNode }) {
  return (
    <section>
      <h2 className="mb-2 text-xs font-semibold tracking-[0.04em] text-ink-3">{title}</h2>
      <CardGrid>{children}</CardGrid>
    </section>
  );
}

const cardClass = (on: boolean) =>
  cn(
    "flex min-w-0 items-center gap-2.5 rounded-lg border bg-surface px-2.5 py-2 text-left transition-[border-color,box-shadow] duration-100",
    on ? "border-accent shadow-[0_0_0_3px_var(--ring-soft)]" : "border-line hover:border-line-strong hover:shadow-card",
  );

function ProviderCard({ m, on, onSelect }: { m: Manifest; on: boolean; onSelect: (name: string) => void }) {
  const { locale } = useI18n();
  const tagline = useTagline();
  const title = manifestTitle(m, locale);
  return (
    <button type="button" aria-pressed={on} onClick={() => onSelect(m.name)} className={cardClass(on)}>
      <ProviderAvatar name={m.name} title={title} />
      <span className="min-w-0">
        <span className="block truncate text-[13px] font-medium" title={title}>
          {title}
        </span>
        <span className="block truncate text-xs text-ink-3">{tagline(m)}</span>
      </span>
    </button>
  );
}

function CustomCard({ on, onSelect }: { on: boolean; onSelect: (name: string) => void }) {
  const { t } = useI18n();
  return (
    <button type="button" aria-pressed={on} onClick={() => onSelect(CUSTOM)} className={cardClass(on)}>
      <span className="grid size-8 shrink-0 place-items-center rounded-lg border border-dashed border-line-strong text-ink-3">
        <SquareDashed className="size-4" />
      </span>
      <span className="min-w-0">
        <span className="block truncate text-[13px] font-medium">{t("gallery.customTitle")}</span>
        <span className="block truncate text-xs text-ink-3">{t("gallery.customTag")}</span>
      </span>
    </button>
  );
}
