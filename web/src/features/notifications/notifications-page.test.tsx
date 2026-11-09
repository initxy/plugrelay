import { screen, waitFor, within } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { translate } from "@/i18n";
import { adminMe, apiError, renderPage, type ApiBody, type ApiRoutes } from "@/test/api";
import { expectPlainWords } from "@/test/plain-words";
import { NotificationsPage } from "./notifications-page";
import type { Channel } from "./shared";

const EVENTS: ApiBody<"GET /notifications/channels">["events"] = [
  "credential_quarantined",
  "credential_cooling",
  "credential_quota_low",
  "credential_secret_unsaved",
  "upstream_paused",
  "upstream_recovered",
  "budget_reached",
  "extension_crashed",
];
const FIELDS = ["id", "time", "caller", "model", "status", "usage"];

const feishu: Channel = {
  id: "c1",
  name: "运维群",
  type: "feishu",
  enabled: true,
  events: ["credential_quarantined", "upstream_paused"],
  language: "zh-CN",
  url: "https://open.feishu.cn/••••cdef",
  has_secret: true,
  has_password: false,
  to: [],
  records: false,
  record_fields: [],
  created_at: "2026-11-07T08:00:00Z",
  updated_at: "2026-11-07T08:00:00Z",
};

const log: ApiBody<"GET /notifications">["items"] = [
  {
    id: "n2",
    created_at: "2026-11-07T09:30:00Z",
    event: "credential_cooling",
    status: "done",
    objects: Array.from({ length: 5 }, (_, i) => ({
      type: "credential" as const,
      id: `k${i}`,
      name: `key-${i}`,
      parent_id: "openai",
      parent_name: "OpenAI",
      detail: { reason: "rate_limit" },
    })),
    deliveries: [
      { channel_id: "c1", channel_name: "运维群", channel_type: "feishu", ok: true, attempts: 1, at: "2026-11-07T09:30:01Z" },
      { channel_id: "c2", channel_name: "值班邮箱", channel_type: "email", ok: false, error: "dial tcp: connection refused", attempts: 3, at: "2026-11-07T09:30:05Z" },
    ],
  },
  {
    id: "n1",
    created_at: "2026-11-07T09:00:00Z",
    event: "budget_reached",
    status: "done",
    objects: [{ type: "key", id: "k9", name: "laptop", detail: { percent: "85", window: "monthly", kind: "spend" } }],
    deliveries: [],
  },
];

const on = adminMe({ pages: ["notifications.channels"], features: ["notifications"] });

function routes() {
  const items: Channel[] = [feishu];
  const posted: unknown[] = [];
  const r: ApiRoutes = {
    "GET /notifications/channels": () => ({ items, events: EVENTS, record_fields: FIELDS, public_url_set: false }),
    "GET /notifications": { items: log },
    "POST /notifications/channels": ({ body }) => {
      posted.push(body);
      if (!body.url && body.type !== "email")
        return apiError(422, "invalid", "url: must be an absolute http(s) URL", {
          params: { count: 1 },
          fields: [{ path: "url", code: "url_http", message: "must be an absolute http(s) URL" }],
        });
      const c: Channel = {
        ...feishu,
        id: "c9",
        name: body.name ?? "",
        type: body.type ?? "webhook",
        events: body.events ?? [],
        records: body.records ?? false,
        record_fields: body.record_fields ?? [],
        has_secret: !!body.secret,
      };
      items.push(c);
      return c;
    },
    "POST /notifications/channels/{id}/test": { ok: false, results: [{ ok: false, attempts: 3, latency_ms: 900, error: "HTTP 404" }] },
  };
  return { items, posted, routes: r };
}

describe("NotificationsPage", () => {
  it("shows channels and the log in plain words (zh-CN)", async () => {
    renderPage(<NotificationsPage />, { locale: "zh-CN", routes: routes().routes, me: on });
    expect(await screen.findByText("运维群", { selector: "button" })).toBeInTheDocument();
    expect(screen.getByText("2 种提醒")).toBeInTheDocument();
    expect(screen.getByText(translate("zh-CN", "notify.noLinkTitle"))).toBeInTheDocument();
    expect(await screen.findByText("凭据长时间暂停")).toBeInTheDocument();
    expect(screen.getByText("OpenAI / key-0、OpenAI / key-1、OpenAI / key-2")).toBeInTheDocument();
    expect(screen.getByText("等 2 个")).toBeInTheDocument();
    expect(screen.getByText("没有渠道接收")).toBeInTheDocument();
    expect(screen.getAllByRole("heading", { level: 1 })).toHaveLength(1);
    expectPlainWords();
  });

  it("shows the feature-off page while notifications are off", async () => {
    renderPage(<NotificationsPage />, { locale: "zh-CN", routes: routes().routes, me: adminMe() });
    expect(await screen.findByText(translate("zh-CN", "features.pageOff"))).toBeInTheDocument();
  });

  it("adds a webhook that also gets records, after a field error", async () => {
    const r = routes();
    const { user } = renderPage(<NotificationsPage />, { routes: r.routes, me: on, locale: "en" });
    await screen.findByText("运维群", { selector: "button" });
    await user.click(screen.getByRole("button", { name: "Add channel" }));
    const drawer = await screen.findByRole("dialog");
    await user.click(within(drawer).getByText("Webhook"));
    await user.type(within(drawer).getByLabelText("Name"), "Billing hook");
    await user.click(within(drawer).getByRole("button", { name: "Save" }));
    expect(await within(drawer).findByText(translate("en", "apiError.url_http"))).toBeInTheDocument();
    await user.type(within(drawer).getByLabelText("Address"), "https://hooks.example.com/pr");
    await user.click(within(drawer).getByRole("radio", { name: "Only these" }));
    await user.click(within(drawer).getByRole("checkbox", { name: "Upstream paused" }));
    await user.click(within(drawer).getByRole("switch", { name: "Also push request records" }));
    await user.click(within(drawer).getByRole("checkbox", { name: "Request and reply text" }));
    await user.click(within(drawer).getByRole("button", { name: "Save" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    expect(r.posted.at(-1)).toMatchObject({
      type: "webhook",
      name: "Billing hook",
      url: "https://hooks.example.com/pr",
      events: ["upstream_paused"],
      records: true,
      record_fields: [...FIELDS, "body"],
    });
  });

  it("sends a test message and keeps the result on the row", async () => {
    const { user } = renderPage(<NotificationsPage />, { routes: routes().routes, me: on, locale: "en" });
    await screen.findByText("运维群", { selector: "button" });
    await user.click(screen.getByRole("button", { name: "Send test" }));
    expect(await screen.findByText("Last test failed")).toBeInTheDocument();
  });

  it("shows the new and the edit drawer in plain words (zh-CN)", async () => {
    const { user } = renderPage(<NotificationsPage />, { routes: routes().routes, me: on, locale: "zh-CN" });
    await screen.findByText("运维群", { selector: "button" });
    await user.click(screen.getByRole("button", { name: translate("zh-CN", "notify.add") }));
    const drawer = await screen.findByRole("dialog");
    expectPlainWords(drawer);
    await user.click(within(drawer).getByText(translate("zh-CN", "notify.type.email")));
    expectPlainWords(drawer);
    await user.click(within(drawer).getByRole("button", { name: translate("zh-CN", "common.close") }));
    await user.click(await screen.findByRole("button", { name: translate("zh-CN", "unsaved.discard") }));
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    await user.click(screen.getByRole("button", { name: translate("zh-CN", "common.edit") }));
    expectPlainWords(await screen.findByRole("dialog"));
  });
});
