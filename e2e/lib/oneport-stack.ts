import { test as base, expect } from "@playwright/test";
import { oneportStackFile, readStack, type Stack } from "./stack";

export const test = base.extend<{ stack: Stack }>({
  stack: async ({}, use) => use(readStack(oneportStackFile)),
});
export { expect };
export type { Stack };
