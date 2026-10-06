import type { Page } from "@playwright/test";
import { test, expect, type Stack } from "../lib/stack";
import { step } from "../lib/evidence";

// One end-to-end run with real providers: the admin UI sets up a provider
// credential and an agent, the admin debug client calls it by voice and text,
// the custom client calls it, and the admin UI deletes it. Every step leaves a
// numbered screenshot in e2e/evidence/.
test.describe.configure({ mode: "serial" });
// Secrets must not reach the HTML report: Playwright titles fill steps with the
// value and traces snapshot input values. So no traces here (video and the
// error context remain), and secrets are typed from inside the page.
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

// Opens the agent's detail view from wherever the Agents page is (saving an
// agent already lands on its detail view).
async function openAgentDetail(page: Page) {
  const heading = page.getByRole("heading", { name: AGENT });
  if (await heading.isVisible()) return;
  await page.getByRole("link", { name: "Agents", exact: true }).first().click();
  const back = page.getByRole("button", { name: "Back to list" });
  if (await back.isVisible()) await back.click();
  await page.getByRole("row").filter({ hasText: AGENT }).getByRole("button", { name: "View detail" }).click();
  await expect(heading).toBeVisible();
}

test("admin UI: add a Google credential and create a ready agent", async ({ page, stack }) => {
  await connectAdmin(page, stack);
  await step(page, "admin connected");

  // Speech (STT/TTS) uses the service account; the LLM uses the Gemini API key.
  // When they are the same secret, one credential backs every stage.
  const speechSecret = process.env.E2E_GOOGLE_SECRET!;
  const llmSecret = process.env.E2E_GOOGLE_LLM_SECRET ?? speechSecret;
  const secrets = [...new Set([speechSecret, llmSecret])];
  await page.exposeFunction("__e2eSecret", (i: number) => secrets[i]);
  await page.getByRole("link", { name: "Providers", exact: true }).first().click();
  for (let i = 0; i < secrets.length; i++) {
    await page.getByLabel("Provider service").selectOption("google");
    await page.getByLabel("API key / secret").evaluate(async (el, index) => {
      const value = await (window as unknown as { __e2eSecret(i: number): Promise<string> }).__e2eSecret(index);
      // React tracks the native setter; set through it, then announce the input.
      Object.getOwnPropertyDescriptor(Object.getPrototypeOf(el), "value")!.set!.call(el, value);
      el.dispatchEvent(new Event("input", { bubbles: true }));
    }, i);
    await page.getByRole("button", { name: "Add credential" }).click();
    await expect(page.getByLabel("API key / secret")).toHaveValue("");
  }
  await expect(page.getByText(/••••/).first()).toBeVisible();
  const html = await page.content();
  const echoed = secrets.some((secret) => html.includes(secret.trim().slice(0, 24)));
  expect(echoed, "a stored secret is echoed in the page").toBe(false);
  await step(page, "google credentials added (masked)");
  const speechCredential = await credentialIdFor(stack, speechSecret);
  const llmCredential = await credentialIdFor(stack, llmSecret);

  await page.getByRole("link", { name: "Agents", exact: true }).first().click();
  await page.getByRole("button", { name: "+ Create agent" }).click();
  await page.getByLabel("Agent name").fill(AGENT);
  await page.getByLabel("Description").fill("Created by the full-stack e2e test");
  for (const [stage, credentialId] of [["STT", speechCredential], ["LLM", llmCredential], ["TTS", speechCredential]]) {
    await page.getByLabel(`${stage} provider`).selectOption("google");
    await page.getByLabel(`${stage} credential`).selectOption(credentialId);
  }
  await page.getByLabel("STT model").fill("chirp_2");
  await page.getByLabel("STT language").fill("en-US");
  await page.getByLabel("LLM model").fill("gemini-2.5-flash");
  await page
    .getByLabel("System prompt")
    .fill("You are a concise voice assistant. Answer in one short sentence.");
  await page.getByLabel("TTS voice").fill("en-US-Journey-F");
  await step(page, "agent form filled");
  await page.getByRole("button", { name: "Save agent" }).click();

  await openAgentDetail(page);
  await expect(page.getByTestId("agent-readiness")).toHaveText(/^ready/);
  agentId = await agentIdFromApi(stack);
  expect(agentId).not.toBe("");
  const saved = await api<{ data: { current_version: { config: Record<string, { credential_id: string }> } } }>(stack, `/agents/${agentId}`);
  expect(saved.data.current_version.config.stt.credential_id).toBe(speechCredential);
  expect(saved.data.current_version.config.llm.credential_id).toBe(llmCredential);
  expect(saved.data.current_version.config.tts.credential_id).toBe(speechCredential);
  await step(page, "agent ready");
});

test("admin debug client: voice call and text call", async ({ page, stack }) => {
  await connectAdmin(page, stack);
  await openAgentDetail(page);

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

test("admin UI: delete the agent", async ({ page, request, stack }) => {
  await connectAdmin(page, stack);
  await openAgentDetail(page);
  await page.getByRole("button", { name: "Delete agent" }).click();
  await step(page, "delete confirmation");
  await page.getByRole("button", { name: "Confirm delete" }).click();
  await expect(page.getByRole("row").filter({ hasText: AGENT })).toHaveCount(0);
  await step(page, "agent deleted");

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

// The id of the stored credential whose masked value ends like secret.
async function credentialIdFor(stack: Stack, secret: string): Promise<string> {
  const body = await api<{ data: { id: string; masked_value: string }[] }>(stack, "/providers/credentials");
  const suffix = [...secret.trim()].slice(-4).join("");
  const match = body.data.find((c) => c.masked_value.endsWith(suffix));
  expect(match, "credential stored for secret").toBeTruthy();
  return match!.id;
}
