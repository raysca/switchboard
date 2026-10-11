import type { Page } from "@playwright/test";
import { test, expect, LIVE, PROVIDER, type Stack } from "../lib/graph-stack";
import { step } from "../lib/evidence";

// Agent graphs end to end in a real browser against a real server. Two modes:
//  - fake providers (default, free): the fake LLM echoes the node's system prompt as a
//    label and calls a node's transfer tool on "transfer to <node>", so every step is
//    deterministic;
//  - live xAI (E2E_PROVIDER=xai, make e2e-xai): real STT/LLM/TTS calls, so the model
//    itself must call the transfer tool. Assertions then use the prompts' marker
//    words, not exact text.
// Either way: the designer, the stored graph, the call, the handoff and the live
// graph view are exercised together.
test.describe.configure({ mode: "serial" });

const AGENT = "E2E Dealership";
// The sales agent must say "catalogue" in every reply: the marker that sales is talking.
// (In the fake, the label is the whole prompt, so the marker is there too.)
const FRONT_PROMPT =
  "You are the front desk of a car dealership. Answer in one short sentence. " +
  "When the caller wants to buy a car or asks about prices, call the transfer_to_sales tool, " +
  "then say one short sentence that you are connecting them.";
const SALES_PROMPT = "You are the sales agent of a car dealership. Answer in one short sentence and always use the word 'catalogue'.";
const SALES = /catalogue/i;
const FRONT = /front desk/i; // fake only: the label
const LLM_MODEL = LIVE ? (process.env.XAI_MODEL ?? "grok-2-1212") : "fake-llm";
// Contains the fake's trigger phrase and reads as a natural request to a real model.
const BUY = "I want to buy a car. Please transfer to sales.";


let credentialId = "";

async function api<T>(stack: Stack, route: string, init?: RequestInit): Promise<T> {
  const res = await fetch(`${stack.adminUrl}/api/v1${route}`, {
    ...init,
    headers: { Authorization: `Bearer ${stack.managementKey}`, "Content-Type": "application/json" },
  });
  expect(res.ok, `${init?.method ?? "GET"} ${route} -> ${res.status}`).toBeTruthy();
  return (await res.json()) as T;
}

async function connectAdmin(page: Page, stack: Stack) {
  await page.goto(`${stack.uiUrl}/dashboard`);
  await page.getByLabel("Management key").fill(stack.managementKey);
  await page.getByRole("button", { name: "Connect", exact: true }).click();
  await expect(page.getByText("Key in memory")).toBeVisible();
}

async function openAgentsList(page: Page) {
  await page.getByRole("link", { name: "Agents", exact: true }).first().click();
}

async function openAgentDetail(page: Page, name: string) {
  const heading = page.getByRole("heading", { name });
  if (await heading.isVisible()) return;
  await openAgentsList(page);
  await page.getByRole("row").filter({ hasText: name }).getByRole("button", { name: "View detail" }).click();
  await expect(heading).toBeVisible();
}

// The bot's reply is final in the transcript, or still streaming.
function botReply(page: Page, text: RegExp) {
  return page
    .locator('[data-testid="transcript-bot"], [data-testid="bot-streaming-text"]')
    .filter({ hasText: text })
    .first();
}

async function startCall(page: Page) {
  await page.getByTestId("start-voice-test-btn").click();
  await expect(page.getByTestId("connection-state")).toHaveText(/ready/i, { timeout: 30_000 });
  // The fake microphone loops; mute it so only typed turns reach the agent.
  await page.getByTestId("mute-btn").click();
}

async function sendText(page: Page, text: string) {
  await page.getByRole("textbox", { name: "Message" }).fill(text);
  await page.getByTestId("send-text-btn").click();
  await expect(page.getByTestId("transcript-user").filter({ hasText: text })).toBeVisible();
}

const liveStatus = (page: Page) => page.getByRole("status").filter({ hasText: /is answering/ });
const pathTaken = (page: Page) => page.getByRole("list", { name: /path taken/i });
const botReplies = (page: Page) => page.getByTestId("transcript-bot");

async function fillStage(page: Page, stage: "STT" | "LLM" | "TTS") {
  await page.getByLabel(`${stage} provider`).selectOption(PROVIDER);
  await page.getByLabel(`${stage} credential`).selectOption(credentialId);
}

const stage = (extra: object = {}) => ({ provider: PROVIDER, credential_id: credentialId, ...extra });

