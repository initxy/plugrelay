import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import type { ColumnDef } from "@tanstack/react-table";
import { Megaphone, Plus, Trash2 } from "lucide-react";
import { useMemo } from "react";
import { toast } from "sonner";
import { useConfirm } from "@/components/confirm";
import { DataTable } from "@/components/data-table";
import { Drawer } from "@/components/drawer";
import { HelpText } from "@/components/form-kit";
import { Markdown } from "@/components/markdown";
import { EmptyState, ErrorState, PageHeader } from "@/components/page";
import { Pill, type Tone } from "@/components/status";
import { Button } from "@/components/ui/button";
import { Field } from "@/components/ui/field";
import { Input, Textarea } from "@/components/ui/input";
import { Select } from "@/components/ui/select";
import { errorMessage } from "@/lib/api/errors";
import { useDrawerParam } from "@/lib/use-url-search";
import { useI18n } from "@/i18n";
import { announcementsApi, type Announcement, type Audience } from "../api";
import { OffBanner, fromLocalInput, toLocalInput, useWindowText } from "./parts";
import { useForm } from "@/lib/use-form";

const listQuery = { queryKey: ["billing", "announcements"], queryFn: () => announcementsApi.all() };
const STATE_TONE: Record<NonNullable<Announcement["state"]>, Tone> = { live: "ok", scheduled: "info", ended: "neutral" };
const AUDIENCES: readonly Audience[] = ["all", "members", "admins"];

