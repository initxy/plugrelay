import { describe, expect, it } from "vitest";
import { parseDuration, splitDuration, toGoDuration } from "./duration";

describe("durations", () => {
  it("reads Go durations", () => {
    expect(parseDuration("15m")).toBe(900);
    expect(parseDuration("1h30m")).toBe(5400);
    expect(parseDuration("90s")).toBe(90);
    expect(parseDuration("1500ms")).toBe(1.5);
    expect(parseDuration("0")).toBe(0);
    expect(parseDuration("15 minutes")).toBeNull();
    expect(parseDuration("")).toBeNull();
  });

  it("shows a duration in its largest whole unit and writes it back", () => {
    expect(splitDuration(5400)).toEqual({ amount: 90, unit: "m" });
    expect(splitDuration(7200)).toEqual({ amount: 2, unit: "h" });
    expect(splitDuration(172800)).toEqual({ amount: 2, unit: "d" });
    expect(splitDuration(45)).toEqual({ amount: 45, unit: "s" });
    expect(toGoDuration(2, "d")).toBe("48h");
    expect(toGoDuration(15, "m")).toBe("15m");
    expect(parseDuration(toGoDuration(3, "d"))).toBe(259200);
  });
});