test("designer: draw a two-agent graph with a handoff and publish it", async ({ page, stack }) => {
  // The provider credential. The secret goes from this process to the management
  // API only: it never enters the browser, a trace or a report.
  const created = await api<{ data: { id: string } }>(stack, "/providers/credentials", {
    method: "POST",
    body: JSON.stringify({ provider: PROVIDER, secret: LIVE ? process.env.E2E_XAI_SECRET : "fake-google-secret-for-e2e" }),
  });
  credentialId = created.data.id;

  await connectAdmin(page, stack);
  await openAgentsList(page);
  await page.getByRole("button", { name: "+ Create graph agent" }).click();
  await expect(page.getByRole("heading", { name: "Design a graph agent" })).toBeVisible();
  await page.getByLabel("Agent name").fill(AGENT);

  // The entry node answers first and owns speech-to-text.
  await fillStage(page, "STT");
  if (!LIVE) await page.getByLabel("STT model").fill("chirp");
  await fillStage(page, "LLM");
  await page.getByLabel("LLM model").fill(LLM_MODEL);
  await page.getByLabel("System prompt").fill(FRONT_PROMPT);
  await fillStage(page, "TTS");
  if (!LIVE) await page.getByLabel("Voice").fill("frontvoice");

  // A second agent: it starts from the entry's model and voice; give it its own prompt.
  await page.getByLabel("New agent node").fill("Sales");
  await page.getByRole("button", { name: "Add agent" }).click();
  await expect(page.getByLabel("Node name")).toHaveValue("Sales");
  await page.getByLabel("System prompt").fill(SALES_PROMPT);
  if (!LIVE) await page.getByLabel("Voice").fill("salesvoice");

  // The handoff between them.
  await page.locator("#handoff-from").selectOption({ label: "Main agent" });
  await page.locator("#handoff-to").selectOption({ label: "Sales" });
  await page.getByLabel("Hand off when").fill("The caller wants to buy a car or asks about prices.");
  await page.getByRole("button", { name: "Add handoff" }).click();
  await expect(page.getByRole("button", { name: "Main agent → Sales" })).toBeVisible();
  await step(page, "graph designed");

  await page.getByRole("button", { name: "Publish" }).click();
  await openAgentDetail(page, AGENT);
  await expect(page.getByTestId("agent-readiness")).toHaveText(/^ready/);
  await expect(pathTaken(page)).toContainText("Main agent");
  await step(page, "graph agent published and ready");

  // The stored version is the graph the designer drew.
  const agents = await api<{ data: { name: string; current_version: { config: any } }[] }>(stack, "/agents");
  const cfg = agents.data.find((a) => a.name === AGENT)!.current_version.config;
  expect(cfg.nodes.map((n: any) => n.name)).toEqual(["Main agent", "Sales"]);
  expect(cfg.nodes[0].config.stt).toBeTruthy();
  expect(cfg.nodes[1].config.stt).toBeUndefined();
  expect(cfg.nodes[1].config.llm.system_prompt).toBe(SALES_PROMPT);
  expect(cfg.edges).toHaveLength(1);
  expect(cfg.entry).toBe(cfg.nodes[0].id);
});

test("call: the front agent hands the call to sales and the live graph follows", async ({ page, stack }) => {
  await connectAdmin(page, stack);
  await openAgentDetail(page, AGENT);
  await startCall(page);
  await expect(liveStatus(page)).toContainText("Main agent is answering");

  await sendText(page, "hello");
  await expect(botReplies(page).filter({ hasText: LIVE ? /./ : FRONT }).first()).toBeVisible();
  await expect(botReplies(page).filter({ hasText: SALES })).toHaveCount(0);

  await sendText(page, BUY);
  // The new agent speaks (the transfer, the farewell and the greeting come from the models),
  // and the graph shows the call moved over.
  await expect(botReply(page, SALES)).toBeVisible({ timeout: LIVE ? 90_000 : 20_000 });
  await expect(liveStatus(page)).toContainText("Sales is answering", { timeout: LIVE ? 90_000 : 15_000 });
  await expect(pathTaken(page)).toContainText("Main agent");
  await expect(pathTaken(page)).toContainText("Sales");
  await step(page, "call moved to sales");

  const salesBefore = await botReplies(page).filter({ hasText: SALES }).count();
  await sendText(page, "what do you sell");
  await expect.poll(() => botReplies(page).filter({ hasText: SALES }).count(), { timeout: LIVE ? 90_000 : 15_000 }).toBeGreaterThan(salesBefore);
  if (!LIVE) await expect(botReplies(page).filter({ hasText: /front desk.*what do you sell/i })).toHaveCount(0);

  // The handoff is recorded on the stored session, for replay: node ids, not just names.
  const sessionId = (await page.getByTestId("session-summary").locator("code").innerText()).trim();
  await expect
    .poll(async () => {
      const t = await api<{ data: { spans: { name: string; attributes?: { from_node?: string; to_node?: string } }[] } }>(stack, `/sessions/${sessionId}/traces`);
      const active = t.data.spans.find((s) => s.name === "handoff.active");
      return [t.data.spans.some((s) => s.name === "handoff.preparing"), !!active?.attributes?.from_node, !!active?.attributes?.to_node];
    })
    .toEqual([true, true, true]);

  await page.getByTestId("disconnect-btn").click();
  await expect(page.getByTestId("connection-state")).toHaveText(/disconnected/i);
});

