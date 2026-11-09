import { Mail, MessageSquare, Webhook } from "lucide-react";
import { useI18n } from "@/i18n";
import type { components } from "@/lib/api/schema.gen";

type S = components["schemas"];
export type Channel = S["NotifyChannel"];
export type ChannelType = S["NotifyChannelType"];

export const TYPE_ICON: Record<ChannelType, typeof Mail> = {
  email: Mail,
  feishu: MessageSquare,
  dingtalk: MessageSquare,
  wecom: MessageSquare,
  webhook: Webhook,
};

/** An event's name for people; the code itself for an event this console does not know. */
export function useEventLabel() {
  const { t, has } = useI18n();
  return (event: string) => {
    const k = `notify.event.${event}`;
    return has(k) ? t(k) : event;
  };
}
