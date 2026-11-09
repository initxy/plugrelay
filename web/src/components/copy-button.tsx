import { Check, Copy } from "lucide-react";
import { useState } from "react";
import { toast } from "sonner";
import { Button, type ButtonProps } from "@/components/ui/button";
import { useI18n } from "@/i18n";

async function copyText(text: string): Promise<boolean> {
  try {
    await navigator.clipboard.writeText(text);
    return true;
  } catch {
    return false;
  }
}

export function CopyButton({
  text,
  label,
  size = "sm",
  variant = "default",
}: {
  text: string;
  label?: string;
  size?: ButtonProps["size"];
  variant?: ButtonProps["variant"];
}) {
  const { t } = useI18n();
  const [done, setDone] = useState(false);
  const onClick = async () => {
    if (await copyText(text)) {
      setDone(true);
      setTimeout(() => setDone(false), 1500);
    } else {
      toast.error(t("common.copyFailed"));
    }
  };
  return (
    <Button size={size} variant={variant} onClick={onClick} aria-label={label ?? t("common.copy")}>
      {done ? <Check className="text-ok" /> : <Copy />}
      {size !== "icon" && size !== "icon-sm" && (done ? t("common.copied") : (label ?? t("common.copy")))}
    </Button>
  );
}
