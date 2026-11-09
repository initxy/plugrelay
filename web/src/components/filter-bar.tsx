import * as M from "@radix-ui/react-dropdown-menu";
import { Check, ChevronDown, Search, X } from "lucide-react";
import { useEffect, useRef, useState, type ReactNode } from "react";
import { cn } from "@/lib/cn";
import { useI18n } from "@/i18n";

/** A row of filter chips above a table. Keep each filter's state in the URL with `useUrlSearch`. */
export function FilterBar({ children, className }: { children: ReactNode; className?: string }) {
  return <div className={cn("flex flex-wrap items-center gap-1.5", className)}>{children}</div>;
}

const chip =
  "inline-flex h-7 items-center gap-1 whitespace-nowrap rounded-full border px-2.5 text-xs transition-colors duration-100 [&_svg]:size-3.5";
const chipIdle = "border-line bg-surface text-ink-2 hover:border-line-strong hover:text-ink";
const chipOn = "border-accent/40 bg-accent-soft font-medium text-accent-strong";

export interface FilterOption<V extends string = string> {
  value: V;
  label: ReactNode;
  /** Text that matches when the menu is long enough to search (defaults to the value). */
  search?: string;
}

/**
 * One filter as a chip: "全部模型 ▾" while unset, "模型：gpt-5.5 ▾ ✕" once chosen.
 * Long option lists get a search box.
 */
export function FilterSelect<V extends string>({
  label,
  allLabel,
  value,
  options,
  onChange,
}: {
  label: string;
  allLabel: string;
  value: V | undefined;
  options: readonly FilterOption<V>[];
  onChange: (v: V | undefined) => void;
}) {
  const { t } = useI18n();
  const [q, setQ] = useState("");
  const current = options.find((o) => o.value === value);
  const searchable = options.length > 8;
  const shown = q ? options.filter((o) => (o.search ?? o.value).toLowerCase().includes(q.toLowerCase())) : options;
  return (
    <span className="inline-flex">
      <M.Root onOpenChange={(o) => !o && setQ("")}>
        <M.Trigger className={cn(chip, current ? chipOn : chipIdle, current && "rounded-r-none border-r-0 pr-1.5")}>
          {current ? (
            <>
              <span className="text-accent-strong/70">{label}</span>
              <span className="max-w-40 truncate">{current.label}</span>
            </>
          ) : (
            allLabel
          )}
          <ChevronDown className="opacity-70" />
        </M.Trigger>
        <M.Portal>
          <M.Content
            align="start"
            sideOffset={4}
            className="z-50 max-h-80 min-w-48 overflow-y-auto rounded-lg border border-line bg-surface p-1 shadow-pop animate-[pop-in_100ms_var(--ease-out-quart)]"
          >
            {searchable && (
              <div className="mb-1 flex items-center gap-1.5 border-b border-line-soft px-2 pb-1.5 pt-1">
                <Search className="size-3.5 text-ink-3" />
                <input
                  autoFocus
                  value={q}
                  onChange={(e) => setQ(e.target.value)}
                  onKeyDown={(e) => e.stopPropagation()}
                  placeholder={t("filter.search")}
                  aria-label={t("filter.search")}
                  className="h-6 flex-1 bg-transparent text-sm outline-none placeholder:text-ink-3"
                />
              </div>
            )}
            <M.RadioGroup value={value ?? ""} onValueChange={(v) => onChange(v === "" ? undefined : (v as V))}>
              <M.RadioItem value="" className={itemClass}>
                <M.ItemIndicator className="absolute left-2">
                  <Check className="size-3.5 text-accent" />
                </M.ItemIndicator>
                {allLabel}
              </M.RadioItem>
              {shown.map((o) => (
                <M.RadioItem key={o.value} value={o.value} className={itemClass}>
                  <M.ItemIndicator className="absolute left-2">
                    <Check className="size-3.5 text-accent" />
                  </M.ItemIndicator>
                  {o.label}
                </M.RadioItem>
              ))}
            </M.RadioGroup>
          </M.Content>
        </M.Portal>
      </M.Root>
      {current && (
        <button
          type="button"
          onClick={() => onChange(undefined)}
          aria-label={`${t("filter.clear")}: ${label}`}
          className={cn(chip, chipOn, "rounded-l-none border-l-0 px-1.5 hover:bg-accent/15")}
        >
          <X />
        </button>
      )}
    </span>
  );
}

const itemClass =
  "relative flex cursor-default select-none items-center rounded-sm py-1.5 pl-7 pr-3 text-sm outline-none data-[highlighted]:bg-surface-2 data-[state=checked]:font-medium";

/** An on/off filter chip: "只看失败". */
export function FilterToggle({ pressed, onChange, children }: { pressed: boolean; onChange: (v: boolean) => void; children: ReactNode }) {
  return (
    <button type="button" aria-pressed={pressed} onClick={() => onChange(!pressed)} className={cn(chip, pressed ? chipOn : chipIdle)}>
      {pressed && <Check />}
      {children}
    </button>
  );
}

/** Free-text filter. Commits after a short pause and on Enter, so typing doesn't push a URL per key. */
export function FilterSearch({
  value,
  onChange,
  placeholder,
  className,
}: {
  value: string | undefined;
  onChange: (v: string | undefined) => void;
  placeholder?: string;
  className?: string;
}) {
  const { t } = useI18n();
  const [text, setText] = useState(value ?? "");
  const [seen, setSeen] = useState(value);
  if (seen !== value) {
    setSeen(value);
    setText(value ?? "");
  }
  const timer = useRef<ReturnType<typeof setTimeout>>(undefined);
  useEffect(() => () => clearTimeout(timer.current), []);
  const commit = (v: string) => {
    clearTimeout(timer.current);
    const next = v.trim() || undefined;
    if (next !== value) onChange(next);
  };
  return (
    <label
      className={cn(
        "inline-flex h-7 w-56 items-center gap-1.5 rounded-full border border-line bg-surface px-2.5 text-xs text-ink-3 transition-[border-color,box-shadow] duration-100 focus-within:border-accent focus-within:shadow-[0_0_0_3px_var(--ring-soft)]",
        className,
      )}
    >
      <Search className="size-3.5 shrink-0" />
      <input
        type="search"
        value={text}
        placeholder={placeholder ?? t("filter.search")}
        aria-label={placeholder ?? t("filter.search")}
        onChange={(e) => {
          const v = e.target.value;
          setText(v);
          clearTimeout(timer.current);
          timer.current = setTimeout(() => commit(v), 350);
        }}
        onKeyDown={(e) => e.key === "Enter" && commit(text)}
        onBlur={() => commit(text)}
        className="h-full min-w-0 flex-1 bg-transparent text-ink outline-none placeholder:text-ink-3 [&::-webkit-search-cancel-button]:hidden"
      />
    </label>
  );
}

/** "Clear filters (3)", shown only while something is filtered. */
export function ClearFilters({ count, onClear }: { count: number; onClear: () => void }) {
  const { t } = useI18n();
  if (count === 0) return null;
  return (
    <button type="button" onClick={onClear} className="ml-1 inline-flex h-7 items-center gap-1 rounded-md px-1.5 text-xs text-ink-3 hover:text-ink">
      <X className="size-3.5" />
      {t("filter.clear")}
      <span className="tabular-nums">({count})</span>
    </button>
  );
}
