import * as D from "@radix-ui/react-dialog";
import { useQueryClient } from "@tanstack/react-query";
import { Link, Outlet, useNavigate, useRouterState } from "@tanstack/react-router";
import {
  ChevronsUpDown,
  Languages,
  LogOut,
  Menu as MenuIcon,
  Monitor,
  Moon,
  PanelLeftClose,
  PanelLeftOpen,
  Search,
  ShieldCheck,
  Sun,
  X,
} from "lucide-react";
import { useEffect, useMemo, useState, type CSSProperties } from "react";
import { toast } from "sonner";
import { Kbd } from "@/components/ui/kbd";
import {
  Menu,
  MenuContent,
  MenuItem,
  MenuLabel,
  MenuRadioGroup,
  MenuRadioItem,
  MenuSeparator,
  MenuTrigger,
} from "@/components/ui/menu";
import { Tooltip } from "@/components/ui/tooltip";
import { api, unwrap } from "@/lib/api/client";
import { errorMessage } from "@/lib/api/errors";
import { cn } from "@/lib/cn";
import { store } from "@/lib/storage";
import { useTheme, type ThemeChoice } from "@/lib/theme";
import { useMediaQuery } from "@/lib/use-media";
import { LOCALES, useI18n, type Locale } from "@/i18n";
import { AnnouncementBanner } from "@/features/billing/announcement-banner";
import { CommandPalette } from "./command-palette";
import { iconFor } from "./icons";
import { buildMenu, isOperator, menuItems, type NavItem, type NavSection } from "./nav";
import { useMe } from "./session";

const WIDE = 224;
const RAIL = 60;
const SIDEBAR_KEY = "pr.sidebar";

/** The menu item the path belongs to: the longest `to` that is the path or a parent of it. */
function activeItem(items: NavItem[], path: string): string | undefined {
  let best: NavItem | undefined;
  for (const i of items) {
    const hit = i.to === "/" ? path === "/" : path === i.to || path.startsWith(`${i.to}/`);
    if (hit && (!best || i.to.length > best.to.length)) best = i;
  }
  return best?.id;
}

function NavLinks({
  sections,
  rail,
  headings,
  onNavigate,
}: {
  sections: NavSection[];
  rail: boolean;
  headings: boolean;
  onNavigate?: () => void;
}) {
  const { t } = useI18n();
  const path = useRouterState({ select: (s) => s.location.pathname });
  const active = activeItem(menuItems(sections), path);
  return (
    <nav aria-label={t("nav.main")} className="grid gap-0.5">
      {sections.map((s, si) => (
        <div key={s.group} className="grid gap-px">
          {si > 0 &&
            (rail ? (
              <div aria-hidden className="mx-3 my-2 h-px bg-line-soft" />
            ) : headings ? (
              <div className="px-2.5 pb-1 pt-3.5 text-2xs font-medium tracking-[0.06em] text-ink-3">{t(`nav.group.${s.group}` as "nav.group.access")}</div>
            ) : (
              <div aria-hidden className="h-2" />
            ))}
          {s.items.map((item) => {
            const Icon = iconFor(item.icon);
            const on = item.id === active;
            const link = (
              <Link
                key={item.id}
                to={item.to}
                onClick={onNavigate}
                aria-current={on ? "page" : undefined}
                aria-label={rail ? t(item.label) : undefined}
                className={cn(
                  "group flex h-8 items-center gap-2.5 rounded-md text-sm text-ink-2 transition-colors duration-100 hover:bg-surface-2 hover:text-ink focus-visible:outline-offset-0",
                  rail ? "mx-auto w-10 justify-center" : "px-2.5",
                  on && "bg-accent-soft font-semibold text-accent-strong hover:bg-accent-soft hover:text-accent-strong",
                )}
              >
                <Icon className={cn("size-4 shrink-0", on ? "text-accent" : "text-ink-3 group-hover:text-ink-2")} strokeWidth={on ? 2.1 : 1.8} />
                {!rail && <span className="truncate">{t(item.label)}</span>}
              </Link>
            );
            return rail ? (
              <Tooltip key={item.id} side="right" content={t(item.label)}>
                {link}
              </Tooltip>
            ) : (
              link
            );
          })}
        </div>
      ))}
    </nav>
  );
}

