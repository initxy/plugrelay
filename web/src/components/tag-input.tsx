import { X } from "lucide-react";
import { useId, useMemo, useRef, useState, type ClipboardEvent, type KeyboardEvent, type ReactNode } from "react";
import { cn } from "@/lib/cn";
import { useI18n } from "@/i18n";

export interface TagSuggestion {
  value: string;
  /** Shown to the right of the value: a display name, a price. */
  hint?: ReactNode;
}

/**
 * A list of short values as removable chips: model names, IP ranges, client names. Enter, comma
 * or a pasted list adds; Backspace on an empty box removes the last chip. `suggestions` open as
 * you type (arrow keys + Enter pick one); anything else typed is still accepted unless `validate`
 * says why not, in which case the chip is kept and marked so the person sees what to fix.
 */
export function TagInput({
  value,
  onChange,
  suggestions = [],
  validate,
  placeholder,
  mono,
  id,
  disabled,
  className,
  ...aria
}: {
  value: readonly string[];
  onChange: (v: string[]) => void;
  suggestions?: readonly TagSuggestion[];
  /** Why an entry is not acceptable, or undefined when it is. */
  validate?: (v: string) => string | undefined;
  placeholder?: string;
  mono?: boolean;
  id?: string;
  disabled?: boolean;
  className?: string;
  "aria-invalid"?: boolean;
  "aria-describedby"?: string;
  "aria-label"?: string;
}) {
  const { t } = useI18n();
  const [text, setText] = useState("");
  const [open, setOpen] = useState(false);
  const [active, setActive] = useState(0);
  const input = useRef<HTMLInputElement>(null);
  const listId = useId();

  const shown = useMemo(() => {
    const q = text.trim().toLowerCase();
    return suggestions.filter((s) => !value.includes(s.value) && (!q || s.value.toLowerCase().includes(q))).slice(0, 8);
  }, [suggestions, text, value]);

  const add = (raw: string[]) => {
    const next = [...value];
    for (const r of raw.map((x) => x.trim()).filter(Boolean)) if (!next.includes(r)) next.push(r);
    if (next.length !== value.length) onChange(next);
    setText("");
    setActive(0);
  };
  const remove = (v: string) => onChange(value.filter((x) => x !== v));

  const onKeyDown = (e: KeyboardEvent<HTMLInputElement>) => {
    const listOpen = open && shown.length > 0;
    if (e.key === "ArrowDown" && listOpen) {
      e.preventDefault();
      setActive((i) => (i + 1) % shown.length);
    } else if (e.key === "ArrowUp" && listOpen) {
      e.preventDefault();
      setActive((i) => (i - 1 + shown.length) % shown.length);
    } else if (e.key === "Enter" || e.key === "," || (e.key === "Tab" && text.trim())) {
      if (e.key === "Enter" && listOpen && shown[active]) {
        e.preventDefault();
        add([shown[active].value]);
      } else if (text.trim()) {
        e.preventDefault();
        add([text]);
      } else if (e.key === "Enter") {
        e.preventDefault();
      }
    } else if (e.key === "Backspace" && text === "" && value.length) {
      remove(value[value.length - 1]!);
    } else if (e.key === "Escape" && listOpen) {
      e.stopPropagation();
      setOpen(false);
    }
  };
  const onPaste = (e: ClipboardEvent<HTMLInputElement>) => {
    const pasted = e.clipboardData.getData("text");
    if (/[\n,\s]/.test(pasted.trim())) {
      e.preventDefault();
      add(pasted.split(/[\n,\s]+/));
    }
  };

  return (
    <div className={cn("relative", className)}>
      <div
        onClick={() => input.current?.focus()}
        className={cn(
          "flex min-h-8 w-full cursor-text flex-wrap items-center gap-1 rounded-md border border-line-strong bg-surface px-1.5 py-1 shadow-xs transition-[border-color,box-shadow] duration-100 hover:border-ink-3/60 focus-within:border-accent focus-within:shadow-[0_0_0_3px_var(--ring-soft)]",
          aria["aria-invalid"] && "border-bad",
          disabled && "pointer-events-none bg-surface-2 opacity-70",
        )}
      >
        {value.map((v) => {
          const problem = validate?.(v);
          return (
            <span
              key={v}
              title={problem}
              className={cn(
                "inline-flex h-6 max-w-full items-center gap-1 rounded-sm pl-2 pr-1 text-xs",
                mono && "font-mono",
                problem ? "bg-bad-soft text-bad" : "bg-surface-2 text-ink",
              )}
            >
              <span className="truncate">{v}</span>
              <button
                type="button"
                onClick={(e) => {
                  e.stopPropagation();
                  remove(v);
                }}
                aria-label={t("tags.remove", { name: v })}
                className="grid size-4 shrink-0 place-items-center rounded-xs text-ink-3 hover:bg-line hover:text-ink"
              >
                <X className="size-3" />
              </button>
            </span>
          );
        })}
        <input
          ref={input}
          id={id}
          value={text}
          disabled={disabled}
          placeholder={value.length ? undefined : placeholder}
          role="combobox"
          aria-expanded={open && shown.length > 0}
          aria-controls={listId}
          aria-autocomplete="list"
          aria-activedescendant={open && shown[active] ? `${listId}-${active}` : undefined}
          {...aria}
          onChange={(e) => {
            setText(e.target.value);
            setOpen(true);
            setActive(0);
          }}
          onFocus={() => setOpen(true)}
          onBlur={() => {
            setOpen(false);
            if (text.trim()) add([text]);
          }}
          onKeyDown={onKeyDown}
          onPaste={onPaste}
          className={cn("h-6 min-w-24 flex-1 bg-transparent px-1 text-sm text-ink outline-none placeholder:text-ink-3", mono && "font-mono text-[12.5px]")}
        />
      </div>
      {open && shown.length > 0 && (
        <ul
          id={listId}
          role="listbox"
          className="absolute inset-x-0 top-full z-30 mt-1 max-h-64 overflow-y-auto rounded-lg border border-line bg-surface p-1 shadow-pop"
        >
          {shown.map((s, i) => (
            <li
              key={s.value}
              id={`${listId}-${i}`}
              role="option"
              aria-selected={i === active}
              onMouseDown={(e) => {
                e.preventDefault();
                add([s.value]);
              }}
              onMouseEnter={() => setActive(i)}
              className={cn("flex cursor-default items-center justify-between gap-3 rounded-sm px-2 py-1.5 text-sm", i === active && "bg-surface-2")}
            >
              <span className={cn("truncate", mono && "font-mono text-[12.5px]")}>{s.value}</span>
              {s.hint && <span className="shrink-0 text-xs text-ink-3">{s.hint}</span>}
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}
