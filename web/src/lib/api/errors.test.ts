import { describe, expect, it } from "vitest";
import { ApiError } from "./errors";

const err = (body: ConstructorParameters<typeof ApiError>[1], locale: "en" | "zh-CN" = "zh-CN") => new ApiError(422, body, "422", locale);

describe("ApiError", () => {
  it("words a known code in the console's language, params and param values included", () => {
    expect(err({ error: { code: "admin_required", message: "admin role required" } }).message).toBe("需要管理员账号才能操作。");
    expect(err({ error: { code: "not_found", params: { object: "user" }, message: "user not found" } }).message).toBe("找不到该用户。");
    expect(err({ error: { code: "not_found", params: { object: "user" }, message: "user not found" } }, "en").message).toBe(
      "The user wasn't found.",
    );
  });

  it("falls back to the server's English for a code it does not know", () => {
    const e = err({ error: { code: "brand_new", params: { n: 1 }, message: "something new happened" } });
    expect(e.message).toBe("something new happened");
    expect(e.code).toBe("brand_new");
  });

  it("words each refused field, and the field itself when it is the only one", () => {
    const one = err({
      error: {
        code: "invalid",
        params: { count: 1 },
        message: "name: must be 1 to 64 characters",
        fields: [{ path: "name", code: "length_range", params: { min: 1, max: 64 }, message: "must be 1 to 64 characters" }],
      },
    });
    expect(one.message).toBe("须为 1–64 个字符。");
    expect(one.fields).toEqual({ name: "须为 1–64 个字符。" });

    const two = err({
      error: {
        code: "invalid",
        params: { count: 2 },
        message: "2 fields are invalid",
        fields: [
          { path: "proxy", code: "url_proxy", message: "want an http, https, socks5 or socks5h URL" },
          { path: "x", code: "unknown_code", message: "raw english" },
        ],
      },
    });
    expect(two.message).toBe("有 2 项需要修改。");
    expect(two.fields).toEqual({ proxy: "须为 http、https、socks5 或 socks5h 代理地址。", x: "raw english" });
  });
});