function Brand({ rail }: { rail: boolean }) {
  const me = useMe();
  return (
    <Link to="/" className={cn("flex min-w-0 items-center gap-2.5 rounded-md py-1", rail ? "justify-center" : "px-1.5")} aria-label={me.site.name}>
      <img
        src={me.site.logo_url || "/favicon.svg"}
        alt=""
        onError={(e) => {
          if (!e.currentTarget.src.endsWith("/favicon.svg")) e.currentTarget.src = "/favicon.svg";
        }}
        className="size-[22px] shrink-0 rounded-[6px]"
      />
      {!rail && <span className="truncate text-base font-semibold tracking-[-0.01em]">{me.site.name}</span>}
    </Link>
  );
}

function Avatar({ name }: { name: string }) {
  return (
    <span aria-hidden className="grid size-7 shrink-0 place-items-center rounded-full bg-accent-soft text-xs font-semibold text-accent-strong">
      {name.slice(0, 1).toUpperCase()}
    </span>
  );
}

function UserMenu({ rail }: { rail: boolean }) {
  const me = useMe();
  const { t, locale, setLocale } = useI18n();
  const { choice, setChoice } = useTheme();
  const qc = useQueryClient();
  const navigate = useNavigate();
  const name = me.user.display_name ?? me.user.username;
  const logout = async () => {
    try {
      await unwrap(api.POST("/auth/logout"));
      qc.clear();
      await navigate({ to: "/login" });
    } catch (e) {
      toast.error(errorMessage(e));
    }
  };
  return (
    <Menu>
      <MenuTrigger
        aria-label={t("shell.account")}
        className={cn(
          "flex w-full items-center gap-2.5 rounded-md text-left hover:bg-surface-2 data-[state=open]:bg-surface-2",
          rail ? "justify-center py-1.5" : "px-1.5 py-1.5",
        )}
      >
        <Avatar name={name} />
        {!rail && (
          <>
            <span className="min-w-0 flex-1 leading-tight">
              <span className="block truncate text-sm font-medium">{name}</span>
              <span className="block truncate text-2xs text-ink-3">{t(`role.${me.user.role}`)}</span>
            </span>
            <ChevronsUpDown className="size-3.5 shrink-0 text-ink-3" />
          </>
        )}
      </MenuTrigger>
      <MenuContent align="start">
        <MenuLabel>
          <span className="block text-sm font-medium text-ink">{name}</span>
          <span className="block">
            {me.user.username} · {t(`role.${me.user.role}`)}
          </span>
        </MenuLabel>
        <MenuSeparator />
        <MenuLabel>{t("shell.theme")}</MenuLabel>
        <MenuRadioGroup value={choice} onValueChange={(v) => setChoice(v as ThemeChoice)}>
          <MenuRadioItem value="system">
            <Monitor className="size-3.5 text-ink-3" />
            {t("shell.themeSystem")}
          </MenuRadioItem>
          <MenuRadioItem value="light">
            <Sun className="size-3.5 text-ink-3" />
            {t("shell.themeLight")}
          </MenuRadioItem>
          <MenuRadioItem value="dark">
            <Moon className="size-3.5 text-ink-3" />
            {t("shell.themeDark")}
          </MenuRadioItem>
        </MenuRadioGroup>
        <MenuSeparator />
        <MenuLabel>
          <span className="inline-flex items-center gap-1.5">
            <Languages className="size-3" />
            {t("shell.language")}
          </span>
        </MenuLabel>
        <MenuRadioGroup value={locale} onValueChange={(v) => setLocale(v as Locale)}>
          {LOCALES.map((l) => (
            <MenuRadioItem key={l.value} value={l.value}>
              <span lang={l.value}>{l.label}</span>
            </MenuRadioItem>
          ))}
        </MenuRadioGroup>
        {me.auth === "session" && (
          <>
            <MenuSeparator />
            <MenuItem onSelect={() => void navigate({ to: "/security" })}>
              <ShieldCheck />
              {t("security.title")}
            </MenuItem>
            <MenuItem onSelect={() => void logout()}>
              <LogOut />
              {t("shell.logout")}
            </MenuItem>
          </>
        )}
      </MenuContent>
    </Menu>
  );
}

