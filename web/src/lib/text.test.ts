import { describe, expect, it } from "vitest";
import { parseHeaders, splitList } from "./text";

describe("parseHeaders", () => {
  it("reads Name: value lines and skips the rest", () => {
    expect(parseHeaders("A: 1\nnot a header\nB:x:y\n: nameless\nC:")).toEqual({ A: "1", B: "x:y", C: "" });
  });
});

describe("splitList", () => {
  it("splits on lines and commas and drops blanks", () => {
    expect(splitList(" a.com,b.com\n\n c.com ,")).toEqual(["a.com", "b.com", "c.com"]);
  });
});
