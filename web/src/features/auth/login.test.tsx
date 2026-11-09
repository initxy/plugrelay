import { screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { apiError, adminMe, renderPage, reply } from "@/test/api";
import type { AuthProviders } from "@/lib/api/types";
import { translate } from "@/i18n";
import { expectPlainWords } from "@/test/plain-words";
import { CaptchaWidget } from "./captcha-widget";
import { LoginPage } from "./login-page";
import { ssoStartHref } from "./sso-buttons";
import { fromB64url, toB64url } from "./webauthn";

const providers = (over: Partial<AuthProviders> = {}): AuthProviders => ({
  password_login: true,
  providers: [
    { id: "google", name: "Google", preset: "google" },
    { id: "corp", name: "Corp SSO", preset: "oidc" },
  ],
  site: { name: "Acme relay" },
  ...over,
});

afterEach(() => {
  delete window.turnstile;
  vi.unstubAllGlobals();
});

describe("LoginPage", () => {
  it("names the relay and links each provider to its start address, keeping where to go next", async () => {
    renderPage(<LoginPage />, {
      locale: "zh-CN",
      routes: { "GET /auth/providers": providers() },
      path: "/login?next=/keys",
      me: null,
    });
    expect(await screen.findByText("登录 Acme relay。")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: /Google/ })).toHaveAttribute("href", "/api/v1/auth/sso/google/start?return_to=%2Fkeys");
    expect(screen.getByRole("link", { name: /Corp SSO/ })).toHaveAttribute("href", "/api/v1/auth/sso/corp/start?return_to=%2Fkeys");
    expectPlainWords();
  });

  it("shows why the last single sign-on failed, and names unknown codes", async () => {
    const { unmount } = renderPage(<LoginPage />, {
      routes: { "GET /auth/providers": providers() },
      path: "/login?sso_error=domain_not_allowed",
      locale: "en",
      me: null,
    });
    expect(await screen.findByRole("alert")).toHaveTextContent("Your email domain is not allowed to sign in here.");
    unmount();
    renderPage(<LoginPage />, {
      routes: { "GET /auth/providers": providers() },
      path: "/login?sso_error=weird",
      locale: "en",
      me: null,
    });
    expect(await screen.findByRole("alert")).toHaveTextContent("Sign-in failed (weird).");
  });

  it("hides the password form when password sign-in is off", async () => {
    renderPage(<LoginPage />, {
      routes: { "GET /auth/providers": providers({ password_login: false }) },
      locale: "en",
      me: null,
    });
    expect(await screen.findByText("Password sign-in is turned off on this relay.")).toBeInTheDocument();
    expect(screen.queryByLabelText("Username")).not.toBeVisible();
  });

  it("asks for the authenticator code after the password and signs in only then", async () => {
    const mfa = vi.fn(({ body }) => ((body as { code: string }).code === "123456" ? adminMe() : apiError(401, "mfa_invalid", "wrong or already used code")));
    const { user, queryClient } = renderPage(<LoginPage />, {
      locale: "en",
      me: null,
      routes: {
        "GET /auth/providers": providers({ providers: [] }),
        "POST /auth/login": reply(202, { methods: ["totp", "recovery"] }),
        "POST /auth/mfa": mfa,
      },
    });
    await user.type(await screen.findByLabelText("Username"), "admin");
    await user.type(screen.getByLabelText("Password"), "correct-horse-1");
    await user.click(screen.getByRole("button", { name: "Log in" }));
    expect(await screen.findByRole("heading", { name: "Two-step sign-in" })).toBeInTheDocument();
    expect(queryClient.getQueryData(["me"])).toBeUndefined();

    const code = screen.getByLabelText("Code from your authenticator app");
    await user.type(code, "111111");
    await user.click(screen.getByRole("button", { name: "Verify" }));
    expect(await screen.findByText("Wrong code, or it was already used.")).toBeInTheDocument();

    await user.type(code, "123456");
    await user.click(screen.getByRole("button", { name: "Verify" }));
    await waitFor(() => expect(queryClient.getQueryData(["me"])).toBeDefined());
  });

  it("accepts a recovery code instead", async () => {
    const { user, api, queryClient } = renderPage(<LoginPage />, {
      locale: "en",
      me: null,
      routes: {
        "GET /auth/providers": providers({ providers: [] }),
        "POST /auth/login": reply(202, { methods: ["totp", "recovery"] }),
        "POST /auth/mfa": adminMe(),
      },
    });
    await user.type(await screen.findByLabelText("Username"), "admin");
    await user.type(screen.getByLabelText("Password"), "x");
    await user.click(screen.getByRole("button", { name: "Log in" }));
    await user.click(await screen.findByRole("button", { name: "Use a recovery code" }));
    await user.type(screen.getByLabelText("Recovery code"), "K7M2-P9QA-X3FD");
    await user.click(screen.getByRole("button", { name: "Verify" }));
    await waitFor(() => expect(queryClient.getQueryData(["me"])).toBeDefined());
    expect(api.calls.find((c) => c.path === "/auth/mfa")?.body).toEqual({
      method: "recovery",
      code: "K7M2-P9QA-X3FD",
    });
  });

  it("shows a captcha after failed sign-ins and sends its token", async () => {
    const render = vi.fn((_el: HTMLElement, opts: Record<string, unknown>) => {
      (opts.callback as (t: string) => void)("tok-" + render.mock.calls.length);
      return "w1";
    });
    window.turnstile = { render, remove: vi.fn() };
    let tries = 0;
    const { user, api, queryClient } = renderPage(<LoginPage />, {
      locale: "en",
      me: null,
      routes: {
        "GET /auth/providers": providers({
          providers: [],
          captcha: { provider: "turnstile", site_key: "site-key", after_failures: 1 },
        }),
        "POST /auth/login": () => (++tries === 1 ? apiError(401, "wrong_password", "wrong username or password") : adminMe()),
      },
    });
    await user.type(await screen.findByLabelText("Username"), "nobody");
    await user.type(screen.getByLabelText("Password"), "x");
    expect(screen.queryByTestId("captcha-widget")).not.toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Log in" }));
    expect(await screen.findByTestId("captcha-widget")).toBeInTheDocument();
    await waitFor(() => expect(render).toHaveBeenCalled());
    await user.click(screen.getByRole("button", { name: "Log in" }));
    await waitFor(() => expect(queryClient.getQueryData(["me"])).toBeDefined());
    expect(api.calls.filter((c) => c.path === "/auth/login").at(-1)?.body).toMatchObject({
      captcha_token: expect.stringMatching(/^tok-/),
    });
  });
});