test("designer: reopen the graph, edit it, republish; a new call gets the new version", async ({ page, stack }) => {
  await connectAdmin(page, stack);
  await openAgentsList(page);
  await page.getByRole("row").filter({ hasText: AGENT }).getByRole("button", { name: "Edit" }).click();
  await expect(page.getByRole("heading", { name: `Design ${AGENT}` })).toBeVisible();
  await expect(page.getByRole("button", { name: "Main agent → Sales" })).toBeVisible();

  // A third node can be added and removed again; then edit the handoff condition.
  await page.getByLabel("New agent node").fill("Support");
  await page.getByRole("button", { name: "Add agent" }).click();
  await page.getByRole("button", { name: "Remove node" }).click();
  await page.getByRole("button", { name: "Main agent → Sales" }).click();
  await page.getByLabel("When should it hand off?").fill("The caller wants to buy a car, asks about prices, or wants a quote.");
  await step(page, "handoff condition edited");
  await page.getByRole("button", { name: "Publish" }).click();
  await openAgentDetail(page, AGENT);

  const agents = await api<{ data: { name: string; current_version: { number: number; config: any } }[] }>(stack, "/agents");
  const v = agents.data.find((a) => a.name === AGENT)!.current_version;
  expect(v.number).toBe(2);
  expect(v.config.nodes).toHaveLength(2);
  expect(v.config.edges[0].condition).toBe("The caller wants to buy a car, asks about prices, or wants a quote.");

  await startCall(page);
  await sendText(page, BUY);
  await expect(botReply(page, SALES)).toBeVisible({ timeout: LIVE ? 90_000 : 20_000 });
  await page.getByTestId("disconnect-btn").click();
});

// Fake only: a node that cannot be built needs a deliberately broken voice, which a
// real provider cannot be made to have on demand.
test("failure: a sales node that cannot be built leaves the caller with the front agent", async ({ page, stack }) => {
  test.skip(LIVE, "needs the fake provider's brokenvoice");
  const body = {
    name: "E2E Broken Sales",
    config: {
      entry: "front",
      nodes: [
        { id: "front", name: "Front", config: { stt: stage({ model: "chirp" }), llm: stage({ model: "m", system_prompt: FRONT_PROMPT }), tts: stage({ voice: "frontvoice" }) } },
        { id: "sales", name: "Sales", config: { llm: stage({ model: "m", system_prompt: SALES_PROMPT }), tts: stage({ voice: "brokenvoice" }) } },
      ],
      edges: [{ id: "e1", from: "front", to: "sales", condition: "The caller wants to buy." }],
    },
  };
  await api(stack, "/agents", { method: "POST", body: JSON.stringify(body) });

  await connectAdmin(page, stack);
  await openAgentDetail(page, "E2E Broken Sales");
  await expect(page.getByTestId("agent-readiness")).toHaveText(/^ready/);
  await startCall(page);

  await sendText(page, BUY);
  await expect(botReply(page, /Connecting you/)).toBeVisible();
  await sendText(page, "are you still there");
  await expect(botReply(page, /front desk.*are you still there/i)).toBeVisible();
  await expect(botReplies(page).filter({ hasText: SALES })).toHaveCount(0);
  await expect(liveStatus(page)).toContainText("Front is answering");
  await step(page, "handoff failed, caller kept by the front agent");

  const sessionId = (await page.getByTestId("session-summary").locator("code").innerText()).trim();
  await expect
    .poll(async () => {
      const t = await api<{ data: { spans: { name: string }[] } }>(stack, `/sessions/${sessionId}/traces`);
      return t.data.spans.map((s) => s.name);
    })
    .toContain("handoff.failed");
  await page.getByTestId("disconnect-btn").click();
});

test("a plain agent still works unchanged", async ({ page, stack }) => {
  await api(stack, "/agents", {
    method: "POST",
    body: JSON.stringify({ name: "E2E Plain", config: { stt: stage(LIVE ? {} : { model: "chirp" }), llm: stage({ model: LLM_MODEL, system_prompt: "You are PLAIN. Answer in one short sentence." }), tts: stage() } }),
  });
  await connectAdmin(page, stack);
  await openAgentDetail(page, "E2E Plain");
  await expect(pathTaken(page)).toHaveCount(0); // no graph view for a one-node agent
  await startCall(page);
  await sendText(page, "hello");
  await expect(botReply(page, /./)).toBeVisible({ timeout: LIVE ? 60_000 : 15_000 });
  await expect(botReplies(page).filter({ hasText: /Connecting you/ })).toHaveCount(0);
  await page.getByTestId("disconnect-btn").click();
});
