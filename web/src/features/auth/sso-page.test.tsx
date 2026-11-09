import { screen, waitFor, within } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { apiError, adminMe, renderPage, type ApiRoutes } from "@/test/api";
import { translate } from "@/i18n";
import { expectPlainWords } from "@/test/plain-words";
import { SsoPage } from "./sso-page";
import type { SsoProvider } from "./sso-provider-drawer";
import type { components } from "@/lib/api/schema.gen";

const google = {
  id: "google",
  name: "Google",
  preset: "google",
  enabled: true,
  client_id: "123.apps.googleusercontent.com",
  auto_create: false,
  link_by_email: false,
  trust_email: false,
  has_client_secret: true,
  redirect_uri: "https://relay.example.com/api/v1/auth/sso/google/callback",
} satisfies SsoProvider;
const on = adminMe({ pages: ["sso.providers"] });

function routes() {
  const items: components["schemas"]["SsoProvider"][] = [google];
  const routes: ApiRoutes = {
    "GET /sso/providers": () => ({
      items,
      presets: ["google", "github", "keycloak"],
      redirect_base: "https://relay.example.com",
    }),
    "GET /groups": { items: [] },
    "POST /sso/providers": ({ body }) => {
      if (!body.id)
        return apiError(422, "invalid", "id: use 1-32 lowercase letters, digits or dashes", {
          params: { count: 1 },
          fields: [{ path: "id", code: "id_format", message: "use 1-32 lowercase letters, digits or dashes" }],
        });
      const p: components["schemas"]["SsoProvider"] = {
        ...body,
        has_client_secret: !!body.client_secret,
        redirect_uri: `https://relay.example.com/api/v1/auth/sso/${body.id}/callback`,
      };
      items.push(p);
      return p;
    },
  };
  return { items, routes };
}

describe("SsoPage", () => {
  it("says the feature is off instead of an empty page", async () => {
    renderPage(<SsoPage />, { locale: "zh-CN", routes: routes().routes });
    expect(await screen.findByText("这个功能还没开")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "去功能页打开" })).toHaveAttribute("href", "/features?id=sso");
  });

  it("lists providers with their redirect address, once titled", async () => {
    renderPage(<SsoPage />, { locale: "zh-CN", routes: routes().routes, me: on });
    expect(await screen.findByText("https://relay.example.com/api/v1/auth/sso/google/callback")).toBeInTheDocument();
    expect(screen.getAllByRole("heading", { level: 1 })).toHaveLength(1);
    expectPlainWords();
  });

  it("adds a GitHub provider in a drawer; the server's field errors land on the fields", async () => {
    const r = routes();
    const { user } = renderPage(<SsoPage />, { routes: r.routes, me: on, locale: "en" });
    await screen.findByText(google.redirect_uri);
    await user.click(screen.getByRole("button", { name: "Add provider" }));
    const drawer = await screen.findByRole("dialog");
    await user.click(within(drawer).getByRole("combobox", { name: "Provider" }));
    await user.click(await screen.findByRole("option", { name: "GitHub" }));
    expect(within(drawer).queryByLabelText("Issuer URL")).not.toBeInTheDocument();

    await user.click(within(drawer).getByRole("button", { name: "Save" }));
    expect(await within(drawer).findByText("Use 1–32 lowercase letters, digits or dashes.")).toBeInTheDocument();

    await user.type(within(drawer).getByLabelText("ID"), "gh");
    await user.type(within(drawer).getByLabelText("Client ID"), "Iv1.abc");
    await user.type(within(drawer).getByLabelText("Client secret"), "s3cret");
    await user.click(within(drawer).getByRole("switch", { name: "Create users on first sign-in" }));
    expect(within(drawer).getByRole("combobox", { name: "Role for new users" })).toBeInTheDocument();
    await user.click(within(drawer).getByRole("button", { name: "Save" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    expect(await screen.findByText("https://relay.example.com/api/v1/auth/sso/gh/callback")).toBeInTheDocument();
    expect(r.items.at(-1)).toMatchObject({
      id: "gh",
      preset: "github",
      client_id: "Iv1.abc",
      auto_create: true,
      client_secret: "s3cret",
    });
  });

  it("asks for the issuer of a Keycloak provider", async () => {
    const { user } = renderPage(<SsoPage />, { routes: routes().routes, me: on, locale: "en" });
    await screen.findByText(google.redirect_uri);
    await user.click(screen.getByRole("button", { name: "Add provider" }));
    const drawer = await screen.findByRole("dialog");
    await user.click(within(drawer).getByRole("combobox", { name: "Provider" }));
    await user.click(await screen.findByRole("option", { name: "Keycloak" }));
    expect(within(drawer).getByLabelText("Issuer URL")).toBeInTheDocument();
  });
});

describe("SsoPage drawers in plain words (zh-CN)", () => {
  it("shows the new and the edit drawer without raw ids or keys", async () => {
    const { user } = renderPage(<SsoPage />, { routes: routes().routes, me: on, locale: "zh-CN" });
    await screen.findByText(google.redirect_uri);
    await user.click(screen.getByRole("button", { name: translate("zh-CN", "sso.add") }));
    expectPlainWords(await screen.findByRole("dialog"));
    await user.click(within(screen.getByRole("dialog")).getByRole("button", { name: translate("zh-CN", "common.close") }));
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    await user.click(screen.getByRole("button", { name: translate("zh-CN", "common.edit") }));
    expectPlainWords(await screen.findByRole("dialog"));
  });
});