function SearchButton({ rail, onClick }: { rail: boolean; onClick: () => void }) {
  const { t } = useI18n();
  const mac = typeof navigator !== "undefined" && /mac|iphone|ipad/i.test(navigator.platform || navigator.userAgent);
  if (rail)
    return (
      <Tooltip side="right" content={`${t("shell.search")} · ${mac ? "⌘" : "Ctrl "}K`}>
        <button type="button" onClick={onClick} aria-label={t("shell.search")} className="mx-auto grid size-10 h-8 place-items-center rounded-md text-ink-3 hover:bg-surface-2 hover:text-ink">
          <Search className="size-4" />
        </button>
      </Tooltip>
    );
  return (
    <button
      type="button"
      onClick={onClick}
      className="flex h-8 w-full items-center gap-2 rounded-md border border-line bg-bg px-2.5 text-sm text-ink-3 transition-colors hover:border-line-strong hover:text-ink-2"
    >
      <Search className="size-3.5" />
      <span className="flex-1 truncate text-left">{t("shell.search")}</span>
      <Kbd>{mac ? "⌘K" : "Ctrl K"}</Kbd>
    </button>
  );
}

function Sidebar({
  sections,
  rail,
  headings,
  onSearch,
  onNavigate,
  onToggle,
}: {
  sections: NavSection[];
  rail: boolean;
  headings: boolean;
  onSearch: () => void;
  onNavigate?: () => void;
  onToggle?: () => void;
}) {
  const { t } = useI18n();
  return (
    <div className="flex h-full flex-col">
      <div className={cn("flex h-14 shrink-0 items-center gap-1", rail ? "justify-center px-2" : "px-3")}>
        <div className="min-w-0 flex-1">
          <Brand rail={rail} />
        </div>
        {onToggle && !rail && (
          <Tooltip content={t("shell.collapse")}>
            <button type="button" onClick={onToggle} aria-label={t("shell.collapse")} className="grid size-7 place-items-center rounded-md text-ink-3 hover:bg-surface-2 hover:text-ink">
              <PanelLeftClose className="size-4" />
            </button>
          </Tooltip>
        )}
      </div>
      <div className={cn("shrink-0 pb-2", rail ? "px-2" : "px-3")}>
        <SearchButton rail={rail} onClick={onSearch} />
      </div>
      <div className={cn("min-h-0 flex-1 overflow-y-auto pb-3", rail ? "px-2" : "px-3")}>
        <NavLinks sections={sections} rail={rail} headings={headings} onNavigate={onNavigate} />
      </div>
      <div className={cn("shrink-0 border-t border-line-soft py-2", rail ? "px-2" : "px-3")}>
        {onToggle && rail && (
          <Tooltip side="right" content={t("shell.expand")}>
            <button type="button" onClick={onToggle} aria-label={t("shell.expand")} className="mx-auto mb-1 grid h-8 w-10 place-items-center rounded-md text-ink-3 hover:bg-surface-2 hover:text-ink">
              <PanelLeftOpen className="size-4" />
            </button>
          </Tooltip>
        )}
        <UserMenu rail={rail} />
      </div>
    </div>
  );
}

/**
 * The signed-in layout. Wide screens: a sidebar the reader can fold into an icon rail.
 * Tablets: the rail. Phones: a top bar with the menu in a slide-over.
 */
