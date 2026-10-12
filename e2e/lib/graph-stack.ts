import { test as base, expect } from "@playwright/test";
import { graphStackFile, readStack, type Stack } from "./stack";

// The flow suite runs against the fake providers (free, offline) or, with
// E2E_PROVIDER=xai, against live xAI (make e2e-xai; spends API credit).
export const LIVE = process.env.E2E_PROVIDER === "xai";
export const PROVIDER = LIVE ? "xai" : "google";
export const SKIP_LIVE_REASON = "No XAI_API_KEY (env or examples/.env): the live xAI run is skipped";

export const test = base.extend<{ stack: Stack; _requireKey: void }>({
  _requireKey: [async ({}, use, info) => {
    info.skip(LIVE && !process.env.E2E_XAI_SECRET, SKIP_LIVE_REASON);
    await use();
  }, { auto: true }],
  stack: async ({}, use) => use(readStack(graphStackFile)),
});
export { expect };
export type { Stack };
