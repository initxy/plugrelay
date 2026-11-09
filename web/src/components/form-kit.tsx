import { useId, useState, type InputHTMLAttributes, type ReactNode } from "react";
import { Eye, EyeOff } from "lucide-react";
import { Input } from "@/components/ui/input";
import { Select } from "@/components/ui/select";
import { Switch } from "@/components/ui/switch";
import { cn } from "@/lib/cn";
import { parseDuration, splitDuration, toGoDuration, type DurationUnit } from "@/lib/duration";
import { currencySymbol, moneyInputValue, parseMoneyInput } from "@/lib/money";
import { useI18n } from "@/i18n";

/**
 * A titled group of fields. `split` puts the title and description in a left column on wide
 * screens, the way long settings pages read best.
 */
export function FormSection({
  title,
  description,
  aside,
  layout = "stacked",
  children,
  className,
}: {
  title: ReactNode;
  description?: ReactNode;
  aside?: ReactNode;
  layout?: "stacked" | "split";
  children: ReactNode;
  className?: string;
}) {
  return (
    <section
      className={cn(
        "border-b border-line-soft py-5 first:pt-0 last:border-b-0 last:pb-0",
        layout === "split" && "md:grid md:grid-cols-[minmax(180px,1fr)_2fr] md:gap-8",
        className,
      )}
    >
      <div className={cn("mb-3.5", layout === "split" && "md:mb-0")}>
        <div className="flex items-center justify-between gap-2">
          <h3 className="text-sm font-semibold">{title}</h3>
          {aside}
        </div>
        {description && <p className="mt-0.5 max-w-[60ch] text-xs text-ink-3">{description}</p>}
      </div>
      <div className="grid gap-4">{children}</div>
    </section>
  );
}

/**
 * A text input with a fixed unit after the value ("%", "秒"). Takes the input's props, so a
 * `Field` around it labels the input itself.
 */
export function SuffixInput({ suffix, className, ...props }: InputHTMLAttributes<HTMLInputElement> & { suffix: ReactNode }) {
  return (
    <div className={cn("relative", className)}>
      <Input {...props} className="pr-9 tabular-nums" />
      <span aria-hidden className="pointer-events-none absolute inset-y-0 right-2.5 flex items-center text-xs text-ink-3">
        {suffix}
      </span>
    </div>
  );
}

/** Help under a control or section, in the quiet tone. */
export function HelpText({ children, className }: { children: ReactNode; className?: string }) {
  return <p className={cn("text-xs text-ink-3", className)}>{children}</p>;
}

/**
 * An amount of money in currency units (12.50, never micro-units). Accepts what people type in
 * either decimal style; `value` is null when empty. Invalid text stays visible and is flagged.
 */
export function MoneyInput({
  value,
  onChange,
  currency,
  id,
  placeholder,
  disabled,
  className,
  ...aria
}: {
  value: number | null | undefined;
  onChange: (v: number | null) => void;
  currency: string;
  id?: string;
  placeholder?: string;
  disabled?: boolean;
  className?: string;
  "aria-invalid"?: boolean;
  "aria-describedby"?: string;
  "aria-label"?: string;
}) {
  const { locale } = useI18n();
  const shown = value === null || value === undefined ? "" : moneyInputValue(Math.round(value * 1_000_000));
  const [text, setText] = useState(shown);
  const [seen, setSeen] = useState(value);
  if (seen !== value) {
    setSeen(value);
    if (parseMoneyInput(text, locale) !== (value === null || value === undefined ? null : Math.round(value * 1_000_000))) setText(shown);
  }
  const bad = text.trim() !== "" && parseMoneyInput(text, locale) === null;
  return (
    <div className={cn("relative", className)}>
      <span aria-hidden className="pointer-events-none absolute inset-y-0 left-2.5 flex items-center text-sm text-ink-3">
        {currencySymbol(currency, locale)}
      </span>
      <Input
        id={id}
        inputMode="decimal"
        autoComplete="off"
        disabled={disabled}
        placeholder={placeholder}
        value={text}
        {...aria}
        aria-invalid={bad || aria["aria-invalid"] || undefined}
        onChange={(e) => {
          setText(e.target.value);
          const micro = parseMoneyInput(e.target.value, locale);
          if (e.target.value.trim() === "") onChange(null);
          else if (micro !== null) onChange(micro / 1_000_000);
        }}
        className="pl-7 tabular-nums"
      />
    </div>
  );
}

