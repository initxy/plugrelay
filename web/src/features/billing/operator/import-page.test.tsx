import { screen, waitFor } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { translate } from "@/i18n";
import { renderPage, type ApiRequest, type ApiRoutes } from "@/test/api";
import { expectPlainWords } from "@/test/plain-words";
import type { ImportReport } from "../api";
import { ImportPage } from "./import-page";
import { billingAdmin } from "./test-fixtures";

const report = (dry: boolean): ImportReport => ({
  dry_run: dry,
  counts: { upstream: 2, user: 3, key: 4, skipped: 1 },
  items: [
    { kind: "upstream", source: "c7", id: "newapi-c7", op: "create" },
    { kind: "upstream", source: "c9", id: "newapi-c9", op: "skip", reason: "channel type 36 (Suno) has no manifest" },
  ],
  warnings: ["user 3: argon2 password hash cannot be carried over"],
});

function routes() {
  const runs: unknown[] = [];
  const r: ApiRoutes = {
    "POST /api/plugins/admin.import-newapi/run": ({ body }: ApiRequest) => {
      runs.push(body);
      return report((body as { dry_run: boolean }).dry_run);
    },
  };
  return { r, runs };
}

describe("ImportPage", () => {
  it("checks first, then imports the same input, and starts over on a change", async () => {
    const { r, runs } = routes();
    const { user } = renderPage(<ImportPage />, { routes: r, me: billingAdmin });
    expect(await screen.findByRole("button", { name: "Import" })).toBeDisabled();
    await user.type(screen.getByLabelText("Connection"), "root:pw@tcp(db:3306)/newapi");
    await user.click(screen.getByRole("button", { name: "Check what would change" }));
    expect(await screen.findByText("What would change")).toBeInTheDocument();
    expect(screen.getByText("channel type 36 (Suno) has no manifest")).toBeInTheDocument();
    expect(runs[0]).toMatchObject({ driver: "mysql", dsn: "root:pw@tcp(db:3306)/newapi", dry_run: true });
    await user.click(screen.getByRole("button", { name: "Import" }));
    expect(await screen.findByText("What was imported")).toBeInTheDocument();
    expect(runs[1]).toMatchObject({ dry_run: false });
    await user.type(screen.getByLabelText("Connection"), "x");
    await waitFor(() => expect(screen.getByRole("button", { name: "Import" })).toBeDisabled());
  });

  it("needs the local-file confirmation for SQLite", async () => {
    const { user } = renderPage(<ImportPage />, { routes: routes().r, me: billingAdmin });
    await user.click(await screen.findByRole("radio", { name: "SQLite" }));
    await user.type(screen.getByLabelText("Connection"), "/data/one-api.db");
    expect(screen.getByRole("button", { name: "Check what would change" })).toBeDisabled();
    await user.click(screen.getByRole("checkbox"));
    expect(screen.getByRole("button", { name: "Check what would change" })).toBeEnabled();
  });

  it("shows the wizard and its report in plain words (zh-CN)", async () => {
    const { user } = renderPage(<ImportPage />, { locale: "zh-CN", routes: routes().r, me: billingAdmin });
    await user.click(await screen.findByRole("radio", { name: translate("zh-CN", "billing.op.imp.fromJson") }));
    await user.type(screen.getByLabelText(translate("zh-CN", "billing.op.imp.json")), "{{}");
    await user.click(screen.getByRole("button", { name: translate("zh-CN", "billing.op.imp.dryRun") }));
    expect(await screen.findByText(translate("zh-CN", "billing.op.imp.dryTitle"))).toBeInTheDocument();
    expectPlainWords();
  });
});
