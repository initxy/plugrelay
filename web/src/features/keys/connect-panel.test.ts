import { describe, expect, it } from "vitest";
import { ccSwitchLink, fillKey } from "./connect-panel";

describe("fillKey", () => {
  const snippet = "export ANTHROPIC_AUTH_TOKEN={{api_key}}\n# {{api_key}} again";
  it("fills the secret everywhere when it is known (right after creation)", () => {
    expect(fillKey(snippet, "{{api_key}}", "pr_abc", "<your-key>")).toBe("export ANTHROPIC_AUTH_TOKEN=pr_abc\n# pr_abc again");
  });
  it("uses a visible marker when the secret is gone", () => {
    expect(fillKey(snippet, "{{api_key}}", undefined, "<your-key>")).toContain("TOKEN=<your-key>");
  });
});

describe("ccSwitchLink", () => {
  const url = "ccswitch://v1/import?resource=provider&app=claude&name=Acme&endpoint=https%3A%2F%2Frelay.example.com&apiKey={{api_key}}&apiFormat=anthropic";
  it("puts the URL-escaped secret in", () => {
    const link = ccSwitchLink(url, "{{api_key}}", "pr_a+b/c")!;
    expect(new URLSearchParams(link.split("?")[1]).get("apiKey")).toBe("pr_a+b/c");
    expect(link).not.toContain("{{api_key}}");
  });
  it("offers no link without the secret", () => {
    expect(ccSwitchLink(url, "{{api_key}}", undefined)).toBeUndefined();
  });
});
