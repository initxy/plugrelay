import * as S from "@radix-ui/react-select";
import { Check, ChevronDown } from "lucide-react";
import { forwardRef, type ReactNode } from "react";
import { cn } from "@/lib/cn";
import { controlClass } from "./input";

export interface Option<V extends string = string> {
  value: V;
  label: ReactNode;
}

interface SelectProps<V extends string> {
  value: V | undefined;
  onChange: (v: V) => void;
  options: readonly Option<V>[];
  placeholder?: string;
  className?: string;
  id?: string;
  disabled?: boolean;
  "aria-label"?: string;
  "aria-invalid"?: boolean;
}

function SelectInner<V extends string>(
  { value, onChange, options, placeholder, className, disabled, ...aria }: SelectProps<V>,
  ref: React.ForwardedRef<HTMLButtonElement>,
) {
  return (
    <S.Root value={value} onValueChange={(v) => onChange(v as V)} disabled={disabled}>
      <S.Trigger
        ref={ref}
        {...aria}
        className={cn(
          controlClass,
          "inline-flex h-8 items-center justify-between gap-2 whitespace-nowrap text-left data-[placeholder]:text-ink-3 data-[state=open]:border-accent data-[state=open]:shadow-[0_0_0_3px_var(--ring-soft)]",
          className,
        )}
      >
        <span className="min-w-0 truncate">
          <S.Value placeholder={placeholder} />
        </span>
        <S.Icon>
          <ChevronDown className="size-3.5 text-ink-3" />
        </S.Icon>
      </S.Trigger>
      <S.Portal>
        <S.Content
          position="popper"
          sideOffset={4}
          className="z-50 max-h-72 min-w-[var(--radix-select-trigger-width)] overflow-hidden rounded-lg border border-line bg-surface shadow-pop animate-[pop-in_100ms_var(--ease-out-quart)]"
        >
          <S.Viewport className="p-1">
            {options.map((o) => (
              <S.Item
                key={o.value}
                value={o.value}
                className="relative flex cursor-default select-none items-center rounded-sm py-1.5 pl-2 pr-7 text-sm outline-none data-[disabled]:opacity-50 data-[highlighted]:bg-surface-2 data-[state=checked]:font-medium"
              >
                <S.ItemText>{o.label}</S.ItemText>
                <S.ItemIndicator className="absolute right-2">
                  <Check className="size-3.5 text-accent" />
                </S.ItemIndicator>
              </S.Item>
            ))}
          </S.Viewport>
        </S.Content>
      </S.Portal>
    </S.Root>
  );
}

export const Select = forwardRef(SelectInner) as <V extends string>(
  p: SelectProps<V> & { ref?: React.Ref<HTMLButtonElement> },
) => ReturnType<typeof SelectInner>;
