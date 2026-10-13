import { defineConfig, devices } from "@playwright/test";
import path from "node:path";

// Browser e2e of the one-port server (make e2e-oneport): the embedded UI, the management API and
// the voice socket on one public origin, fake providers only (free, offline).
const fixture = (name: string) => path.resolve(__dirname, "fixtures", name);

export default defineConfig({
  testDir: "tests-oneport",
  outputDir: "test-results-oneport",
  globalSetup: "./global-setup-oneport.ts",
  globalTeardown: "./global-teardown-oneport.ts",
  workers: 1,
  fullyParallel: false,
  retries: 0,
  timeout: 90_000,
  expect: { timeout: 15_000 },
  reporter: [["list"], ["html", { outputFolder: "playwright-report-oneport", open: "never" }]],
  use: {
    ...devices["Desktop Chrome"],
    headless: true,
    screenshot: "on",
    video: "retain-on-failure",
    trace: "retain-on-failure",
    permissions: ["microphone"],
    launchOptions: {
      args: ["--use-fake-ui-for-media-stream", "--use-fake-device-for-media-stream", `--use-file-for-fake-audio-capture=${fixture("voice-debug.wav")}`],
    },
  },
  projects: [{ name: "chromium" }],
});
