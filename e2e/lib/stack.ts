import { test as base, expect } from "@playwright/test";
import fs from "node:fs";
import path from "node:path";

export type Stack = {
  repo: string;
  publicUrl: string; // http://127.0.0.1:<public>  (voice WS at ws://.../ws)
  adminUrl: string; // /ready, /health, /api/v1 (management)
  uiUrl: string; // admin UI (Vite)
  customClientUrl: string; // examples/rtvi_client
  managementKey: string;
  tokenSecret: string;
  devDir: string; // throwaway agent store + credential key
  tmpDir: string;
  logDir: string;
  pids: Record<string, number>;
};

export const stackFile = path.resolve(__dirname, "../.stack.json");
// The graph suite (playwright.graph.config.ts) has its own stack file, so it can coexist with the paid suite.
export const graphStackFile = path.resolve(__dirname, "../.stack-graph.json");
export const readStack = (file: string = stackFile): Stack => JSON.parse(fs.readFileSync(file, "utf8"));

export const SKIP_REASON = "No Google credential (GOOGLE_APPLICATION_CREDENTIALS service-account file or GOOGLE_API_KEY in examples/.env): e2e suite skipped";

// Every spec imports `test` from here: skips cleanly without GOOGLE_API_KEY.
export const test = base.extend<{ stack: Stack; _requireKey: void }>({
  _requireKey: [async ({}, use, info) => {
    info.skip(!process.env.E2E_GOOGLE_SECRET, SKIP_REASON);
    await use();
  }, { auto: true }],
  stack: async ({}, use) => use(readStack()),
});
export { expect };
