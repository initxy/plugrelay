import { screen, waitFor } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { renderPage } from "@/test/api";
import { expectPlainWords } from "@/test/plain-words";
import { SignupPage } from "./signup";

const config = { enabled: true, invite_required: false, allowed_domains: ["example.com"], min_password: 10 };

describe("SignupPage", () => {
  it("sends the invite link's referral code with the sign-up (zh-CN)", async () => {
    const { user, api } = renderPage(<SignupPage />, {
      me: null,
      path: "/signup?ref=K7Q2M9XA",
      locale: "zh-CN",
      routes: {
        "GET /api/plugins/admin.signup/config": config,
        "POST /api/plugins/admin.signup/register": { user_id: "u9", email: "new@example.com" },
      },
    });
    expect(await screen.findByText("你是受朋友邀请来的。")).toBeInTheDocument();
    expectPlainWords();
    await user.type(screen.getByLabelText("邮箱"), "new@example.com");
    await user.type(screen.getByLabelText("密码"), "correct-horse-9");
    await user.click(screen.getByRole("button", { name: "注册" }));
    await waitFor(() => expect(api.called("POST /api/plugins/admin.signup/register")).toHaveLength(1));
    expect(api.called("POST /api/plugins/admin.signup/register")[0]!.body).toMatchObject({
      email: "new@example.com",
      referral_code: "K7Q2M9XA",
    });
  });

  it("says when sign-up is closed", async () => {
    renderPage(<SignupPage />, { me: null, path: "/signup", routes: { "GET /api/plugins/admin.signup/config": { ...config, enabled: false } } });
    expect(await screen.findByText("Sign-up is closed on this relay.")).toBeInTheDocument();
  });
});
