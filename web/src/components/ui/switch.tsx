import * as S from "@radix-ui/react-switch";
import { forwardRef } from "react";
import { cn } from "@/lib/cn";

interface SwitchProps {
  checked: boolean;
  onChange: (v: boolean) => void;
  disabled?: boolean;
  id?: string;
  "aria-label"?: string;
  "aria-describedby"?: string;
  className?: string;
}

export const Switch = forwardRef<HTMLButtonElement, SwitchProps>(
  ({ checked, onChange, className, ...rest }, ref) => (
    <S.Root
      ref={ref}
      checked={checked}
      onCheckedChange={onChange}
      {...rest}
      className={cn(
        "inline-flex h-[18px] w-[32px] shrink-0 cursor-pointer items-center rounded-full bg-line-strong transition-colors duration-150 hover:brightness-95 data-[state=checked]:bg-accent disabled:cursor-not-allowed disabled:opacity-50",
        className,
      )}
    >
      <S.Thumb className="block size-[14px] translate-x-[2px] rounded-full bg-white shadow-[0_1px_2px_rgb(0_0_0/0.25)] transition-transform duration-150 data-[state=checked]:translate-x-[16px]" />
    </S.Root>
  ),
);
Switch.displayName = "Switch";
