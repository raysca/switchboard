import { test as base, expect } from "@playwright/test";
import { fakeStackFile, readStack, type Stack } from "./stack";

// The fake-providers suite needs no secrets: every spec gets the stack that
// global-setup-fake.ts started (server built with -tags fakeproviders).
export const test = base.extend<{ stack: Stack }>({
  stack: async ({}, use) => use(readStack(fakeStackFile)),
});
export { expect };
export type { Stack };
