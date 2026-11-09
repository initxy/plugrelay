import { screen } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import type * as Router from "@tanstack/react-router";
import { LoginPage } from "@/features/auth/login-page";
import { renderPage } from "@/test/api";

// Radix Select calls pointer-capture and scroll APIs jsdom lacks.
Element.prototype.hasPointerCapture ??= () => false;
Element.prototype.releasePointerCapture ??= () => {};
Element.prototype.scrollIntoView ??= () => {};

vi.mock("@tanstack/react-router", async (importOriginal) => ({
  ...(await importOriginal<typeof Router>()),
  useSearch: () => ({}),
}));

beforeEach(() => {
  localStorage.clear();
});

describe("language switcher", () => {
  it("re-renders the page in the chosen language and remembers it", async () => {
    const { user } = renderPage(<LoginPage />, {
      path: "/login",
      me: null,
      routes: { "GET /auth/providers": { password_login: true, providers: [], site: { name: "plugrelay" } } },
    });
    expect(await screen.findByLabelText("Password")).toBeVisible();

    await user.click(screen.getByRole("combobox", { name: "Language" }));
    await user.click(await screen.findByRole("option", { name: "简体中文" }));

    expect(await screen.findByLabelText("密码")).toBeVisible();
    expect(screen.queryByLabelText("Password")).toBeNull();
    expect(document.documentElement.lang).toBe("zh-CN");
    expect(localStorage.getItem("pr.locale")).toBe("zh-CN");

    await user.click(screen.getByRole("combobox", { name: "语言" }));
    await user.click(await screen.findByRole("option", { name: "English" }));
    expect(await screen.findByLabelText("Password")).toBeVisible();
  });
});
