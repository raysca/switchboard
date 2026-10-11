import { defineConfig, devices } from "@playwright/test";
import path from "node:path";

// Browser e2e against the FAKE providers: free, offline, deterministic. It
// proves the graph designer, the stored graph agent, the live call and the
// handoff work together (make e2e-fake). The paid suite is playwright.config.ts.
export const fixture = (name: string) => path.resolve(__dirname, "fixtures", name);

export default defineConfig({
  testDir: "tests-fake",
  outputDir: "test-results-fake",
  globalSetup: "./global-setup-fake.ts",
  globalTeardown: "./global-teardown-fake.ts",
  workers: 1,
  fullyParallel: false,
  retries: 0,
  timeout: 90_000,
  expect: { timeout: 15_000 },
  reporter: [["list"], ["html", { outputFolder: "playwright-report-fake", open: "never" }]],
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
