import type { Page } from "@playwright/test";
import { test, expect, type Stack } from "../lib/fake-stack";
import { step } from "../lib/evidence";

// Agent graphs end to end with the fake providers (free, offline). The fake
// LLM labels each reply with its node's system prompt ("[FRONT] You said: ...")
// and calls a node's transfer tool when told "transfer to <node>", so the
// designer, the stored graph, the live call, the handoff and the live graph
// view are all exercised from a real browser against a real server.
test.describe.configure({ mode: "serial" });

const AGENT = "E2E Dealership";
let credentialId = "";
let agentId = "";

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

async function fillStage(page: Page, stage: "STT" | "LLM" | "TTS") {
  await page.getByLabel(`${stage} provider`).selectOption("google");
  await page.getByLabel(`${stage} credential`).selectOption(credentialId);
}

test.beforeAll(async ({}, info) => {
  info.setTimeout(30_000);
});

test("designer: draw a two-agent graph with a handoff and publish it", async ({ page, stack }) => {
  // A stored credential for the fake providers (any secret works).
  const created = await api<{ data: { id: string } }>(stack, "/providers/credentials", {
    method: "POST",
    body: JSON.stringify({ provider: "google", secret: "fake-google-secret-for-e2e" }),
  });
  credentialId = created.data.id;

  await connectAdmin(page, stack);
  await openAgentsList(page);
  await page.getByRole("button", { name: "+ Create graph agent" }).click();
  await expect(page.getByRole("heading", { name: "Design a graph agent" })).toBeVisible();
  await page.getByLabel("Agent name").fill(AGENT);

  // The entry node answers first and owns speech-to-text.
  await fillStage(page, "STT");
  await page.getByLabel("STT model").fill("chirp");
  await fillStage(page, "LLM");
  await page.getByLabel("LLM model").fill("fake-llm");
  await page.getByLabel("System prompt").fill("FRONT");
  await fillStage(page, "TTS");
  await page.getByLabel("Voice").fill("frontvoice");

  // A second agent: it starts from the entry's model and voice; give it its own.
  await page.getByLabel("New agent node").fill("Sales");
  await page.getByRole("button", { name: "Add agent" }).click();
  await expect(page.getByLabel("Node name")).toHaveValue("Sales");
  await page.getByLabel("System prompt").fill("SALES");
  await page.getByLabel("Voice").fill("salesvoice");

  // The handoff between them.
  await page.locator("#handoff-from").selectOption({ label: "Main agent" });
  await page.locator("#handoff-to").selectOption({ label: "Sales" });
  await page.getByLabel("Hand off when").fill("The caller wants to buy a car.");
  await page.getByRole("button", { name: "Add handoff" }).click();
  await expect(page.getByRole("button", { name: "Main agent → Sales" })).toBeVisible();
  await step(page, "graph designed");

  await page.getByRole("button", { name: "Publish" }).click();
  await openAgentDetail(page, AGENT);
  await expect(page.getByTestId("agent-readiness")).toHaveText(/^ready/);
  await expect(pathTaken(page)).toContainText("Main agent");
  await step(page, "graph agent published and ready");

  // The stored version is the graph the designer drew.
  const agents = await api<{ data: { id: string; name: string; current_version: { number: number; config: any } }[] }>(stack, "/agents");
  const stored = agents.data.find((a) => a.name === AGENT)!;
  agentId = stored.id;
  const cfg = stored.current_version.config;
  expect(cfg.nodes.map((n: any) => n.name)).toEqual(["Main agent", "Sales"]);
  expect(cfg.nodes[0].config.stt).toBeTruthy();
  expect(cfg.nodes[1].config.stt).toBeUndefined();
  expect(cfg.nodes[1].config.llm.system_prompt).toBe("SALES");
  expect(cfg.edges).toHaveLength(1);
  expect(cfg.edges[0].condition).toBe("The caller wants to buy a car.");
  expect(cfg.entry).toBe(cfg.nodes[0].id);
});

test("call: the front agent hands the call to sales and the live graph follows", async ({ page, stack }) => {
  await connectAdmin(page, stack);
  await openAgentDetail(page, AGENT);
  await startCall(page);
  await expect(liveStatus(page)).toContainText("Main agent is answering");

  await sendText(page, "hello");
  await expect(botReply(page, /\[FRONT\].*hello/)).toBeVisible();

  await sendText(page, "please transfer to sales");
  await expect(botReply(page, /Connecting you/)).toBeVisible();
  // The new agent greets, and the graph shows the call moved over.
  await expect(botReply(page, /\[SALES\]/)).toBeVisible({ timeout: 20_000 });
  await expect(liveStatus(page)).toContainText("Sales is answering");
  await expect(pathTaken(page)).toContainText("Main agent");
  await expect(pathTaken(page)).toContainText("Sales");
  await step(page, "call moved to sales");

  await sendText(page, "what do you sell");
  await expect(botReply(page, /\[SALES\].*what do you sell/)).toBeVisible();
  await expect(page.getByTestId("transcript-bot").filter({ hasText: /\[FRONT\].*what do you sell/ })).toHaveCount(0);

  // The handoff is recorded on the stored session, for replay.
  const sessionId = (await page.getByTestId("session-summary").locator("code").innerText()).trim();
  await expect
    .poll(async () => {
      const t = await api<{ data: { spans: { name: string }[] } }>(stack, `/sessions/${sessionId}/traces`);
      return t.data.spans.map((s) => s.name).filter((n) => n.startsWith("handoff."));
    })
    .toEqual(expect.arrayContaining(["handoff.preparing", "handoff.active"]));

  await page.getByTestId("disconnect-btn").click();
  await expect(page.getByTestId("connection-state")).toHaveText(/disconnected/i);
});

