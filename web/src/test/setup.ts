import "@testing-library/jest-dom/vitest";

// Node's Request rejects relative URLs; browsers resolve them against the page. The API client
// builds requests from "/api/v1/…" and grabs globalThis.Request on import, so patch it first.
const NodeRequest = globalThis.Request;
globalThis.Request = class extends NodeRequest {
  constructor(input: RequestInfo | URL, init?: RequestInit) {
    super(typeof input === "string" ? new URL(input, location.origin) : input, init);
  }
};

// jsdom only logs "not implemented"; the router calls it on every navigation.
window.scrollTo = () => {};

// jsdom lacks what Radix popovers, selects and switches touch.
globalThis.ResizeObserver ??= class {
  observe() {}
  unobserve() {}
  disconnect() {}
};
Element.prototype.scrollIntoView ??= () => {};
Element.prototype.hasPointerCapture ??= () => false;
Element.prototype.releasePointerCapture ??= () => {};
