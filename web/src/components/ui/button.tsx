import { Slot } from "@radix-ui/react-slot";
import { cva, type VariantProps } from "class-variance-authority";
import { forwardRef, type ButtonHTMLAttributes } from "react";
import { cn } from "@/lib/cn";

const buttonVariants = cva(
  "inline-flex select-none items-center justify-center gap-1.5 whitespace-nowrap rounded-md font-medium transition-[background-color,border-color,color,box-shadow] duration-100 focus-visible:outline-offset-1 disabled:pointer-events-none disabled:opacity-50 [&_svg]:size-4 [&_svg]:shrink-0",
  {
    variants: {
      variant: {
        primary: "bg-accent text-accent-ink shadow-xs hover:bg-accent-hover",
        default: "border border-line-strong bg-surface text-ink shadow-xs hover:border-ink-3/50 hover:bg-surface-2 [&_svg]:text-ink-2",
        ghost: "text-ink-2 hover:bg-surface-2 hover:text-ink",
        danger: "border border-line-strong bg-surface text-bad shadow-xs hover:border-bad/40 hover:bg-bad-soft",
        "danger-solid": "bg-bad text-white shadow-xs hover:brightness-110",
        link: "h-auto px-0 text-accent underline-offset-4 hover:underline",
      },
      size: {
        xs: "h-6 gap-1 px-2 text-xs [&_svg]:size-3.5",
        sm: "h-7 px-2.5 text-xs [&_svg]:size-3.5",
        md: "h-8 px-3 text-sm",
        lg: "h-9 px-4 text-sm",
        icon: "size-8",
        "icon-sm": "size-7 [&_svg]:size-3.5",
      },
    },
    defaultVariants: { variant: "default", size: "md" },
  },
);

export interface ButtonProps
  extends ButtonHTMLAttributes<HTMLButtonElement>,
    VariantProps<typeof buttonVariants> {
  asChild?: boolean;
}

export const Button = forwardRef<HTMLButtonElement, ButtonProps>(
  ({ className, variant, size, asChild, type, ...props }, ref) => {
    const Comp = asChild ? Slot : "button";
    return (
      <Comp
        ref={ref}
        type={asChild ? undefined : (type ?? "button")}
        className={cn(buttonVariants({ variant, size }), className)}
        {...props}
      />
    );
  },
);
Button.displayName = "Button";
