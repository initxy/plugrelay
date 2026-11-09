import { screen, waitFor } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { meQuery } from "@/app/session";
import { adminMe, apiError, renderPage, type ApiRoutes } from "@/test/api";
import { expectPlainWords } from "@/test/plain-words";
import { SetupPage } from "./setup-page";

const status = { needs_setup: true, version: "dev", listen: "127.0.0.1:8080" };

async function fillAccount(user: ReturnType<typeof renderPage>["user"], password: string, confirm = password) {
  await user.type(await screen.findByLabelText("Password"), password);
  await user.type(screen.getByLabelText("Repeat password"), confirm);
  await user.click(screen.getByRole("button", { name: "Next" }));
}

describe("SetupPage", () => {
  it("walks account, usage and address, then lands in the console", async () => {
    const r: ApiRoutes = { "GET /setup/status": status, "POST /setup": adminMe() };
    const { user, router, queryClient, api: stub } = renderPage(<SetupPage />, { path: "/setup", routes: r });
    expect(await screen.findByLabelText("Username")).toHaveValue("admin");
    await fillAccount(user, "correct-horse-battery");

    expect(screen.getByRole("radio", { name: /A team/ })).toHaveAttribute("aria-checked", "true");
    await user.click(screen.getByRole("radio", { name: /Selling/ }));
    expect(screen.getByText("Billing: prepaid balances, top-ups, redeem codes and plans")).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Next" }));

    expect(await screen.findByText("Listening on 127.0.0.1:8080 right now.")).toBeInTheDocument();
    await user.click(screen.getByRole("switch", { name: "Accept connections from other machines" }));
    await user.type(screen.getByLabelText(/Public address/), "https://relay.example.com");
    await user.click(screen.getByRole("button", { name: "Finish and open the console" }));

    await waitFor(() => expect(router.state.location.pathname).toBe("/"));
    expect(stub.called("POST", "/setup")[0]!.body).toEqual({
      admin_username: "admin",
      admin_password: "correct-horse-battery",
      usage: "sell",
      listen_public: true,
      site_name: "plugrelay",
      currency: "USD",
      public_url: "https://relay.example.com",
    });
    expect(queryClient.getQueryData(meQuery.queryKey)?.user.username).toBe("admin@example.com");
  });

  it("keeps you on the account step until the password is long enough and repeated", async () => {
    const { user } = renderPage(<SetupPage />, { path: "/setup", routes: { "GET /setup/status": status } });
    await fillAccount(user, "short");
    expect(screen.getByLabelText("Password")).toHaveAccessibleDescription("Use at least 10 characters.");
    expect(screen.queryByRole("radiogroup")).not.toBeInTheDocument();
    await user.clear(screen.getByLabelText("Password"));
    await user.clear(screen.getByLabelText("Repeat password"));
    await fillAccount(user, "correct-horse-battery", "correct-horse-batterz");
    expect(screen.getByLabelText("Repeat password")).toHaveAccessibleDescription("The two passwords don't match.");
  });

  it("refuses a public address that is not a full URL, and shows the server's refusal", async () => {
    const r: ApiRoutes = {
      "GET /setup/status": status,
      "POST /setup": apiError(409, "setup_done", "setup has already been completed"),
    };
    const { user, router, api: stub } = renderPage(<SetupPage />, { path: "/setup", routes: r });
    await fillAccount(user, "correct-horse-battery");
    await user.click(screen.getByRole("button", { name: "Next" }));
    await user.type(await screen.findByLabelText(/Public address/), "relay.example.com");
    await user.click(screen.getByRole("button", { name: "Finish and open the console" }));
    expect(await screen.findByText("Enter a full address starting with http:// or https://")).toBeInTheDocument();
    expect(stub.called("POST", "/setup")).toHaveLength(0);

    await user.clear(screen.getByLabelText(/Public address/));
    await user.click(screen.getByRole("button", { name: "Finish and open the console" }));
    expect(await screen.findByText("Setup is already done.")).toBeInTheDocument();
    expect(router.state.location.pathname).toBe("/setup");
  });
});

describe("setup in plain words (zh-CN)", () => {
  it("walks every step without raw ids or keys", async () => {
    const { user } = renderPage(<SetupPage />, {
      path: "/setup",
      me: null,
      routes: { "GET /setup/status": status },
      locale: "zh-CN",
    });
    await user.type(await screen.findByLabelText("密码"), "correct-horse-battery");
    expectPlainWords();
    await user.type(screen.getByLabelText("再输一遍密码"), "correct-horse-battery");
    await user.click(screen.getByRole("button", { name: "下一步" }));
    await user.click(await screen.findByRole("radio", { name: /对外卖额度/ }));
    expectPlainWords();
    await user.click(screen.getByRole("button", { name: "下一步" }));
    expect(await screen.findByRole("button", { name: "完成，进入控制台" })).toBeInTheDocument();
    expectPlainWords();
  });
});
