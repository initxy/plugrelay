import { describe, expect, it } from "vitest";
import { csvRow } from "./parts";

describe("csvRow", () => {
  it("quotes separators and keeps formulas from running", () => {
    expect(csvRow(["a,b", 'say "hi"', "=1+1", "@SUM(A1)", "+x", "-x", "\tx", "-3", -2, "a=b"])).toBe(
      `"a,b","say ""hi""",'=1+1,'@SUM(A1),'+x,'-x,'\tx,-3,-2,a=b`,
    );
    expect(csvRow(["\rx"])).toBe(`"'\rx"`);
  });
});
