import { describe, expect, it } from "vitest";
import { formatMoney, lineAmount, moneyInputValue, parseMoneyInput } from "./money";

const cny = (micro: number) => ({ micro, currency: "CNY" });

describe("formatMoney", () => {
  it("keeps small amounts readable", () => {
    expect(formatMoney(cny(197_000), "en")).toBe("¥0.197");
    expect(formatMoney(cny(4_200), "en")).toBe("¥0.0042");
    expect(formatMoney(cny(31_000), "en")).toBe("¥0.031");
  });

  it("uses two decimals from one unit up, with grouping", () => {
    expect(formatMoney(cny(128_400_000), "en")).toBe("¥128.40");
    expect(formatMoney({ micro: 1_204_500_000, currency: "USD" }, "en")).toBe("$1,204.50");
  });

  it("shows zero without decimals and follows the locale", () => {
    expect(formatMoney(cny(0), "en")).toBe("¥0");
    expect(formatMoney(cny(36_200_000), "zh-CN")).toBe("¥36.20");
  });

  it("compacts large amounts on request only", () => {
    expect(formatMoney({ micro: 25_300_000_000, currency: "USD" }, "en", { compact: true })).toBe("$25.3K");
    expect(formatMoney({ micro: 25_300_000_000, currency: "USD" }, "en")).toBe("$25,300.00");
  });
});

describe("lineAmount", () => {
  it("multiplies tokens by a per-million price in micro-units", () => {
    // 38,400 cached tokens × ¥2.16 / M = ¥0.082944
    expect(lineAmount(38_400, 2_160_000)).toBe(82_944);
    expect(lineAmount(1, 1)).toBe(0);
  });
});

describe("money input", () => {
  it("parses what people type into micro-units", () => {
    expect(parseMoneyInput("12.5")).toBe(12_500_000);
    expect(parseMoneyInput("¥1,000")).toBe(1_000_000_000);
    expect(parseMoneyInput("")).toBeNull();
    expect(parseMoneyInput("abc")).toBeNull();
  });

  it("reads the decimal comma of locales that use one", () => {
    expect(parseMoneyInput("12,5", "de")).toBe(12_500_000);
    expect(parseMoneyInput("1.234,50 €", "de")).toBe(1_234_500_000);
    expect(parseMoneyInput("12.5", "de")).toBe(12_500_000);
    expect(parseMoneyInput("1,234.50", "fr")).toBe(1_234_500_000);
    expect(parseMoneyInput("12,5", "en")).toBe(125_000_000);
  });

  it("round-trips through the input value", () => {
    expect(parseMoneyInput(moneyInputValue(200_000_000))).toBe(200_000_000);
    expect(moneyInputValue(undefined)).toBe("");
  });
});
