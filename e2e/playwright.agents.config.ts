import { defineConfig, devices } from "@playwright/test";
import fs from "node:fs";
import path from "node:path";

// Browser e2e for agents. Default: the FAKE providers (free, offline,
// deterministic; make e2e-fake). E2E_PROVIDER=xai: live xAI (make e2e-xai, spends
// API credit). It proves the agent designer, the stored agent with handoffs, the live call
// and the handoff work together. The paid Google suite is playwright.config.ts.
// The xAI key comes from the environment or examples/.env and is only ever passed
// to the server process's environment: never to the API or the browser.
const live = process.env.E2E_PROVIDER === "xai";
if (live && !process.env.E2E_XAI_SECRET) {
  const envFile = path.resolve(__dirname, "../examples/.env");
  const fromEnv = process.env.XAI_API_KEY;
  if (fromEnv) process.env.E2E_XAI_SECRET = fromEnv;
  else if (fs.existsSync(envFile)) {
    for (const line of fs.readFileSync(envFile, "utf8").split("\n")) {
      const m = line.match(/^\s*XAI_API_KEY\s*=\s*(.*?)\s*$/);
      if (m && m[1]) process.env.E2E_XAI_SECRET = m[1].replace(/^["']|["']$/g, "");
    }
  }
}

export const fixture = (name: string) => path.resolve(__dirname, "fixtures", name);

export default defineConfig({
  testDir: "tests-agents",
  outputDir: "test-results-agents",
  globalSetup: "./global-setup-agents.ts",
  globalTeardown: "./global-teardown-agents.ts",
  workers: 1,
  fullyParallel: false,
  retries: 0,
  timeout: live ? 240_000 : 90_000,
  expect: { timeout: live ? 60_000 : 15_000 },
  reporter: [["list"], ["html", { outputFolder: "playwright-report-agents", open: "never" }]],
  use: {
    ...devices["Desktop Chrome"],
    headless: true,
    screenshot: "on",
    video: "retain-on-failure",
    // Live: no traces. The run is paid, and a trace would snapshot request details.
    trace: live ? "off" : "retain-on-failure",
    permissions: ["microphone"],
    launchOptions: {
      args: ["--use-fake-ui-for-media-stream", "--use-fake-device-for-media-stream", `--use-file-for-fake-audio-capture=${fixture("voice-debug.wav")}`],
    },
  },
  projects: [{ name: "chromium" }],
});
