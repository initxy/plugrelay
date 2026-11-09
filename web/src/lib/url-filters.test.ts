import { describe, expect, it } from "vitest";
import { cleanSearch, countActiveFilters, parseLogFilters, pickEnum, pickFlag, pickString, serializeLogFilters } from "./url-filters";

describe("log filters ↔ URL", () => {
  it("keeps valid filters and drops junk from the URL", () => {
    const f = parseLogFilters({
      model: "gpt-5.5",
      status: "error",
      tier: "convert",
      cause: "rate_limit",
      retried: "true",
      key_id: "",
      upstream_id: "  openai ",
      bogus: "x",
      tier2: "patch",
    });
    expect(f).toEqual({
      model: "gpt-5.5",
      status: "error",
      tier: "convert",
      cause: "rate_limit",
      retried: true,
      upstream_id: "openai",
    });
  });

  it("rejects values outside the enums", () => {
    expect(parseLogFilters({ status: "maybe", tier: "teleport", cause: "gremlins" })).toEqual({});
  });

  it("serializes sorted, without empty values, and round-trips", () => {
    const f = { q: "req_8f", model: "claude-sonnet-4-5", status: "ok" as const, retried: false, key_id: "" };
    const s = serializeLogFilters(f);
    expect(Object.keys(s)).toEqual(["model", "q", "status"]);
    expect(parseLogFilters(s)).toEqual({ q: "req_8f", model: "claude-sonnet-4-5", status: "ok" });
    expect(new URLSearchParams(s).toString()).toBe("model=claude-sonnet-4-5&q=req_8f&status=ok");
  });

  it("counts filters without the time window", () => {
    expect(countActiveFilters({ from: "2026-11-01T00:00:00Z", model: "x", retried: true })).toBe(2);
  });
});

describe("generic search params", () => {
  it("drops empty values so a cleared filter leaves the URL", () => {
    expect(cleanSearch({ a: "x", b: "", c: undefined, d: false, e: 0, f: true, g: null, h: NaN })).toEqual({ a: "x", e: 0, f: true });
  });

  it("accepts only valid values from the URL", () => {
    expect(pickEnum("ok", ["ok", "bad"] as const)).toBe("ok");
    expect(pickEnum("nope", ["ok", "bad"] as const)).toBeUndefined();
    expect(pickString("  a ")).toBe("a");
    expect(pickString("   ")).toBeUndefined();
    expect(pickString(3)).toBeUndefined();
    expect([true, "true", "1", 1, "yes", false].map(pickFlag)).toEqual([true, true, true, true, undefined, undefined]);
  });
});
