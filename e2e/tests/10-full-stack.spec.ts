import type { Page } from "@playwright/test";
import { test, expect, type Stack } from "../lib/stack";
import { step } from "../lib/evidence";

// One end-to-end run with real providers: the server reads the Google secrets from
// its environment, the admin UI creates an agent, the admin debug client calls it by voice and text,
// the custom client calls it, and the admin UI deletes it. Every step leaves a
// numbered screenshot in e2e/evidence/.
test.describe.configure({ mode: "serial" });
// No traces here (video and the error context remain): the run is paid, and the
// report must never hold anything near a secret.
test.use({ trace: "off" });

const AGENT = "E2E Concierge";
// No leading \b: a transcript entry's text runs its label into the reply
// ("AssistantFour."). The lookbehind still rejects "14"; the trailing \b "fourteen".
const FOUR = /(?<!\d)(4|four)\b/i;
const PARIS = /paris/i;
// Google STT normalises numbers: "two plus two" arrives as "2 + 2".
const TWO_PLUS_TWO = /(two plus two|2\s*\+\s*2)/i;

let agentId = "";

async function connectAdmin(page: Page, stack: Stack) {
  await page.goto(`${stack.uiUrl}/dashboard`);
  await page.getByLabel("Management key").fill(stack.managementKey);
  await page.getByRole("button", { name: "Connect", exact: true }).click();
  await expect(page.getByText("Key in memory")).toBeVisible();
}

// Opens the agent's Test call panel from wherever the editor is (publishing already lands in the editor).
async function openAgentTest(page: Page) {
  const heading = page.getByRole("heading", { name: AGENT, level: 1 });
  if (!(await heading.isVisible())) {
    await page.getByRole("link", { name: "Agents", exact: true }).first().click();
    await page.getByRole("link", { name: AGENT, exact: true }).click();
    await expect(heading).toBeVisible();
  }
  if (!(await page.getByTestId("agent-readiness").isVisible())) await page.getByRole("button", { name: "Test call" }).click();
}

test("admin UI: Google is detected from the server environment and a ready agent is created", async ({ page, stack }) => {
  await connectAdmin(page, stack);
  await step(page, "admin connected");

  // The secrets reach the server process as environment variables (global-setup):
  // they never pass through the API or the browser. The providers page shows only status.
  const secrets = [process.env.E2E_GOOGLE_SECRET!, process.env.E2E_GOOGLE_LLM_SECRET].filter((s): s is string => !!s);
  await page.getByRole("link", { name: "Providers", exact: true }).first().click();
  await expect(page.getByRole("row").filter({ hasText: "Google" })).toContainText("Environment variable");
  const html = await page.content();
  const echoed = secrets.some((secret) => html.includes(secret.trim().slice(0, 24)));
  expect(echoed, "a provider secret is echoed in the page").toBe(false);
  await step(page, "google detected (no secret shown)");

  await page.getByRole("link", { name: "Agents", exact: true }).first().click();
  await page.getByRole("button", { name: "New agent" }).click();
  await page.getByLabel("Name").fill(AGENT);
  await page.getByRole("button", { name: "Create and open editor" }).click();
  await page.getByRole("button", { name: "Add first step" }).click();
  await page.getByLabel("Step name").fill("Assistant");
  await page.getByLabel("Instructions").fill("You are a concise voice assistant. Answer in one short sentence.");
  await page.getByLabel("LLM provider").selectOption("google");
  await page.getByRole("combobox", { name: "LLM model", exact: true }).selectOption("gemini-2.5-flash");
  await page.getByLabel("TTS provider").selectOption("google");
  await page.getByRole("combobox", { name: "Voice", exact: true }).selectOption("en-US-Chirp3-HD-Aoede");
  await page.getByRole("button", { name: "Agent settings" }).click();
  await page.getByLabel("STT provider").selectOption("google");
  await page.getByRole("combobox", { name: "STT model", exact: true }).selectOption("chirp_2");
  await page.getByLabel("STT language").selectOption("en-US");
  await step(page, "agent filled");
  await page.getByRole("button", { name: "Publish", exact: true }).click();
  await page.getByRole("button", { name: "Publish v1" }).click();
  await expect(page.getByText(/Published v1/)).toBeVisible();

  await openAgentTest(page);
  await expect(page.getByTestId("agent-readiness")).toHaveText(/^ready/);
  agentId = await agentIdFromApi(stack);
  expect(agentId).not.toBe("");
  const saved = await api<{ data: { current_version: { config: Record<string, unknown> } } }>(stack, `/agents/${agentId}`);
  // A named step is published as an agent.
  const cfg = (saved.data.current_version.config as unknown as { steps: { config: Record<string, Record<string, unknown>> }[] }).steps[0].config;
  expect([cfg.stt.provider, cfg.stt.model, cfg.llm.model, cfg.tts.voice]).toEqual(["google", "chirp_2", "gemini-2.5-flash", "en-US-Chirp3-HD-Aoede"]);
  expect(JSON.stringify(cfg)).not.toContain("credential");
  await step(page, "agent ready");
});

