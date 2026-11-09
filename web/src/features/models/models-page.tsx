import { useMutation, useQueryClient } from "@tanstack/react-query";
import { Plus, RefreshCw } from "lucide-react";
import { toast } from "sonner";
import { PageHeader } from "@/components/page";
import { Button } from "@/components/ui/button";
import { Tabs, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { api, unwrap } from "@/lib/api/client";
import { errorMessage } from "@/lib/api/errors";
import { cn } from "@/lib/cn";
import { pickEnum } from "@/lib/url-filters";
import { useDrawerParam, useUrlSearch } from "@/lib/use-url-search";
import { useI18n } from "@/i18n";
import { ModelsTab } from "./models-tab";
import { PresetsTab } from "./presets-tab";
import { PricingTab } from "./pricing-tab";
import { useModels, usePresets, usePricing } from "./queries";

const TABS = ["models", "presets", "pricing"] as const;
type Tab = (typeof TABS)[number];

/** Models (with their routes and prices), presets and the price list, as tabs (`?tab=`). */
export function ModelsPage() {
  const { t } = useI18n();
  const qc = useQueryClient();
  const [search, setSearch] = useUrlSearch();
  const drawer = useDrawerParam();
  const tab: Tab = pickEnum(search.tab, TABS) ?? "models";
  const models = useModels();
  const presets = usePresets();
  const pricing = usePricing();
  const sync = useMutation({
    mutationFn: () => unwrap(api.POST("/models/sync", { body: {} })),
    onSuccess: (r) => {
      void qc.invalidateQueries({ queryKey: ["models"] });
      const msg = t("models.syncDone", { added: r.added.length, disabled: r.disabled.length, unchanged: r.unchanged });
      if (r.failed.length) toast.warning(msg, { description: r.failed.map((f) => `${f.upstream_id}: ${f.error}`).join("\n") });
      else toast.success(msg);
    },
    onError: (e) => toast.error(errorMessage(e)),
  });

  const count = (n: number | undefined) => (n === undefined ? null : <span className="ml-1.5 tabular-nums text-ink-3">{n}</span>);
  const actions =
    tab === "models" ? (
      <>
        <Button onClick={() => sync.mutate()} disabled={sync.isPending}>
          <RefreshCw className={cn(sync.isPending && "animate-spin")} />
          {sync.isPending ? t("models.syncing") : t("models.sync")}
        </Button>
        <Button variant="primary" onClick={() => drawer.open("new")}>
          <Plus />
          {t("models.add")}
        </Button>
      </>
    ) : tab === "presets" ? (
      <Button variant="primary" onClick={() => drawer.open("new")}>
        <Plus />
        {t("presets.add")}
      </Button>
    ) : (
      <Button variant="primary" onClick={() => drawer.open("new")}>
        <Plus />
        {t("pricing.add")}
      </Button>
    );

  return (
    <>
      <PageHeader title={t("nav.models")} description={t(`models.description.${tab}`)} actions={actions} className="mb-3" />
      <Tabs value={tab} onValueChange={(v) => setSearch({ tab: v === "models" ? undefined : v, id: undefined, q: undefined, upstream: undefined, state: undefined })}>
        <TabsList className="mb-4 px-0">
          <TabsTrigger value="models">
            {t("models.tab.models")}
            {count(models.data?.items.length)}
          </TabsTrigger>
          <TabsTrigger value="presets">
            {t("models.tab.presets")}
            {count(presets.data?.items.length)}
          </TabsTrigger>
          <TabsTrigger value="pricing">
            {t("models.tab.pricing")}
            {count(pricing.data?.items.length)}
          </TabsTrigger>
        </TabsList>
      </Tabs>
      {tab === "models" && <ModelsTab />}
      {tab === "presets" && <PresetsTab />}
      {tab === "pricing" && <PricingTab />}
    </>
  );
}
