import { screen, waitFor, within } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { translate } from "@/i18n";
import { renderPage, type ApiRequest, type ApiRoutes } from "@/test/api";
import { expectPlainWords } from "@/test/plain-words";
import type { Announcement } from "../api";
import { AnnouncementsPage } from "./announcements-page";
import { billingAdmin } from "./test-fixtures";

const maint: Announcement = {
  id: "ann_1",
  title: "Maintenance tonight",
  body: "We restart at **22:00**.\n\n- Requests pause for a minute\n- See [status](https://status.example.com)",
  audience: "members",
  ends_at: "2026-11-08T00:00:00Z",
  created_at: "2026-11-07T00:00:00Z",
  updated_at: "2026-11-07T00:00:00Z",
  state: "live",
};

function routes(enabled = true) {
  const saved: unknown[] = [];
  const deleted: string[] = [];
  const r: ApiRoutes = {
    "GET /api/plugins/admin.announcements/announcements": { items: [maint], enabled },
    "POST /api/plugins/admin.announcements/announcements": ({ body }: ApiRequest) => {
      saved.push(body);
      return { ...maint, id: "ann_2", ...(body as object) };
    },
    "PUT /api/plugins/admin.announcements/announcements/{id}": ({ body }: ApiRequest) => ({ ...maint, ...(body as object) }),
    "DELETE /api/plugins/admin.announcements/announcements/{id}": ({ params }: ApiRequest) => {
      deleted.push(params.id!);
      return undefined;
    },
  };
  return { r, saved, deleted };
}

describe("AnnouncementsPage", () => {
  it("lists announcements and says when they are off (zh-CN)", async () => {
    renderPage(<AnnouncementsPage />, { locale: "zh-CN", routes: routes(false).r, me: billingAdmin });
    expect(await screen.findByText("Maintenance tonight")).toBeInTheDocument();
    expect(screen.getByText(translate("zh-CN", "billing.op.ann.offTitle"))).toBeInTheDocument();
    expect(screen.getByText(translate("zh-CN", "billing.op.ann.aud.members"))).toBeInTheDocument();
    expectPlainWords();
  });

  it("writes one with a live preview", async () => {
    const { r, saved } = routes();
    const { user } = renderPage(<AnnouncementsPage />, { routes: r, me: billingAdmin });
    await screen.findByText("Maintenance tonight");
    await user.click(screen.getByRole("button", { name: "New announcement" }));
    const drawer = await screen.findByRole("dialog");
    await user.type(within(drawer).getByLabelText("Title"), "New prices");
    await user.type(within(drawer).getByLabelText("Text"), "Cheaper **Claude** from today");
    expect(within(drawer).getByText("Claude", { selector: "strong" })).toBeInTheDocument();
    await user.click(within(drawer).getByRole("button", { name: "Save" }));
    await waitFor(() => expect(saved).toHaveLength(1));
    expect(saved[0]).toMatchObject({ title: "New prices", body: "Cheaper **Claude** from today", audience: "all" });
  });

  it("edits and deletes one (zh-CN)", async () => {
    const { r, deleted } = routes();
    const { user } = renderPage(<AnnouncementsPage />, { locale: "zh-CN", routes: r, me: billingAdmin, path: "/billing/announcements?id=ann_1" });
    const drawer = await screen.findByRole("dialog");
    expect(within(drawer).getByRole("link", { name: "status" })).toHaveAttribute("href", "https://status.example.com");
    expectPlainWords(drawer);
    await user.click(within(drawer).getByRole("button", { name: translate("zh-CN", "common.delete") }));
    const confirm = await screen.findByRole("alertdialog");
    await user.click(within(confirm).getByRole("button", { name: translate("zh-CN", "common.delete") }));
    await waitFor(() => expect(deleted).toEqual(["ann_1"]));
  });
});
