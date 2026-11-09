import { render } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { findRawTerms } from "./plain-words";

const terms = (ui: React.ReactElement) => findRawTerms(render(ui).container);

describe("findRawTerms", () => {
  it("catches config keys, module ids and snake_case ids in text and labels", () => {
    expect(terms(<p>设置 site.public_base_url 后生效</p>)).toEqual([expect.stringContaining('"site.public_base_url"')]);
    expect(terms(<p>middleware.ratelimit</p>)).toHaveLength(1);
    expect(terms(<span>priority_weight</span>)).toHaveLength(1);
    expect(terms(<button aria-label="toggle needs_approval" />)).toEqual([expect.stringContaining("aria-label")]);
  });

  it("leaves paths, URLs, emails, host and file names, numbers and code alone", () => {
    expect(
      terms(
        <div>
          <p>回调指向 /api/plugins/admin.payment/webhook/stripe，或 https://relay.example.com/v1</p>
          <p>admin@demo.local · example.com · relay.yaml · v1.2.0 · 12.50 · e.g. this</p>
          <p>以 sk_live_ 开头</p>
          <code>security.captcha_secret</code>
          <span data-raw>user_typed.value</span>
        </div>,
      ),
    ).toEqual([]);
  });
});