test("admin debug client: voice call and text call", async ({ page, stack }) => {
  await connectAdmin(page, stack);
  await openAgentTest(page);

  // Voice: the fake microphone plays "What is two plus two?".
  await page.getByTestId("start-voice-test-btn").click();
  await expect(page.getByTestId("connection-state")).toHaveText(/ready/i, { timeout: 30_000 });
  await step(page, "debug client connected");
  await expect(page.getByTestId("transcript-user").first()).toContainText(TWO_PLUS_TWO, { timeout: 60_000 });
  await expect(botReply(page)).toContainText(FOUR, { timeout: 60_000 });
  await step(page, "debug client voice turn answered");

  // Text: mute the looping microphone so it cannot interrupt the typed turn.
  await page.getByTestId("mute-btn").click();
  await page.getByRole("textbox", { name: "Message" }).fill("What is the capital of France?");
  await page.getByTestId("send-text-btn").click();
  await expect(page.getByTestId("transcript-user").filter({ hasText: /capital of France/i })).toBeVisible({ timeout: 30_000 });
  await expect(botReply(page, PARIS)).toBeVisible({ timeout: 60_000 });
  await step(page, "debug client text turn answered");

  await page.getByTestId("disconnect-btn").click();
  await expect(page.getByTestId("connection-state")).toHaveText(/disconnected/i);
  await step(page, "debug client disconnected");
});

test("custom client: calls the agent by agent_id", async ({ page, stack }) => {
  await page.goto(stack.customClientUrl);
  await page.getByLabel("Admin URL").fill(stack.adminUrl);
  await page.getByLabel("Management key").fill(stack.managementKey);
  await page.getByLabel("Agent ID").fill(agentId);
  await page.getByLabel("Server WebSocket URL").fill(stack.publicUrl.replace(/^http/, "ws") + "/ws");
  await step(page, "custom client configured");

  await page.getByRole("button", { name: "Connect", exact: true }).click();
  await expect(page.getByTestId("connection-status")).toHaveText(/^connected/i, { timeout: 30_000 });
  await expect(page.getByTestId("transcript-user").first()).toContainText(TWO_PLUS_TWO, { timeout: 60_000 });
  await expect(page.getByTestId("transcript-bot").filter({ hasText: FOUR }).first()).toBeVisible({ timeout: 60_000 });
  await step(page, "custom client voice turn answered");

  await page.getByRole("textbox", { name: "Message" }).fill("What is the capital of France?");
  await page.getByRole("button", { name: "Send text" }).click();
  await expect(page.getByTestId("transcript-bot").filter({ hasText: PARIS }).first()).toBeVisible({ timeout: 60_000 });
  await step(page, "custom client text turn answered");
  await page.getByRole("button", { name: "Disconnect" }).click();
});

test("admin UI: archive the agent", async ({ page, request, stack }) => {
  await connectAdmin(page, stack);
  await page.getByRole("link", { name: "Agents", exact: true }).first().click();
  await page.getByRole("button", { name: `Archive ${AGENT}` }).click();
  await step(page, "archive confirmation");
  await page.getByRole("button", { name: "Archive agent" }).click();
  await expect(page.getByRole("link", { name: AGENT, exact: true })).toHaveCount(0);
  await step(page, "agent archived");

  // A deleted agent can no longer take calls.
  const token = await request.post(`${stack.adminUrl}/api/v1/voice-tokens`, {
    headers: { Authorization: `Bearer ${stack.managementKey}` },
    data: { principal: "e2e", agent_id: agentId },
  });
  expect(token.status()).toBe(409);
});

// The bot's reply is final in the transcript, or still streaming.
function botReply(page: Page, text: RegExp = FOUR) {
  return page
    .locator('[data-testid="transcript-bot"], [data-testid="bot-streaming-text"]')
    .filter({ hasText: text })
    .first();
}

async function api<T>(stack: Stack, route: string): Promise<T> {
  const res = await fetch(`${stack.adminUrl}/api/v1${route}`, {
    headers: { Authorization: `Bearer ${stack.managementKey}` },
  });
  expect(res.ok, `GET ${route}`).toBeTruthy();
  return (await res.json()) as T;
}

async function agentIdFromApi(stack: Stack): Promise<string> {
  const body = await api<{ data: { id: string; name: string }[] }>(stack, "/agents");
  return body.data.find((a) => a.name === AGENT)?.id ?? "";
}