test("designer: reopen the graph, change sales, republish; a new call gets the new version", async ({ page, stack }) => {
  await connectAdmin(page, stack);
  await openAgentsList(page);
  await page.getByRole("row").filter({ hasText: AGENT }).getByRole("button", { name: "Edit" }).click();
  await expect(page.getByRole("heading", { name: `Design ${AGENT}` })).toBeVisible();
  await expect(page.getByRole("button", { name: "Main agent → Sales" })).toBeVisible();

  // Open the Sales node from the handoff and edit it.
  await page.getByLabel("New agent node").fill("Support");
  await page.getByRole("button", { name: "Add agent" }).click(); // proves adding a third node works
  await page.getByRole("button", { name: "Remove node" }).click();
  await page.getByRole("button", { name: "Main agent → Sales" }).click();
  await page.getByLabel("When should it hand off?").fill("The caller wants to buy a car or asks about prices.");
  await step(page, "handoff condition edited");
  await page.getByRole("button", { name: "Publish" }).click();
  await openAgentDetail(page, AGENT);

  const agents = await api<{ data: { name: string; current_version: { number: number; config: any } }[] }>(stack, "/agents");
  const v = agents.data.find((a) => a.name === AGENT)!.current_version;
  expect(v.number).toBe(2);
  expect(v.config.nodes).toHaveLength(2);
  expect(v.config.edges[0].condition).toBe("The caller wants to buy a car or asks about prices.");

  await startCall(page);
  await sendText(page, "transfer to sales");
  await expect(botReply(page, /\[SALES\]/)).toBeVisible({ timeout: 20_000 });
  await page.getByTestId("disconnect-btn").click();
});

test("failure: a sales node that cannot be built leaves the caller with the front agent", async ({ page, stack }) => {
  const stage = (extra: object) => ({ provider: "google", credential_id: credentialId, ...extra });
  const body = {
    name: "E2E Broken Sales",
    config: {
      entry: "front",
      nodes: [
        { id: "front", name: "Front", config: { stt: stage({ model: "chirp" }), llm: stage({ model: "m", system_prompt: "FRONT" }), tts: stage({ voice: "frontvoice" }) } },
        { id: "sales", name: "Sales", config: { llm: stage({ model: "m", system_prompt: "SALES" }), tts: stage({ voice: "brokenvoice" }) } },
      ],
      edges: [{ id: "e1", from: "front", to: "sales", condition: "The caller wants to buy." }],
    },
  };
  await api(stack, "/agents", { method: "POST", body: JSON.stringify(body) });

  await connectAdmin(page, stack);
  await openAgentDetail(page, "E2E Broken Sales");
  await expect(page.getByTestId("agent-readiness")).toHaveText(/^ready/);
  await startCall(page);

  await sendText(page, "transfer to sales");
  await expect(botReply(page, /Connecting you/)).toBeVisible();
  await sendText(page, "are you still there");
  await expect(botReply(page, /\[FRONT\].*are you still there/)).toBeVisible();
  await expect(page.getByTestId("transcript-bot").filter({ hasText: /\[SALES\]/ })).toHaveCount(0);
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
  const stage = (extra: object) => ({ provider: "google", credential_id: credentialId, ...extra });
  await api(stack, "/agents", {
    method: "POST",
    body: JSON.stringify({ name: "E2E Plain", config: { stt: stage({}), llm: stage({ model: "m", system_prompt: "PLAIN" }), tts: stage({}) } }),
  });
  await connectAdmin(page, stack);
  await openAgentDetail(page, "E2E Plain");
  await expect(pathTaken(page)).toHaveCount(0); // no graph view for a one-node agent
  await startCall(page);
  await sendText(page, "transfer to sales");
  await expect(botReply(page, /\[PLAIN\].*transfer to sales/)).toBeVisible();
  await expect(page.getByTestId("transcript-bot").filter({ hasText: /Connecting you/ })).toHaveCount(0);
  await page.getByTestId("disconnect-btn").click();
});
