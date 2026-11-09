import { Lock } from "lucide-react";
import { useState } from "react";
import { Input, Textarea } from "@/components/ui/input";
import { Select } from "@/components/ui/select";
import { Switch } from "@/components/ui/switch";
import type { SettingItem } from "@/lib/api/types";
import { cn } from "@/lib/cn";
import { currencySymbol } from "@/lib/money";
import { useI18n } from "@/i18n";
import { UNIT } from "./layout";

const CURRENCIES = ["USD", "CNY", "EUR", "GBP", "JPY", "HKD", "SGD", "CAD", "AUD", "KRW", "INR", "BRL"];
/** Names of the drivers a relay.yaml can pick; anything else shows as written. */
const DRIVERS: Record<string, string> = { sqlite: "SQLite", postgres: "PostgreSQL", mysql: "MySQL", redis: "Redis" };

/** A read-only value from the config file, shown as text with a lock. */
function FileValue({ item }: { item: SettingItem }) {
  const { t } = useI18n();
  const raw = item.value === null || item.value === undefined || item.value === "" ? "—" : String(item.value);
  const shown = item.key.endsWith(".driver") ? (raw === "memory" ? t("settings.value.memory") : (DRIVERS[raw] ?? raw)) : raw;
  return (
    <div className="flex h-8 max-w-sm items-center gap-2 rounded-md border border-dashed border-line-strong bg-surface-2/60 px-2.5 text-sm text-ink-2">
      <Lock className="size-3.5 shrink-0 text-ink-3" aria-hidden />
      <span data-raw={item.key === "listen" ? "" : undefined} className={cn("truncate", item.key === "listen" && "font-mono text-xs")}>
        {shown}
      </span>
    </div>
  );
}

function CurrencySelect({ id, value, onChange, invalid }: { id: string; value: string; onChange: (v: string) => void; invalid: boolean }) {
  const { locale } = useI18n();
  const names = new Intl.DisplayNames([locale], { type: "currency" });
  const codes = CURRENCIES.includes(value) || !value ? CURRENCIES : [value, ...CURRENCIES];
  return (
    <Select
      id={id}
      aria-invalid={invalid || undefined}
      value={value || undefined}
      onChange={onChange}
      className="max-w-72"
      options={codes.map((c) => ({
        value: c,
        label: (
          <span className="flex items-center gap-2">
            <span className="w-9 font-medium tabular-nums">{c}</span>
            <span className="text-ink-2">
              {currencySymbol(c, locale)} · {names.of(c)}
            </span>
          </span>
        ),
      }))}
    />
  );
}

function AccentInput({ id, value, onChange, invalid }: { id: string; value: string; onChange: (v: string) => void; invalid: boolean }) {
  const { t } = useI18n();
  const hex = /^#[0-9a-f]{6}$/i.test(value) ? value : "#0d6b73";
  return (
    <div className="flex max-w-72 items-center gap-2">
      <label className="relative size-8 shrink-0 cursor-pointer overflow-hidden rounded-md border border-line-strong shadow-xs" style={{ background: value || "var(--accent)" }}>
        <span className="sr-only">{t("settings.pickColor")}</span>
        <input type="color" value={hex} onChange={(e) => onChange(e.target.value)} className="absolute inset-0 cursor-pointer opacity-0" />
      </label>
      <Input id={id} aria-invalid={invalid || undefined} value={value} placeholder={t("settings.accentDefault")} onChange={(e) => onChange(e.target.value)} className="font-mono text-xs" />
    </div>
  );
}

function LogoInput({ id, value, onChange, invalid }: { id: string; value: string; onChange: (v: string) => void; invalid: boolean }) {
  const [broken, setBroken] = useState<string>();
  return (
    <div className="flex max-w-xl items-center gap-2">
      <Input id={id} aria-invalid={invalid || undefined} value={value} placeholder="https://" onChange={(e) => onChange(e.target.value)} className="font-mono text-xs" />
      {value && broken !== value && (
        <img src={value} alt="" onError={() => setBroken(value)} className="size-8 shrink-0 rounded-md border border-line bg-surface-2 object-contain p-0.5" />
      )}
    </div>
  );
}

/** The control for one site setting, chosen by its type and, for a few keys, by what the key holds. */
export function SettingControl({
  item,
  value,
  onChange,
  invalid,
  disabled,
  describedBy,
}: {
  item: SettingItem;
  value: unknown;
  onChange: (v: unknown) => void;
  invalid: boolean;
  disabled?: boolean;
  describedBy?: string;
}) {
  const { t, pick } = useI18n();
  const id = `s-${item.key}`;
  if (item.apply === "file") return <FileValue item={item} />;
  const common = { id, disabled, "aria-invalid": invalid || undefined, "aria-describedby": describedBy };
  const text = typeof value === "string" ? value : value === null || value === undefined ? "" : String(value);
  if (item.type === "bool") return <Switch id={id} disabled={disabled} aria-describedby={describedBy} checked={Boolean(value)} onChange={onChange} />;
  if (item.key === "site.currency") return <CurrencySelect id={id} value={text} onChange={onChange} invalid={invalid} />;
  if (item.key === "site.accent") return <AccentInput id={id} value={text} onChange={onChange} invalid={invalid} />;
  if (item.key === "site.logo_url") return <LogoInput id={id} value={text} onChange={onChange} invalid={invalid} />;
  if (item.enum)
    return (
      <Select
        {...common}
        value={text || undefined}
        onChange={onChange}
        className="max-w-72"
        options={item.enum.map((e) => ({ value: e, label: item.enum_labels?.[e] ? pick(item.enum_labels[e]) : e }))}
      />
    );
  if (item.type === "secret") {
    const set = item.source === "database";
    return (
      <Input
        {...common}
        type="password"
        autoComplete="new-password"
        value={text}
        placeholder={set ? t("form.secretKeep") : t("form.secretEmpty")}
        onChange={(e) => onChange(e.target.value || undefined)}
        className="max-w-sm"
      />
    );
  }
  if (item.type === "list")
    return (
      <Textarea
        {...common}
        value={Array.isArray(value) ? (value as string[]).join("\n") : ""}
        onChange={(e) => onChange(e.target.value.split("\n").map((s) => s.trim()).filter(Boolean))}
        className="max-w-sm font-mono text-xs"
      />
    );
  if (item.type === "markdown" || item.type === "json")
    return <Textarea {...common} value={text} onChange={(e) => onChange(e.target.value)} className="max-w-xl font-mono text-xs" />;
  if (item.type === "int" || item.type === "float") {
    const unit = UNIT[item.key];
    return (
      <div className="flex items-center gap-2">
        <Input
          {...common}
          inputMode={item.type === "int" ? "numeric" : "decimal"}
          value={text}
          placeholder={item.default !== undefined && item.default !== null ? String(item.default) : ""}
          onChange={(e) => {
            const raw = e.target.value.trim();
            const n = Number(raw);
            onChange(raw === "" ? null : Number.isFinite(n) ? n : raw);
          }}
          className="w-28 tabular-nums"
        />
        {unit && <span className="text-sm text-ink-3">{t(unit)}</span>}
      </div>
    );
  }
  const url = item.key.endsWith("_url");
  return (
    <Input
      {...common}
      value={text}
      placeholder={url ? "https://relay.example.com" : item.default ? String(item.default) : ""}
      onChange={(e) => onChange(e.target.value)}
      className={cn("max-w-sm", url && "max-w-xl font-mono text-xs")}
    />
  );
}
