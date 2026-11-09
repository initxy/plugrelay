import { useQuery, useQueryClient } from "@tanstack/react-query";
import { EmptyState, ErrorState, LoadingRows } from "@/components/page";
import { api, unwrap } from "@/lib/api/client";
import type { SettingValues } from "@/lib/api/types";
import { useI18n } from "@/i18n";
import { SettingsForm } from "./settings-form";

type Layer = "group" | "user" | "key";

const ENDPOINT = {
  group: {
    get: (id: string) => api.GET("/groups/{id}/settings", { params: { path: { id } } }),
    put: (id: string, settings: SettingValues) => api.PUT("/groups/{id}/settings", { params: { path: { id } }, body: { settings } }),
  },
  user: {
    get: (id: string) => api.GET("/users/{id}/settings", { params: { path: { id } } }),
    put: (id: string, settings: SettingValues) => api.PUT("/users/{id}/settings", { params: { path: { id } }, body: { settings } }),
  },
  key: {
    get: (id: string) => api.GET("/keys/{id}/settings", { params: { path: { id } } }),
    put: (id: string, settings: SettingValues) => api.PUT("/keys/{id}/settings", { params: { path: { id } }, body: { settings } }),
  },
} as const;

/**
 * The settings tab of a group, user or key drawer: the settings that turned-on features add to
 * that object's layer. `hint` says what the layer is for; `saved` is the toast after a save.
 */
export function LayerSettingsTab({ layer, id, hint, saved, onDirty }: { layer: Layer; id: string; hint: string; saved: string; onDirty: (dirty: boolean) => void }) {
  const { t } = useI18n();
  const qc = useQueryClient();
  const key = [`${layer}s`, id, "settings"];
  const q = useQuery({ queryKey: key, queryFn: () => unwrap(ENDPOINT[layer].get(id)) });
  if (q.isError)
    return (
      <div className="p-5">
        <ErrorState error={q.error} onRetry={() => void q.refetch()} />
      </div>
    );
  if (q.isPending)
    return (
      <div className="p-5">
        <LoadingRows rows={5} />
      </div>
    );
  return (
    <SettingsForm
      items={q.data.items}
      layer={layer}
      saved={saved}
      onDirtyChange={onDirty}
      save={async (body) => qc.setQueryData(key, await unwrap(ENDPOINT[layer].put(id, body)))}
      empty={<EmptyState compact title={t("layer.empty")} />}
      intro={<p className="mb-4 text-xs text-ink-3">{hint}</p>}
      className="px-5 pt-5"
    />
  );
}
