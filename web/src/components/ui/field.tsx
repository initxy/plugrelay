import * as LabelPrimitive from "@radix-ui/react-label";
import { cloneElement, isValidElement, useId, type ReactElement, type ReactNode } from "react";
import { cn } from "@/lib/cn";
import { useI18n } from "@/i18n";

interface FieldProps {
  label: ReactNode;
  hint?: ReactNode;
  error?: string;
  /** Extra content on the label row, e.g. an "applies after restart" badge. */
  aside?: ReactNode;
  /** Marks the field "optional" next to its label. */
  optional?: boolean;
  className?: string;
  children: ReactElement<{ id?: string; "aria-invalid"?: boolean; "aria-describedby"?: string }>;
}

/** Label + control + hint/error, wired with ids so the error is announced with the control. */
export function Field({ label, hint, error, aside, optional, className, children }: FieldProps) {
  const { t } = useI18n();
  const id = useId();
  const msgId = `${id}-msg`;
  const control = isValidElement(children)
    ? cloneElement(children, {
        id: children.props.id ?? id,
        "aria-invalid": error ? true : undefined,
        "aria-describedby": error || hint ? msgId : undefined,
      })
    : children;
  return (
    <div className={cn("grid gap-1.5", className)}>
      <div className="flex items-center justify-between gap-2">
        <LabelPrimitive.Root htmlFor={children.props.id ?? id} className="text-xs font-semibold text-ink">
          {label}
          {optional && <span className="ml-1.5 font-normal text-ink-3">{t("form.optional")}</span>}
        </LabelPrimitive.Root>
        {aside}
      </div>
      {control}
      {(error || hint) && (
        <p id={msgId} className={cn("text-xs", error ? "text-bad" : "text-ink-3")}>
          {error ?? hint}
        </p>
      )}
    </div>
  );
}
