import { useMutation, useQuery } from "@tanstack/react-query";
import { Plus, Trash2 } from "lucide-react";
import { toast } from "sonner";
import { Drawer } from "@/components/drawer";
import { SwitchRow } from "@/components/form-kit";
import { Button } from "@/components/ui/button";
import { Field } from "@/components/ui/field";
import { Input, Textarea } from "@/components/ui/input";
import { Select } from "@/components/ui/select";
import { api, unwrap } from "@/lib/api/client";
import { errorMessage } from "@/lib/api/errors";
import type { components } from "@/lib/api/schema.gen";
import { useForm } from "@/lib/use-form";
import { useDiscardCheck } from "@/lib/use-unsaved-guard";
import { useI18n } from "@/i18n";
import { splitList } from "@/lib/text";

type S = components["schemas"];
// The generator does not carry `required` across allOf.
export type SsoProvider = S["SsoProvider"] &
  Required<
    Pick<
      S["SsoProviderInput"],
      "id" | "name" | "preset" | "enabled" | "client_id" | "auto_create" | "link_by_email" | "trust_email"
    >
  > &
  Pick<S["SsoProviderInput"], "link_admins">;
type Preset = S["SsoPreset"];
type Rule = S["SsoGroupRule"];

const PRESETS: Preset[] = [
  "google",
  "microsoft",
  "github",
  "feishu",
  "lark",
  "keycloak",
  "authentik",
  "oidc",
];
const NONE = "__none";

const needsIssuer = (p: Preset) => p === "oidc" || p === "keycloak" || p === "authentik";
const hasTenants = (p: Preset) => p === "microsoft" || p === "feishu" || p === "lark";
const isOIDC = (p: Preset) => p !== "github" && p !== "feishu" && p !== "lark";

