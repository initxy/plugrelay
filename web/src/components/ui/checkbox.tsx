import * as C from "@radix-ui/react-checkbox";
import { Check, Minus } from "lucide-react";
import { cn } from "@/lib/cn";

export function Checkbox({
  checked,
  onChange,
  className,
  ...rest
}: {
  checked: boolean | "indeterminate";
  onChange: (v: boolean) => void;
  className?: string;
  "aria-label"?: string;
  id?: string;
}) {
  return (
    <C.Root
      checked={checked}
      onCheckedChange={(v) => onChange(v === true)}
      {...rest}
      className={cn(
        "grid size-4 shrink-0 place-items-center rounded-[4px] border border-line-strong bg-surface data-[state=checked]:border-accent data-[state=checked]:bg-accent data-[state=indeterminate]:border-accent data-[state=indeterminate]:bg-accent text-accent-ink",
        className,
      )}
    >
      <C.Indicator>
        {checked === "indeterminate" ? <Minus className="size-3" /> : <Check className="size-3" />}
      </C.Indicator>
    </C.Root>
  );
}
