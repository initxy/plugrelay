import { useMutation } from "@tanstack/react-query";
import { toast } from "sonner";
import { Drawer } from "@/components/drawer";
import { SwitchRow } from "@/components/form-kit";
import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import { Field } from "@/components/ui/field";
import { Input, Textarea } from "@/components/ui/input";
import { Select } from "@/components/ui/select";
import { useI18n } from "@/i18n";
import { api, unwrap } from "@/lib/api/client";
import { errorMessage } from "@/lib/api/errors";
import type { components } from "@/lib/api/schema.gen";
import { cn } from "@/lib/cn";
import { useForm } from "@/lib/use-form";
import { useDiscardCheck } from "@/lib/use-unsaved-guard";
import { TYPE_ICON, useEventLabel, type Channel, type ChannelType } from "./shared";
import { splitList } from "@/lib/text";

type S = components["schemas"];
type Event = S["NotifyEvent"];
type TLS = NonNullable<S["NotifyChannelInput"]["tls"]>;
type Lang = NonNullable<S["NotifyChannelInput"]["language"]>;

const TYPES: ChannelType[] = ["feishu", "dingtalk", "wecom", "email", "webhook"];
const TLS_MODES: TLS[] = ["starttls", "tls", "none"];

/** Add or edit one channel; secrets are write-only and kept when left empty. */
export function ChannelDrawer({
  channel,
  events,
  recordFields,
  onClose,
  onSaved,
}: {
  channel: Channel | "new";
  events: Event[];
  recordFields: string[];
  onClose: () => void;
  onSaved: () => void;
}) {
  const { t, locale, has } = useI18n();
  const discard = useDiscardCheck();
  const eventLabel = useEventLabel();
  const isNew = channel === "new";
  const c = isNew ? undefined : channel;
  const form = useForm({
    type: (c?.type ?? "feishu") as ChannelType,
    name: c?.name ?? "",
    enabled: c?.enabled ?? true,
    language: (c?.language ?? (locale === "zh-CN" ? "zh-CN" : "en")) as Lang,
    allEvents: (c?.events.length ?? 0) === 0,
    events: (c?.events ?? []) as Event[],
    url: "",
    secret: "",
    clearSecret: false,
    smtp_host: c?.smtp_host ?? "",
    smtp_port: c?.smtp_port ? String(c.smtp_port) : "",
    tls: (c?.tls ?? "starttls") as TLS,
    username: c?.username ?? "",
    password: "",
    from: c?.from ?? "",
    to: (c?.to ?? []).join("\n"),
    records: c?.records ?? false,
    record_fields: c?.record_fields.length ? c.record_fields : recordFields,
  });
  const v = form.values;
  const bot = v.type === "feishu" || v.type === "dingtalk" || v.type === "wecom";
  const signs = v.type === "feishu" || v.type === "dingtalk" || v.type === "webhook";
  const save = useMutation({
    mutationFn: () => {
      const body: S["NotifyChannelInput"] = {
        name: v.name.trim(),
        enabled: v.enabled,
        language: v.language,
        events: v.allEvents ? [] : v.events,
      };
      if (v.url.trim()) body.url = v.url.trim();
      if (v.secret) body.secret = v.secret;
      else if (v.clearSecret) body.secret = "";
      if (v.type === "email") {
        Object.assign(body, {
          smtp_host: v.smtp_host.trim(),
          smtp_port: v.smtp_port ? Number(v.smtp_port) : 0,
          tls: v.tls,
          username: v.username.trim(),
          from: v.from.trim(),
          to: splitList(v.to),
        });
        if (v.password) body.password = v.password;
      }
      if (v.type === "webhook") {
        body.records = v.records;
        const all = recordFields.every((f) => v.record_fields.includes(f)) && !v.record_fields.includes("body");
        body.record_fields = all ? [] : v.record_fields;
      }
      if (isNew) return unwrap(api.POST("/notifications/channels", { body: { ...body, type: v.type } }));
      return unwrap(api.PATCH("/notifications/channels/{id}", { params: { path: { id: c!.id } }, body }));
    },
    onSuccess: (r) => {
      toast.success(t("notify.saved", { name: r.name }));
      onSaved();
    },
    onError: (e) => {
      form.absorb(e);
      toast.error(errorMessage(e));
    },
  });
  const close = async () => {
    if (await discard(form.dirty)) onClose();
  };
  const toggle = <T extends string>(list: T[], x: T, on: boolean) => (on ? [...list, x] : list.filter((y) => y !== x));
  const urlHint = v.type === "email" ? undefined : t(`notify.field.urlHint.${v.type}`);
  const fieldsOffered = [...recordFields, "body"];
  return (
    <Drawer
      open
      onOpenChange={(o) => !o && void close()}
      onRequestClose={() => void close()}
      title={isNew ? t("notify.add") : c!.name}
      size="md"
      footer={
        <>
          <Button onClick={() => void close()}>{t("common.cancel")}</Button>
          <Button variant="primary" onClick={() => save.mutate()} disabled={save.isPending}>
            {save.isPending ? t("common.saving") : t("common.save")}
          </Button>
        </>
      }
    >
      <div className="grid gap-5 p-5">
        {isNew && (
          <fieldset className="grid gap-1.5">
            <legend className="mb-1.5 text-xs font-semibold">{t("notify.field.type")}</legend>
            <div className="grid grid-cols-2 gap-2 sm:grid-cols-3">
              {TYPES.map((ty) => {
                const Icon = TYPE_ICON[ty];
                const on = v.type === ty;
                return (
                  <label
                    key={ty}
                    className={cn(
                      "flex cursor-pointer flex-col gap-1 rounded-md border px-3 py-2.5 transition-colors duration-100 has-[:focus-visible]:outline-2 has-[:focus-visible]:outline-ring",
                      on ? "border-accent bg-accent-soft" : "border-line hover:bg-surface-2",
                    )}
                  >
                    <input type="radio" name="notify-type" className="sr-only" checked={on} onChange={() => form.set("type", ty)} />
                    <span className="flex items-center gap-1.5 text-sm font-medium">
                      <Icon className="size-4 text-ink-2" aria-hidden />
                      {t(`notify.type.${ty}`)}
                    </span>
                    <span className="text-xs text-ink-3">{t(`notify.typeHint.${ty}`)}</span>
                  </label>
                );
              })}
            </div>
          </fieldset>
        )}
        <div className="grid items-start gap-4 sm:grid-cols-[1fr_9rem]">
          <Field label={t("notify.field.name")} error={form.errors.name}>
            <Input value={v.name} onChange={(e) => form.set("name", e.target.value)} placeholder={t("notify.field.namePlaceholder")} />
          </Field>
          <Field label={t("notify.field.language")} error={form.errors.language}>
            <Select
              value={v.language}
              onChange={(x) => form.set("language", x)}
              options={(["zh-CN", "en"] as Lang[]).map((l) => ({ value: l, label: t(`notify.lang.${l}`) }))}
            />
          </Field>
        </div>

        {v.type === "email" ? (
          <>
            <div className="grid items-start gap-4 sm:grid-cols-[1fr_7rem]">
              <Field label={t("notify.field.smtpHost")} error={form.errors.smtp_host}>
                <Input value={v.smtp_host} onChange={(e) => form.set("smtp_host", e.target.value)} placeholder="smtp.example.com" className="font-mono" />
              </Field>
              <Field label={t("notify.field.smtpPort")} error={form.errors.smtp_port} optional>
                <Input
                  value={v.smtp_port}
                  inputMode="numeric"
                  onChange={(e) => form.set("smtp_port", e.target.value.replace(/\D/g, ""))}
                  className="font-mono"
                />
              </Field>
            </div>
            <Field label={t("notify.field.tls")} hint={t("notify.field.smtpPortHint")} error={form.errors.tls}>
              <Select value={v.tls} onChange={(x) => form.set("tls", x)} options={TLS_MODES.map((m) => ({ value: m, label: t(`notify.tls.${m}`) }))} />
            </Field>
            <div className="grid items-start gap-4 sm:grid-cols-2">
              <Field label={t("notify.field.username")} hint={t("notify.field.usernameHint")} error={form.errors.username} optional>
                <Input value={v.username} autoComplete="off" onChange={(e) => form.set("username", e.target.value)} />
              </Field>
              <Field label={t("notify.field.password")} error={form.errors.password} optional>
                <Input
                  type="password"
                  autoComplete="new-password"
                  value={v.password}
                  placeholder={c?.has_password ? t("form.secretKeep") : undefined}
                  onChange={(e) => form.set("password", e.target.value)}
                />
              </Field>
            </div>
            <Field label={t("notify.field.from")} error={form.errors.from}>
              <Input value={v.from} onChange={(e) => form.set("from", e.target.value)} placeholder="plugrelay <alerts@example.com>" />
            </Field>
            <Field label={t("notify.field.to")} hint={t("notify.field.toHint")} error={form.errors.to ?? form.errors["to.0"]}>
              <Textarea value={v.to} onChange={(e) => form.set("to", e.target.value)} className="min-h-16" placeholder="ops@example.com" />
            </Field>
          </>
        ) : (
          <>
            <Field
              label={t(bot ? "notify.field.url" : "notify.field.webhookUrl")}
              hint={urlHint}
              error={form.errors.url}
            >
              <Input
                value={v.url}
                autoComplete="off"
                onChange={(e) => form.set("url", e.target.value)}
                placeholder={c?.url ? t("notify.field.urlKeep") : "https://"}
                className="font-mono"
              />
            </Field>
            {signs && (
              <Field
                label={t("notify.field.secret")}
                hint={t(v.type === "webhook" ? "notify.field.secretHint.webhook" : "notify.field.secretHint.bot")}
                error={form.errors.secret}
                optional
                aside={
                  c?.has_secret && !v.secret ? (
                    <label className="flex items-center gap-1.5 text-xs text-ink-2">
                      <Checkbox checked={v.clearSecret} onChange={(x) => form.set("clearSecret", x)} />
                      {t("notify.field.secretRemove")}
                    </label>
                  ) : undefined
                }
              >
                <Input
                  type="password"
                  autoComplete="new-password"
                  value={v.secret}
                  placeholder={c?.has_secret ? t("form.secretKeep") : undefined}
                  onChange={(e) => form.set("secret", e.target.value)}
                />
              </Field>
            )}
          </>
        )}
        <p className="-mt-2 text-xs text-ink-3">
          {t("notify.field.privateHint")} <code className="rounded bg-surface-2 px-1 font-mono">egress.allow_private_cidrs</code>
        </p>

        <fieldset className="grid gap-2">
          <legend className="mb-1 text-xs font-semibold">{t("notify.field.events")}</legend>
          <div className="flex flex-wrap gap-x-4 gap-y-1 text-sm">
            {[true, false].map((all) => (
              <label key={String(all)} className="flex cursor-pointer items-center gap-2">
                <input type="radio" name="notify-events" checked={v.allEvents === all} onChange={() => form.set("allEvents", all)} className="accent-[var(--color-accent)]" />
                {t(all ? "notify.field.eventsAll" : "notify.field.eventsPick")}
              </label>
            ))}
          </div>
          {!v.allEvents && (
            <div className="grid gap-1.5 rounded-md border border-line p-3 sm:grid-cols-2">
              {events.map((e) => (
                <label key={e} className="flex cursor-pointer items-center gap-2 text-sm">
                  <Checkbox checked={v.events.includes(e)} onChange={(on) => form.set("events", toggle(v.events, e, on))} />
                  {eventLabel(e)}
                </label>
              ))}
            </div>
          )}
          {form.errors.events && <p className="text-xs text-bad">{form.errors.events}</p>}
        </fieldset>

        {v.type === "webhook" && (
          <div className="grid gap-3 rounded-md border border-line p-3">
            <SwitchRow
              label={t("notify.field.records")}
              description={t("notify.field.recordsHint")}
              checked={v.records}
              onChange={(x) => form.set("records", x)}
            />
            {v.records && (
              <fieldset className="grid gap-1.5">
                <legend className="mb-1 text-xs font-semibold">{t("notify.field.recordFields")}</legend>
                <p className="text-xs text-ink-3">{t("notify.field.recordFieldsHint")}</p>
                <div className="grid gap-1.5 sm:grid-cols-2">
                  {fieldsOffered.map((f) => {
                    const k = `notify.rf.${f}`;
                    return (
                      <label key={f} className="flex cursor-pointer items-center gap-2 text-sm">
                        <Checkbox checked={v.record_fields.includes(f)} onChange={(on) => form.set("record_fields", toggle(v.record_fields, f, on))} />
                        {has(k) ? t(k) : f}
                      </label>
                    );
                  })}
                </div>
                {form.errors["record_fields.0"] && <p className="text-xs text-bad">{form.errors["record_fields.0"]}</p>}
              </fieldset>
            )}
          </div>
        )}

        <SwitchRow label={t("notify.field.enabled")} checked={v.enabled} onChange={(x) => form.set("enabled", x)} />
      </div>
    </Drawer>
  );
}