/**
 * A duration as a number plus a unit. `value` is a Go duration string ("15m", "48h"); null when empty.
 */
export function DurationInput({
  value,
  onChange,
  units = ["s", "m", "h", "d"],
  id,
  disabled,
  className,
  ...aria
}: {
  value: string | null | undefined;
  onChange: (v: string | null) => void;
  units?: readonly DurationUnit[];
  id?: string;
  disabled?: boolean;
  className?: string;
  "aria-invalid"?: boolean;
  "aria-describedby"?: string;
  "aria-label"?: string;
}) {
  const { t } = useI18n();
  const seconds = value ? parseDuration(value) : null;
  const split = seconds === null ? null : splitDuration(seconds);
  const fallbackUnit = units.includes("m") ? "m" : units[0]!;
  const [unit, setUnit] = useState<DurationUnit>(split && units.includes(split.unit) ? split.unit : fallbackUnit);
  const [text, setText] = useState(split ? String(split.amount) : "");
  const [seen, setSeen] = useState(value);
  if (seen !== value) {
    setSeen(value);
    const mine = text.trim() === "" ? null : toGoDuration(Number(text), unit);
    if (mine === null || parseDuration(mine) !== seconds) {
      setText(split ? String(split.amount) : "");
      if (split && units.includes(split.unit)) setUnit(split.unit);
    }
  }
  const emit = (amount: string, u: DurationUnit) => {
    if (amount.trim() === "") return onChange(null);
    const n = Number(amount);
    if (Number.isFinite(n) && n >= 0) onChange(toGoDuration(n, u));
  };
  const bad = text.trim() !== "" && !(Number.isFinite(Number(text)) && Number(text) >= 0);
  const unitId = useId();
  return (
    <div className={cn("flex gap-1.5", className)}>
      <Input
        id={id}
        inputMode="decimal"
        disabled={disabled}
        value={text}
        {...aria}
        aria-invalid={bad || aria["aria-invalid"] || undefined}
        onChange={(e) => {
          setText(e.target.value);
          emit(e.target.value, unit);
        }}
        className="w-24 tabular-nums"
      />
      <Select
        id={unitId}
        aria-label={t("duration.unit")}
        disabled={disabled}
        value={unit}
        onChange={(u) => {
          setUnit(u);
          emit(text, u);
        }}
        options={units.map((u) => ({ value: u, label: t(`duration.${u}`) }))}
        className="w-24"
      />
    </div>
  );
}

/** A setting that is just on or off: label and description on the left, the switch on the right. */
export function SwitchRow({
  label,
  description,
  checked,
  onChange,
  disabled,
  aside,
  className,
}: {
  label: ReactNode;
  description?: ReactNode;
  checked: boolean;
  onChange: (v: boolean) => void;
  disabled?: boolean;
  aside?: ReactNode;
  className?: string;
}) {
  const id = useId();
  return (
    <div className={cn("flex items-start justify-between gap-4", disabled && "opacity-60", className)}>
      <div className="min-w-0">
        <label htmlFor={id} className="flex flex-wrap items-center gap-1.5 text-sm font-medium">
          {label}
          {aside}
        </label>
        {description && (
          <p id={`${id}-d`} className="mt-0.5 text-xs text-ink-3">
            {description}
          </p>
        )}
      </div>
      <Switch id={id} checked={checked} onChange={onChange} disabled={disabled} aria-describedby={description ? `${id}-d` : undefined} className="mt-0.5" />
    </div>
  );
}

/** A password box with a show/hide toggle. */
export function PasswordInput({ id, value, onChange, autoComplete, ...aria }: { id?: string; value: string; onChange: (v: string) => void; autoComplete: string; "aria-invalid"?: boolean; "aria-describedby"?: string }) {
  const { t } = useI18n();
  const [show, setShow] = useState(false);
  return (
    <div className="relative">
      <Input id={id} type={show ? "text" : "password"} autoComplete={autoComplete} value={value} onChange={(e) => onChange(e.target.value)} className="h-10 pr-10" {...aria} />
      <button
        type="button"
        onClick={() => setShow((s) => !s)}
        aria-label={show ? t("form.hide") : t("form.show")}
        aria-pressed={show}
        className="absolute inset-y-0 right-0 grid w-10 place-items-center rounded-r-md text-ink-3 hover:text-ink"
      >
        {show ? <EyeOff className="size-4" /> : <Eye className="size-4" />}
      </button>
    </div>
  );
}

