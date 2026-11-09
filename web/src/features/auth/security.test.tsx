import { screen, waitFor, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import type { Passkey, SecurityStatus } from "@/lib/api/types";
import { apiError, renderPage, reply, type ApiRoutes } from "@/test/api";
import { translate } from "@/i18n";
import { expectPlainWords } from "@/test/plain-words";
import { SecurityPage } from "./security-page";

afterEach(() => vi.unstubAllGlobals());

/** A small stateful account: what /me/security answers follows what the test did. */
function account(start: Partial<SecurityStatus> = {}) {
  const s: SecurityStatus = {
    totp_enabled: false,
    recovery_codes_left: 0,
    passkeys: [],
    has_password: true,
    required: false,
    ...start,
  };
  let reauthed = true;
  const codes = Array.from({ length: 10 }, (_, i) => `k7m2-p9qa-x3f${i}`);
  const guard = <T,>(fn: () => T) => (reauthed ? fn() : apiError(403, "reauth_required", "confirm it is you"));
  const routes: ApiRoutes = {
    "GET /me/security": () => structuredClone(s),
    "POST /me/reauth": ({ body }) => {
      reauthed = (body as { password?: string }).password === "correct-horse-1";
      return reauthed ? reply(204) : apiError(401, "reauth_failed", "That did not match");
    },
    "POST /me/totp/setup": () =>
      guard(() => ({
        secret: "JBSWY3DPEHPK3PXP",
        otpauth_uri: "otpauth://totp/plugrelay:admin?secret=JBSWY3DPEHPK3PXP",
        expires_at: "2026-11-07T10:10:00Z",
      })),
    "POST /me/totp": ({ body }) => {
      if ((body as { code: string }).code !== "123456")
        return apiError(422, "invalid", "code: the code does not match; check the device clock and try the current code", {
          params: { count: 1 },
          fields: [{ path: "code", code: "totp_mismatch", message: "the code does not match; check the device clock and try the current code" }],
        });
      s.totp_enabled = true;
      s.recovery_codes_left = 10;
      return { recovery_codes: codes };
    },
    "DELETE /me/totp": () =>
      guard(() => {
        s.totp_enabled = false;
        return reply(204);
      }),
    "POST /me/password": () => guard(() => reply(204)),
    "PATCH /me/passkeys/{id}": ({ params, body }) => {
      const p = s.passkeys.find((x) => x.id === params.id)!;
      p.name = body.name;
      return undefined;
    },
    "DELETE /me/passkeys/{id}": ({ params }) =>
      guard(() => {
        s.passkeys = s.passkeys.filter((x) => x.id !== params.id);
        return reply(204);
      }),
    "POST /me/passkeys/options": () => ({
      challenge: "Y2hhbGxlbmdl",
      rp: { id: "localhost", name: "plugrelay" },
      user: { id: "dTE", name: "admin", displayName: "Admin" },
      pubKeyCredParams: [{ type: "public-key", alg: -7 }],
      timeout: 300000,
      attestation: "none",
      excludeCredentials: [],
      authenticatorSelection: { residentKey: "preferred", userVerification: "preferred" },
    }),
    "POST /me/passkeys": ({ body }) => {
      const p: Passkey = {
        id: "pk_new",
        name: (body as { name?: string }).name ?? "Passkey",
        created_at: "2026-11-07T10:00:00Z",
        last_used_at: null,
      };
      s.passkeys.push(p);
      return { passkey: p };
    },
  };
  return { s, routes, dropReauth: () => (reauthed = false) };
}

function stubPasskeys() {
  const buf = (x: string) => new TextEncoder().encode(x).buffer;
  const create = vi.fn(async () => ({
    id: "cred-1",
    rawId: buf("cred-1"),
    response: { clientDataJSON: buf("{}"), attestationObject: buf("att"), getTransports: () => ["internal"] },
  }));
  vi.stubGlobal("PublicKeyCredential", function PublicKeyCredential() {});
  Object.defineProperty(navigator, "credentials", { value: { create, get: vi.fn() }, configurable: true });
  return { create };
}

describe("SecurityPage", () => {
  it("says at a glance whether a second step protects the account", async () => {
    renderPage(<SecurityPage />, { locale: "zh-CN", routes: account().routes });
    expect(await screen.findByText("两步验证还没开启")).toBeInTheDocument();
    expect(screen.getByText("还没有通行密钥。")).toBeInTheDocument();
    expectPlainWords();
  });

  it("sets up the authenticator app with a QR code, then shows recovery codes once", async () => {
    const acct = account();
    const { user } = renderPage(<SecurityPage />, { routes: acct.routes, locale: "en" });
    await user.click(await screen.findByRole("button", { name: "Set up" }));
    expect(await screen.findByRole("img", { name: "QR code for your authenticator app" })).toBeInTheDocument();
    expect(screen.getByText("JBSWY3DPEHPK3PXP")).toBeInTheDocument();

    const code = screen.getByLabelText("Code from your authenticator app");
    await user.type(code, "000000");
    await user.click(screen.getByRole("button", { name: "Turn on" }));
    expect(await screen.findByText("The code doesn't match. Check the device clock and enter the current code.")).toBeInTheDocument();

    await user.clear(code);
    await user.type(code, "123456");
    await user.click(screen.getByRole("button", { name: "Turn on" }));
    const dialog = await screen.findByRole("dialog", { name: "Recovery codes" });
    expect(within(dialog).getAllByRole("listitem")).toHaveLength(10);
    await user.click(within(dialog).getByRole("button", { name: "I saved them" }));
    expect(await screen.findByRole("button", { name: "Turn off" })).toBeInTheDocument();
    expect(screen.getByText("10 left")).toBeInTheDocument();
    expect(screen.getAllByText("Two-step sign-in is on").length).toBeGreaterThan(0);
  });

  it("asks to confirm it is you before turning the app off", async () => {
    const acct = account({ totp_enabled: true, recovery_codes_left: 3 });
    acct.dropReauth();
    const { user } = renderPage(<SecurityPage />, { routes: acct.routes, locale: "en" });
    await user.click(await screen.findByRole("button", { name: "Turn off" }));
    await user.click(within(await screen.findByRole("alertdialog")).getByRole("button", { name: "Turn off" }));
    const reauth = await screen.findByRole("dialog", { name: "Confirm it's you" });
    await user.type(within(reauth).getByLabelText("Password or authenticator code"), "correct-horse-1");
    await user.click(within(reauth).getByRole("button", { name: "Confirm" }));
    await waitFor(() => expect(acct.s.totp_enabled).toBe(false));
    expect(await screen.findByRole("button", { name: "Set up" })).toBeInTheDocument();
  });

  it("changes the password after confirming it is you", async () => {
    const acct = account();
    acct.dropReauth();
    const { user, api } = renderPage(<SecurityPage />, { routes: acct.routes, locale: "en" });
    await user.click(await screen.findByRole("button", { name: "Change password" }));
    await user.type(screen.getByLabelText("New password"), "brand-new-pass-1");
    await user.type(screen.getByLabelText("Repeat it"), "brand-new-pass-2");
    expect(screen.getByText("The two passwords don't match.")).toBeInTheDocument();
    await user.clear(screen.getByLabelText("Repeat it"));
    await user.type(screen.getByLabelText("Repeat it"), "brand-new-pass-1");
    await user.click(screen.getByRole("button", { name: "Change password" }));
    const reauth = await screen.findByRole("dialog", { name: "Confirm it's you" });
    await user.type(within(reauth).getByLabelText("Password"), "correct-horse-1");
    await user.click(within(reauth).getByRole("button", { name: "Confirm" }));
    expect(await screen.findByText("Password changed")).toBeInTheDocument();
    expect(api.calls.filter((c) => c.path === "/me/password").at(-1)?.body).toEqual({
      password: "brand-new-pass-1",
    });
  });

  it("lists passkeys with dates, renames, adds and deletes them", async () => {
    const { create } = stubPasskeys();
    const acct = account({
      passkeys: [
        {
          id: "pk_1",
          name: "laptop",
          created_at: "2026-11-01T10:00:00Z",
          last_used_at: "2026-11-05T10:00:00Z",
        },
      ],
    });
    const { user } = renderPage(<SecurityPage />, { routes: acct.routes, locale: "en" });
    const list = await screen.findByRole("list", { name: "Passkeys" });
    expect(within(list).getByText("laptop")).toBeInTheDocument();
    expect(within(list).getByText(/last used/)).toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "Rename laptop" }));
    const name = within(list).getByLabelText("Passkey name");
    await user.clear(name);
    await user.type(name, "work laptop{Enter}");
    await waitFor(() => expect(acct.s.passkeys[0]!.name).toBe("work laptop"));

    await user.type(screen.getByPlaceholderText("e.g. Work laptop"), "phone");
    await user.click(screen.getByRole("button", { name: "Add a passkey" }));
    expect(await screen.findByText("Added phone")).toBeInTheDocument();
    expect(create).toHaveBeenCalledOnce();

    await user.click(await screen.findByRole("button", { name: "Delete work laptop" }));
    await user.click(within(await screen.findByRole("alertdialog")).getByRole("button", { name: "Delete" }));
    await waitFor(() => expect(acct.s.passkeys.map((p) => p.name)).toEqual(["phone"]));
  });

  it("explains when passkeys cannot be used here", async () => {
    renderPage(<SecurityPage />, { routes: account().routes, locale: "en" });
    expect(await screen.findByText("This browser does not support passkeys.")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Add a passkey" })).not.toBeInTheDocument();
  });
});

describe("SecurityPage in plain words (zh-CN)", () => {
  it("shows an account with two-step sign-in and passkeys, and the app setup, without raw ids", async () => {
    const passkey: Passkey = {
      id: "pk_1",
      name: "MacBook Touch ID",
      created_at: "2026-10-01T10:00:00Z",
      last_used_at: "2026-11-06T10:00:00Z",
      transports: ["internal"],
    };
    const { unmount } = renderPage(<SecurityPage />, {
      locale: "zh-CN",
      routes: account({ totp_enabled: true, recovery_codes_left: 3, passkeys: [passkey] }).routes,
    });
    expect(await screen.findByText("MacBook Touch ID")).toBeInTheDocument();
    expectPlainWords();
    unmount();
    const { user } = renderPage(<SecurityPage />, { locale: "zh-CN", routes: account().routes });
    await user.click(await screen.findByRole("button", { name: translate("zh-CN", "totp.setup") }));
    await screen.findByText("JBSWY3DPEHPK3PXP");
    expectPlainWords();
  });
});