/** Notices shown as a banner at the top of the console while they run. */
export function AnnouncementsPage() {
  const { t } = useI18n();
  const windowText = useWindowText();
  const drawer = useDrawerParam();
  const q = useQuery(listQuery);
  const items = q.data?.items;
  const columns = useMemo<ColumnDef<Announcement>[]>(
    () => [
      {
        id: "title",
        header: t("billing.op.ann.titleCol"),
        cell: ({ row }) => (
          <span className="grid min-w-0">
            <span className="truncate font-medium">{row.original.title}</span>
            <span className="truncate text-xs text-ink-3">{row.original.body.split("\n")[0]?.replace(/[*`]|^#+\s*|^[-*]\s+/g, "")}</span>
          </span>
        ),
      },
      { id: "audience", header: t("billing.op.ann.audience"), meta: { className: "w-[130px]" }, cell: ({ row }) => t(`billing.op.ann.aud.${row.original.audience}`) },
      {
        id: "window",
        header: t("billing.op.ann.window"),
        meta: { className: "w-[260px] max-lg:hidden" },
        cell: ({ row }) => <span className="text-xs text-ink-2">{windowText(row.original.starts_at, row.original.ends_at)}</span>,
      },
      {
        id: "state",
        header: t("billing.op.ann.state"),
        meta: { className: "w-[110px]" },
        cell: ({ row }) => {
          const s = row.original.state ?? "live";
          return (
            <Pill tone={STATE_TONE[s]} dot>
              {t(`billing.op.ann.stateName.${s}`)}
            </Pill>
          );
        },
      },
    ],
    [t, windowText],
  );
  const add = (
    <Button variant="primary" onClick={() => drawer.open("new")}>
      <Plus aria-hidden />
      {t("billing.op.ann.add")}
    </Button>
  );
  return (
    <>
      <PageHeader title={t("nav.billing.announcements")} description={t("billing.op.ann.description")} actions={add} />
      {q.data && !q.data.enabled && <OffBanner title={t("billing.op.ann.offTitle")} hint={t("billing.op.ann.offHint")} />}
      {q.isError ? (
        <ErrorState error={q.error} onRetry={() => void q.refetch()} />
      ) : (
        <DataTable
          columns={columns}
          data={items}
          getRowId={(a) => a.id}
          onRowClick={(a) => drawer.open(a.id)}
          selectedId={drawer.id}
          empty={<EmptyState icon={Megaphone} title={t("billing.op.ann.empty")} hint={t("billing.op.ann.emptyHint")} action={add} />}
        />
      )}
      {drawer.id && (drawer.id === "new" || items) && (
        <AnnouncementDrawer key={drawer.id} item={drawer.id === "new" ? undefined : items?.find((a) => a.id === drawer.id)} onClose={drawer.close} />
      )}
    </>
  );
}

function AnnouncementDrawer({ item, onClose }: { item?: Announcement; onClose: () => void }) {
  const { t } = useI18n();
  const qc = useQueryClient();
  const confirm = useConfirm();
  const form = useForm({
    title: item?.title ?? "",
    body: item?.body ?? "",
    audience: (item?.audience ?? "all") as Audience,
    starts: toLocalInput(item?.starts_at),
    ends: toLocalInput(item?.ends_at),
  });
  const { title, body, audience, starts, ends } = form.values;
  const windowBad = !!starts && !!ends && new Date(ends) <= new Date(starts);
  const refresh = () => void qc.invalidateQueries({ queryKey: listQuery.queryKey });
  const save = useMutation({
    mutationFn: () => {
      const input = { title: title.trim(), body, audience, starts_at: fromLocalInput(starts), ends_at: fromLocalInput(ends) };
      return item ? announcementsApi.update(item.id, input) : announcementsApi.create(input);
    },
    onSuccess: () => {
      toast.success(t("billing.op.ann.saved"));
      refresh();
      onClose();
    },
    onError: (e) => toast.error(errorMessage(e)),
  });
  const remove = useMutation({
    mutationFn: () => announcementsApi.remove(item!.id),
    onSuccess: () => {
      toast.success(t("billing.op.ann.deleted"));
      refresh();
      onClose();
    },
    onError: (e) => toast.error(errorMessage(e)),
  });
  return (
    <Drawer
      open
      onOpenChange={(o) => !o && onClose()}
      size="lg"
      title={item ? item.title : t("billing.op.ann.add")}
      subtitle={t("billing.op.ann.drawerHint")}
      footer={
        <>
          {item && (
            <Button
              variant="danger"
              className="mr-auto"
              onClick={async () => {
                if (await confirm({ title: t("billing.op.ann.deleteTitle", { title: item.title }), body: t("billing.op.ann.deleteBody"), confirm: t("common.delete"), danger: true }))
                  remove.mutate();
              }}
            >
              <Trash2 aria-hidden />
              {t("common.delete")}
            </Button>
          )}
          <Button onClick={onClose}>{t("common.cancel")}</Button>
          <Button variant="primary" disabled={!title.trim() || windowBad || save.isPending} onClick={() => save.mutate()}>
            {save.isPending ? t("common.saving") : t("common.save")}
          </Button>
        </>
      }
    >
      <div className="grid gap-4 p-5">
        <Field label={t("billing.op.ann.titleField")}>
          <Input value={title} onChange={(e) => form.set("title", e.target.value)} maxLength={120} placeholder={t("billing.op.ann.titlePlaceholder")} />
        </Field>
        <div className="grid gap-4 md:grid-cols-2">
          <Field label={t("billing.op.ann.body")} hint={t("billing.op.ann.bodyHint")}>
            <Textarea value={body} onChange={(e) => form.set("body", e.target.value)} rows={10} maxLength={10000} className="font-mono text-xs" />
          </Field>
          <div className="grid content-start gap-1.5">
            <span className="text-xs font-semibold">{t("billing.op.ann.preview")}</span>
            <div className="min-h-[200px] rounded-lg border border-line bg-surface-2 p-3">
              <p className="mb-1.5 text-sm font-semibold">{title || t("billing.op.ann.titlePlaceholder")}</p>
              {body.trim() ? <Markdown source={body} /> : <p className="text-sm text-ink-3">{t("billing.op.ann.previewEmpty")}</p>}
            </div>
          </div>
        </div>
        <Field label={t("billing.op.ann.audience")}>
          <Select value={audience} onChange={(v) => form.set("audience", v)} options={AUDIENCES.map((a) => ({ value: a, label: t(`billing.op.ann.aud.${a}`) }))} />
        </Field>
        <div className="grid grid-cols-2 gap-4">
          <Field label={t("billing.op.ann.starts")} optional hint={t("billing.op.ann.startsHint")}>
            <Input type="datetime-local" value={starts} onChange={(e) => form.set("starts", e.target.value)} />
          </Field>
          <Field label={t("billing.op.ann.ends")} optional hint={t("billing.op.ann.endsHint")} error={windowBad ? t("billing.err.announcement_window") : undefined}>
            <Input type="datetime-local" value={ends} onChange={(e) => form.set("ends", e.target.value)} />
          </Field>
        </div>
        <HelpText>{t("billing.op.ann.dismissHelp")}</HelpText>
      </div>
    </Drawer>
  );
}
