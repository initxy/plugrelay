import { cn } from "@/lib/cn";
import { avatarColor, monogram } from "./labels";

/** A square monogram in a stable color per provider. No external logos. */
export function ProviderAvatar({ name, title, size = "md", className }: { name: string; title: string; size?: "sm" | "md" | "lg"; className?: string }) {
  return (
    <span
      aria-hidden
      style={{ backgroundColor: avatarColor(name) }}
      className={cn(
        "grid shrink-0 place-items-center font-semibold text-white shadow-[inset_0_0_0_1px_rgb(255_255_255/0.12)]",
        size === "sm" && "size-6 rounded-md text-[11px]",
        size === "md" && "size-8 rounded-lg text-[13px]",
        size === "lg" && "size-10 rounded-[10px] text-base",
        className,
      )}
    >
      {monogram(title)}
    </span>
  );
}