describe("CaptchaWidget", () => {
  it("renders the provider widget and reports its token", async () => {
    const render = vi.fn((_el: HTMLElement, opts: Record<string, unknown>) => {
      (opts.callback as (t: string) => void)("tok-1");
      return "w1";
    });
    window.turnstile = { render, remove: vi.fn() };
    const onToken = vi.fn();
    renderPage(<CaptchaWidget provider="turnstile" siteKey="site-key" onToken={onToken} />, {
      locale: "zh-CN",
      me: null,
    });
    await waitFor(() => expect(onToken).toHaveBeenCalledWith("tok-1"));
    expect(render.mock.calls[0]![1]).toMatchObject({ sitekey: "site-key" });
  });
});

describe("helpers", () => {
  it("escapes the provider id and return path", () => {
    expect(ssoStartHref("a b")).toBe("/api/v1/auth/sso/a%20b/start");
    expect(ssoStartHref("gh", "/x?y=1")).toBe("/api/v1/auth/sso/gh/start?return_to=%2Fx%3Fy%3D1");
  });

  it("round-trips base64url bytes without padding", () => {
    const bytes = new Uint8Array([0, 251, 255, 1, 2]);
    const s = toB64url(bytes);
    expect(s).toBe("APv_AQI");
    expect(new Uint8Array(fromB64url(s))).toEqual(bytes);
  });
});

describe("LoginPage in plain words (zh-CN)", () => {
  it("shows the two-step sign-in step without raw ids", async () => {
    const { user } = renderPage(<LoginPage />, {
      locale: "zh-CN",
      me: null,
      routes: {
        "GET /auth/providers": providers({ providers: [] }),
        "POST /auth/login": reply(202, { methods: ["totp", "recovery", "passkey"] }),
      },
    });
    await user.type(await screen.findByLabelText(translate("zh-CN", "login.username")), "admin");
    await user.type(screen.getByLabelText(translate("zh-CN", "login.password")), "correct-horse-1");
    await user.click(screen.getByRole("button", { name: translate("zh-CN", "login.submit") }));
    expect(await screen.findByRole("heading", { name: translate("zh-CN", "mfa.title") })).toBeInTheDocument();
    expectPlainWords();
  });
});
