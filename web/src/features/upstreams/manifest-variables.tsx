import { Field } from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import type { ManifestVariable } from "@/lib/api/types";
import { useI18n } from "@/i18n";
import { variableDescription, variableLabel } from "./labels";

/** Field errors for required variables left empty, keyed `config.<name>`. */
export function missingVariables(vars: ManifestVariable[], values: Record<string, unknown>, required: string): Record<string, string> {
  return Object.fromEntries(
    vars.filter((v) => v.required && !String(values[v.name] ?? "").trim()).map((v) => [`config.${v.name}`, required]),
  );
}

/**
 * One field per value the upstream type needs (resource name, region, server address), labelled
 * in plain words with the type's own description. Empty means "use the default", which is the
 * placeholder.
 */
export function ManifestVariableFields({
  vars,
  values,
  onChange,
  errors,
}: {
  vars: ManifestVariable[];
  values: Record<string, unknown>;
  onChange: (name: string, value: string) => void;
  errors: Record<string, string>;
}) {
  const { t, locale } = useI18n();
  if (vars.length === 0) return null;
  // Required first: they block saving.
  const sorted = [...vars].sort((a, b) => Number(b.required) - Number(a.required));
  return (
    <>
      {sorted.map((v) => (
        <Field
          key={v.name}
          label={variableLabel(v.name, t)}
          optional={!v.required}
          hint={variableDescription(v, locale)}
          error={errors[`config.${v.name}`]}
        >
          <Input
            value={typeof values[v.name] === "string" ? (values[v.name] as string) : ""}
            placeholder={v.default ? t("upstreams.variableDefault", { value: v.default }) : undefined}
            onChange={(e) => onChange(v.name, e.target.value)}
            className="font-mono text-[12.5px]"
            spellCheck={false}
          />
        </Field>
      ))}
    </>
  );
}