export function AppShell() {
  const me = useMe();
  const { t } = useI18n();
  const sections = useMemo(() => buildMenu(me), [me]);
  const headings = isOperator(me);
  const [paletteOpen, setPaletteOpen] = useState(false);
  const [mobileNav, setMobileNav] = useState(false);
  const [folded, setFolded] = useState(() => store.get(SIDEBAR_KEY) === "rail");
  const wide = useMediaQuery("(min-width: 1024px)");
  const rail = !wide || folded;
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "k") {
        e.preventDefault();
        setPaletteOpen((o) => !o);
      }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, []);

  const toggle = () =>
    setFolded((f) => {
      store.set(SIDEBAR_KEY, f ? "wide" : "rail");
      return !f;
    });

  return (
    <div className="min-h-dvh md:grid md:grid-cols-[var(--sidebar-w)_minmax(0,1fr)]" style={{ "--sidebar-w": `${rail ? RAIL : WIDE}px` } as CSSProperties}>
      <a
        href="#content"
        className="sr-only z-[70] rounded-md bg-surface px-3 py-2 text-sm font-medium shadow-pop focus:not-sr-only focus:fixed focus:left-3 focus:top-3"
      >
        {t("shell.skip")}
      </a>
      <aside className="sticky top-0 hidden h-dvh border-r border-line bg-surface md:block">
        <Sidebar sections={sections} rail={rail} headings={headings} onSearch={() => setPaletteOpen(true)} onToggle={wide ? toggle : undefined} />
      </aside>

      <header className="sticky top-0 z-30 flex h-12 items-center gap-1 border-b border-line bg-surface/95 px-2 backdrop-blur md:hidden">
        <button type="button" className="grid size-9 place-items-center rounded-md hover:bg-surface-2" onClick={() => setMobileNav(true)} aria-label={t("shell.openMenu")}>
          <MenuIcon className="size-5" />
        </button>
        <div className="min-w-0 flex-1">
          <Brand rail={false} />
        </div>
        <button type="button" className="grid size-9 place-items-center rounded-md hover:bg-surface-2" onClick={() => setPaletteOpen(true)} aria-label={t("shell.search")}>
          <Search className="size-5" />
        </button>
      </header>
      <D.Root open={mobileNav} onOpenChange={setMobileNav}>
        <D.Portal>
          <D.Overlay className="fixed inset-0 z-40 bg-scrim animate-[fade-in_120ms_ease-out] md:hidden" />
          <D.Content className="fixed inset-y-0 left-0 z-50 w-[272px] max-w-[85vw] border-r border-line bg-surface shadow-pop outline-none animate-[slide-in-left_180ms_var(--ease-out-quart)] md:hidden">
            <D.Title className="sr-only">{t("nav.main")}</D.Title>
            <D.Description className="sr-only">{me.site.name}</D.Description>
            <D.Close className="absolute right-2.5 top-3.5 grid size-7 place-items-center rounded-md text-ink-3 hover:bg-surface-2" aria-label={t("common.close")}>
              <X className="size-4" />
            </D.Close>
            <Sidebar sections={sections} rail={false} headings={headings} onSearch={() => setPaletteOpen(true)} onNavigate={() => setMobileNav(false)} />
          </D.Content>
        </D.Portal>
      </D.Root>

      <main id="content" tabIndex={-1} className="min-w-0 px-4 py-5 outline-none md:px-6 md:py-6 xl:px-8">
        <div className="mx-auto w-full max-w-[1480px]">
          {me.mfa?.required && !me.mfa.enabled && (
            <p role="status" className="mb-4 rounded-md border border-warn/30 bg-warn-soft px-3 py-2 text-sm text-warn">
              {t("security.banner")}{" "}
              <Link to="/security" className="font-medium underline">
                {t("security.bannerLink")}
              </Link>
            </p>
          )}
          <AnnouncementBanner />
          <Outlet />
        </div>
      </main>
      <CommandPalette open={paletteOpen} onOpenChange={setPaletteOpen} items={menuItems(sections)} />
    </div>
  );
}
