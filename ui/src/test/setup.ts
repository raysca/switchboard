import "@testing-library/jest-dom/vitest";
import { afterEach } from "vitest";
import { cleanup } from "@testing-library/react";

afterEach(cleanup);

// React Flow measures its container; jsdom has no layout engine.
class ResizeObserverStub {
  observe() {}
  unobserve() {}
  disconnect() {}
}
globalThis.ResizeObserver ??= ResizeObserverStub as unknown as typeof ResizeObserver;
if (!globalThis.DOMMatrixReadOnly) {
  globalThis.DOMMatrixReadOnly = class {
    m22 = 1;
    constructor(_transform?: string) {}
  } as unknown as typeof DOMMatrixReadOnly;
}

// jsdom has no modal dialogs: open and close are enough for unit tests; the browser suite checks the rest.
if (typeof HTMLDialogElement !== "undefined" && !HTMLDialogElement.prototype.showModal) {
  HTMLDialogElement.prototype.showModal = function (this: HTMLDialogElement) {
    this.open = true;
  };
  HTMLDialogElement.prototype.close = function (this: HTMLDialogElement) {
    this.open = false;
  };
}