/** Add or edit one sign-in provider; the client secret is write-only and kept when left empty. */
export function ProviderDrawer({
  provider,
  onClose,
  onSaved,
}: {
  provider: SsoProvider | "new";
  onClose: () => void;
  onSaved: () => void;
}) {
  const { t } = useI18n();
  const discard = useDiscardCheck();
  const groups = useQuery({ queryKey: ["groups"], queryFn: () => unwrap(api.GET("/groups")) });
  const isNew = provider === "new";
  const p = isNew ? undefined : provider;
  const form = useForm({
    preset: (p?.preset ?? "google") as Preset,
    id: p?.id ?? "",
    name: p?.name ?? "",
    enabled: p?.enabled ?? true,
    issuer: p?.issuer ?? "",
    tenant: p?.tenant ?? "",
    client_id: p?.client_id ?? "",
    client_secret: "",
    scopes: (p?.scopes ?? []).join(" "),
    allowed_domains: (p?.allowed_domains ?? []).join("\n"),
    allowed_tenants: (p?.allowed_tenants ?? []).join("\n"),
    auto_create: p?.auto_create ?? false,
    default_role: (p?.default_role ?? "user") as "user" | "member",
    default_group_id: p?.default_group_id || NONE,
    group_rules: (p?.group_rules ?? []) as Rule[],
    link_by_email: p?.link_by_email ?? false,
    link_admins: p?.link_admins ?? false,
    trust_email: p?.trust_email ?? false,
  });
  const v = form.values;
  const unlinks =
    !isNew &&
    (v.client_id.trim() !== (p!.client_id ?? "") ||
      (needsIssuer(v.preset) && v.issuer.trim() !== (p!.issuer ?? "")) ||
      (v.preset === "microsoft" && v.tenant.trim() !== (p!.tenant ?? "")));
  const groupOptions = (groups.data?.items ?? []).map((g) => ({ value: g.id, label: g.name }));

  const save = useMutation({
    mutationFn: () => {
      const body: S["SsoProviderInput"] = {
        name: v.name.trim() || undefined,
        enabled: v.enabled,
        client_id: v.client_id.trim(),
        client_secret: v.client_secret || undefined,
        scopes: v.scopes.trim() ? v.scopes.trim().split(/\s+/) : undefined,
        allowed_domains: splitList(v.allowed_domains),
        allowed_tenants: hasTenants(v.preset) ? splitList(v.allowed_tenants) : [],
        auto_create: v.auto_create,
        default_role: v.default_role,
        default_group_id: v.default_group_id === NONE ? "" : v.default_group_id,
        group_rules: v.group_rules,
        link_by_email: v.link_by_email,
        link_admins: v.link_by_email && v.link_admins,
        trust_email: v.trust_email,
        ...(needsIssuer(v.preset) ? { issuer: v.issuer.trim() } : {}),
        ...(v.preset === "microsoft" ? { tenant: v.tenant.trim() } : {}),
      };
      if (isNew)
        return unwrap(api.POST("/sso/providers", { body: { ...body, id: v.id.trim(), preset: v.preset } }));
      return unwrap(api.PATCH("/sso/providers/{id}", { params: { path: { id: p!.id } }, body }));
    },
    onSuccess: (r) => {
      toast.success(t("sso.saved", { name: r.name ?? r.id ?? "" }));
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
  const setRule = (i: number, r: Partial<Rule>) =>
    form.set(
      "group_rules",
      v.group_rules.map((x, j) => (j === i ? { ...x, ...r } : x)),
    );

  return (
    <Drawer
      open
      onOpenChange={(o) => !o && void close()}
      title={isNew ? t("sso.add") : p!.name}
      subtitle={t("sso.dialogHint")}
      size="md"
      onRequestClose={() => void close()}
      footer={
        <>
          <Button onClick={() => void close()}>{t("common.cancel")}</Button>
          <Button variant="primary" onClick={() => save.mutate()} disabled={save.isPending}>
            {save.isPending ? t("common.saving") : t("common.save")}
          </Button>
        </>
      }
    >
      <div className="grid gap-4 p-5">
        <div className="grid gap-4 sm:grid-cols-2">
          <Field label={t("sso.field.preset")} hint={t(`sso.presetHint.${v.preset}`)}>
            <Select
              value={v.preset}
              disabled={!isNew}
              onChange={(x) => form.set("preset", x)}
              options={PRESETS.map((x) => ({ value: x, label: t(`sso.preset.${x}`) }))}
            />
          </Field>
          <Field label={t("sso.field.id")} hint={t("sso.field.idHint")} error={form.errors.id}>
            <Input
              value={v.id}
              disabled={!isNew}
              onChange={(e) => form.set("id", e.target.value)}
              autoComplete="off"
              className="font-mono"
            />
          </Field>
        </div>
        <Field label={t("sso.field.name")} error={form.errors.name}>
          <Input
            value={v.name}
            placeholder={t(`sso.preset.${v.preset}`)}
            onChange={(e) => form.set("name", e.target.value)}
          />
        </Field>
        {needsIssuer(v.preset) && (
          <Field label={t("sso.field.issuer")} hint={t("sso.field.issuerHint")} error={form.errors.issuer}>
            <Input
              value={v.issuer}
              onChange={(e) => form.set("issuer", e.target.value)}
              placeholder="https://idp.example.com/realms/main"
              className="font-mono"
            />
          </Field>
        )}
        {v.preset === "microsoft" && (
          <Field label={t("sso.field.tenant")} hint={t("sso.field.tenantHint")} error={form.errors.tenant}>
            <Input
              value={v.tenant}
              onChange={(e) => form.set("tenant", e.target.value)}
              className="font-mono"
            />
          </Field>
        )}
        <div className="grid gap-4 sm:grid-cols-2">
          <Field label={t("sso.field.clientId")} error={form.errors.client_id}>
            <Input
              value={v.client_id}
              onChange={(e) => form.set("client_id", e.target.value)}
              autoComplete="off"
              className="font-mono"
            />
          </Field>
          <Field label={t("sso.field.clientSecret")} error={form.errors.client_secret}>
            <Input
              type="password"
              autoComplete="off"
              value={v.client_secret}
              placeholder={isNew ? "" : t("form.secretKeep")}
              onChange={(e) => form.set("client_secret", e.target.value)}
            />
          </Field>
        </div>
        {unlinks && (
          <p role="alert" className="text-xs text-warn">
            {t("sso.field.unlinkWarning")}
          </p>
        )}
        <Field
          label={t("sso.field.scopes")}
          hint={isOIDC(v.preset) ? t("sso.field.scopesHintOidc") : t("sso.field.scopesHint")}
          error={form.errors.scopes}
        >
          <Input
            value={v.scopes}
            onChange={(e) => form.set("scopes", e.target.value)}
            className="font-mono"
          />
        </Field>
        <Field
          label={t("sso.field.domains")}
          hint={t("sso.field.domainsHint")}
          error={form.errors.allowed_domains}
        >
          <Textarea
            value={v.allowed_domains}
            onChange={(e) => form.set("allowed_domains", e.target.value)}
            className="min-h-14 font-mono"
          />
        </Field>
        {hasTenants(v.preset) && (
          <Field
            label={t("sso.field.tenants")}
            hint={t("sso.field.tenantsHint")}
            error={form.errors.allowed_tenants}
          >
            <Textarea
              value={v.allowed_tenants}
              onChange={(e) => form.set("allowed_tenants", e.target.value)}
              className="min-h-14 font-mono"
            />
          </Field>
        )}
        <SwitchRow
          label={t("sso.field.linkByEmail")}
          description={t("sso.field.linkByEmailHint")}
          checked={v.link_by_email}
          onChange={(c) => form.set("link_by_email", c)}
        />
        {v.link_by_email && (
          <SwitchRow
            label={t("sso.field.linkAdmins")}
            description={t("sso.field.linkAdminsHint")}
            checked={v.link_admins ?? false}
            onChange={(c) => form.set("link_admins", c)}
          />
        )}
        <SwitchRow
          label={t("sso.field.autoCreate")}
          description={t("sso.field.autoCreateHint")}
          checked={v.auto_create}
          onChange={(c) => form.set("auto_create", c)}
        />
        {v.auto_create && (
          <div className="grid gap-4 sm:grid-cols-2">
            <Field label={t("sso.field.defaultRole")} error={form.errors.default_role}>
              <Select
                value={v.default_role}
                onChange={(r) => form.set("default_role", r)}
                options={(["user", "member"] as const).map((r) => ({ value: r, label: t(`role.${r}`) }))}
              />
            </Field>
            <Field label={t("sso.field.defaultGroup")} error={form.errors.default_group_id}>
              <Select
                value={v.default_group_id}
                onChange={(g) => form.set("default_group_id", g)}
                options={[{ value: NONE, label: t("users.noGroup") }, ...groupOptions]}
              />
            </Field>
          </div>
        )}
        {v.auto_create && (
          <fieldset className="grid gap-2">
            <legend className="text-[13px] font-medium">{t("sso.field.rules")}</legend>
            <p className="text-xs text-ink-3">{t("sso.field.rulesHint")}</p>
            {v.group_rules.map((r, i) => (
              <div key={i} className="grid grid-cols-[1fr_1fr_1fr_auto] items-center gap-2">
                <Input
                  aria-label={t("sso.field.ruleClaim")}
                  placeholder={t("sso.field.ruleClaimPlaceholder")}
                  value={r.domain ? `@${r.domain}` : (r.claim ?? "")}
                  onChange={(e) => {
                    const x = e.target.value.trim();
                    setRule(
                      i,
                      x.startsWith("@")
                        ? { domain: x.slice(1), claim: undefined, value: undefined }
                        : { claim: x, domain: undefined },
                    );
                  }}
                  className="font-mono"
                />
                <Input
                  aria-label={t("sso.field.ruleValue")}
                  placeholder={t("sso.field.ruleValue")}
                  disabled={Boolean(r.domain)}
                  value={r.value ?? ""}
                  onChange={(e) => setRule(i, { value: e.target.value })}
                  className="font-mono"
                />
                <Select
                  aria-label={t("users.group")}
                  value={r.group_id || undefined}
                  onChange={(g) => setRule(i, { group_id: g })}
                  options={groupOptions}
                />
                <Button
                  size="icon-sm"
                  variant="ghost"
                  aria-label={t("common.remove")}
                  onClick={() =>
                    form.set(
                      "group_rules",
                      v.group_rules.filter((_, j) => j !== i),
                    )
                  }
                >
                  <Trash2 aria-hidden />
                </Button>
                {form.errors[`group_rules.${i}`] && (
                  <p className="col-span-4 text-xs text-bad">{form.errors[`group_rules.${i}`]}</p>
                )}
              </div>
            ))}
            <div>
              <Button
                size="sm"
                onClick={() =>
                  form.set("group_rules", [...v.group_rules, { claim: "", value: "", group_id: "" }])
                }
              >
                <Plus aria-hidden />
                {t("sso.field.addRule")}
              </Button>
            </div>
          </fieldset>
        )}
        <SwitchRow
          label={t("sso.field.trustEmail")}
          description={t("sso.field.trustEmailHint")}
          checked={v.trust_email}
          onChange={(c) => form.set("trust_email", c)}
        />
        <SwitchRow label={t("sso.field.enabled")} checked={v.enabled} onChange={(c) => form.set("enabled", c)} />
      </div>
    </Drawer>
  );
}
