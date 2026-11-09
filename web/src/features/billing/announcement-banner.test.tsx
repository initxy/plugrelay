import { screen, waitFor } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { memberMe, renderPage } from "@/test/api";
import { expectPlainWords } from "@/test/plain-words";
import { AnnouncementBanner } from "./announcement-banner";

const billingOn = memberMe({ features: ["limits", "routing", "request_log", "metrics", "billing"] });
const items = [
  { id: "a1", title: "Maintenance tonight", body: "**22:00–23:00**, requests may fail.\n\n- retry later", audience: "all", created_at: "", updated_at: "" },
  { id: "a2", title: "New models", body: "See [the catalog](https://relay.example.com/catalog).", audience: "all", created_at: "", updated_at: "" },
];

describe("AnnouncementBanner", () => {
  it("shows the newest announcement, the rest on demand, and dismisses one (zh-CN)", async () => {
    const { user, api } = renderPage(<AnnouncementBanner />, {
      me: billingOn,
      locale: "zh-CN",
      routes: { "GET /api/plugins/admin.announcements/me": { items }, "POST /api/plugins/admin.announcements/me/dismiss": undefined },
    });
    expect(await screen.findByText("Maintenance tonight")).toBeInTheDocument();
    expect(screen.getByText("22:00–23:00").tagName).toBe("STRONG");
    expect(screen.queryByText("New models")).not.toBeInTheDocument();
    expectPlainWords();
    await user.click(screen.getByRole("button", { name: "还有 1 条公告" }));
    expect(screen.getByRole("link", { name: "the catalog" })).toHaveAttribute("href", "https://relay.example.com/catalog");
    await user.click(screen.getByRole("button", { name: "关闭公告：Maintenance tonight" }));
    await waitFor(() => expect(screen.queryByText("Maintenance tonight")).not.toBeInTheDocument());
    expect(api.called("POST /api/plugins/admin.announcements/me/dismiss")[0]!.body).toEqual({ id: "a1" });
  });

  it("asks nothing while billing is off", async () => {
    const { api } = renderPage(<AnnouncementBanner />, { me: memberMe() });
    await new Promise((r) => setTimeout(r, 20));
    expect(api.calls).toHaveLength(0);
  });
});
