import { forwardRef, type InputHTMLAttributes, type TextareaHTMLAttributes } from "react";
import { cn } from "@/lib/cn";

/** Shared look of text controls: a quiet border, an accent border plus soft ring on focus, red when invalid. */
export const controlClass =
  "w-full rounded-md border border-line-strong bg-surface px-2.5 text-sm text-ink shadow-xs transition-[border-color,box-shadow] duration-100 placeholder:text-ink-3 hover:border-ink-3/60 focus-visible:border-accent focus-visible:shadow-[0_0_0_3px_var(--ring-soft)] focus-visible:outline-none disabled:cursor-not-allowed disabled:bg-surface-2 disabled:opacity-70 aria-[invalid=true]:border-bad aria-[invalid=true]:focus-visible:shadow-[0_0_0_3px_color-mix(in_srgb,var(--bad)_20%,transparent)]";

export const Input = forwardRef<HTMLInputElement, InputHTMLAttributes<HTMLInputElement>>(
  ({ className, ...props }, ref) => <input ref={ref} className={cn(controlClass, "h-8", className)} {...props} />,
);
Input.displayName = "Input";

export const Textarea = forwardRef<HTMLTextAreaElement, TextareaHTMLAttributes<HTMLTextAreaElement>>(
  ({ className, ...props }, ref) => (
    <textarea ref={ref} className={cn(controlClass, "min-h-20 py-1.5 leading-relaxed", className)} {...props} />
  ),
);
Textarea.displayName = "Textarea";
