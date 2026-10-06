import { defineConfig, devices } from "@playwright/test";
import fs from "node:fs";
import path from "node:path";

// Provider secrets for specs, from examples/.env (never printed). Real env wins.
// E2E_GOOGLE_SECRET is what the test stores as the Google credential: the
// service-account JSON when GOOGLE_APPLICATION_CREDENTIALS points at one (needed
// for streaming Speech-to-Text v2), otherwise GOOGLE_API_KEY.
const examplesDir = path.resolve(__dirname, "../examples");
const envFile = path.join(examplesDir, ".env");
if (fs.existsSync(envFile)) {
  for (const line of fs.readFileSync(envFile, "utf8").split("\n")) {
    const m = line.match(/^\s*(GOOGLE_API_KEY|XAI_API_KEY|GOOGLE_APPLICATION_CREDENTIALS)\s*=\s*(.*?)\s*$/);
    if (m && !process.env[m[1]]) process.env[m[1]] = m[2].replace(/^["']|["']$/g, "");
  }
}
// The LLM stage uses the Gemini API key: service accounts often lack Vertex AI
// access (aiplatform.endpoints.predict), while Speech/TTS accept them.
if (!process.env.E2E_GOOGLE_LLM_SECRET && process.env.GOOGLE_API_KEY) {
  process.env.E2E_GOOGLE_LLM_SECRET = process.env.GOOGLE_API_KEY;
}
if (!process.env.E2E_GOOGLE_SECRET) {
  const sa = process.env.GOOGLE_APPLICATION_CREDENTIALS;
  const saPath = sa ? (path.isAbsolute(sa) ? sa : path.join(examplesDir, sa)) : "";
  if (saPath && fs.existsSync(saPath)) {
    process.env.E2E_GOOGLE_SECRET = JSON.stringify(JSON.parse(fs.readFileSync(saPath, "utf8")));
  } else if (process.env.GOOGLE_API_KEY) {
    process.env.E2E_GOOGLE_SECRET = process.env.GOOGLE_API_KEY;
  }
}

// The fake microphone WAV.
export const fixture = (name: string) => path.resolve(__dirname, "fixtures", name);

export default defineConfig({
  testDir: "tests",
  outputDir: "test-results",
  globalSetup: "./global-setup.ts",
  globalTeardown: "./global-teardown.ts",
  workers: 1,
  fullyParallel: false,
  retries: 0,
  timeout: 180_000,
  expect: { timeout: 30_000 },
  reporter: [["list"], ["html", { outputFolder: "playwright-report", open: "never" }]],
  use: {
    ...devices["Desktop Chrome"],
    headless: true,
    screenshot: "on",
    video: "retain-on-failure",
    trace: "retain-on-failure",
    permissions: ["microphone"],
    launchOptions: {
      args: [
        "--use-fake-ui-for-media-stream",
        "--use-fake-device-for-media-stream",
        `--use-file-for-fake-audio-capture=${fixture("voice-debug.wav")}`,
      ],
    },
  },
  projects: [{ name: "chromium" }],
});
