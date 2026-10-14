import type { Page } from "@playwright/test";
import { expect, LIVE, PROVIDER, type Stack } from "./flows-stack";
import { flowsStackFile, readStack } from "./stack";

// Shared by the flow specs (tests-flows/): the stack's admin API, the admin UI's navigation, the
// flow editor and the Test call panel. The dealership flow (FLOW) is created by the lifecycle
// spec's first test; later specs rely on it and on the files running in order.

export const FLOW = "E2E Dealership";
// The sales step must say "catalogue" in every reply: the marker that sales is talking.
// (In the fake, the label is the whole prompt, so the marker is there too.)
export const FRONT_PROMPT =
  "You are the front desk of a car dealership. Answer in one short sentence. " +
  "When the caller wants to buy a car or asks about prices, call the transfer_to_sales tool, " +
  "then say one short sentence that you are connecting them.";
export const SALES_PROMPT = "You are the sales agent of a car dealership. Answer in one short sentence and always use the word 'catalogue'.";
export const SALES = /catalogue/i;
export const FRONT = /front desk/i; // fake only: the label
// Contains the fake's trigger phrase and reads as a natural request to a real model.
export const BUY = "I want to buy a car. Please transfer to sales.";

export type Entry = { id: string; default?: boolean };
export type Service = { available: boolean; missing_env?: string[]; models?: Entry[]; voices?: Entry[] };
export type ProviderStatus = { id: string; display_name: string; detected: boolean; services: Partial<Record<"stt" | "llm" | "tts", Service>> };
// GET /api/v1/providers, read by loadProviders before a spec's tests (so a single --grep run has it too).
export const providers: ProviderStatus[] = [];
export async function loadProviders() {
  if (LIVE && !process.env.E2E_XAI_SECRET) return; // skipped run: no stack
  providers.splice(0, providers.length, ...(await api<{ data: ProviderStatus[] }>(readStack(flowsStackFile), "/providers")).data);
}
export const catalogDefault = (kind: "stt" | "llm" | "tts") => {
  const svc = providers.find((p) => p.id === PROVIDER)?.services[kind];
  return (kind === "tts" ? svc?.voices : svc?.models)?.find((e) => e.default)?.id ?? "";
};
// The fake's own model and voice names are not in the catalog: entered through "Other…".
// The echo provider's lists are closed (no "Other…"), so both stacks pick the catalog default.
export const LLM_MODEL = () => catalogDefault("llm");

export async function api<T = any>(stack: Stack, route: string, init?: RequestInit): Promise<T> {
  const res = await fetch(`${stack.adminUrl}/api/v1${route}`, {
    ...init,
    headers: { Authorization: `Bearer ${stack.managementKey}`, "Content-Type": "application/json" },
  });
  expect(res.ok, `${init?.method ?? "GET"} ${route} -> ${res.status}`).toBeTruthy();
  return (await res.json()) as T;
}

// The server-side timeline of the session that moved between steps (admin listener).
export async function timelineEvents(stack: Stack): Promise<{ type: string; details?: string }[]> {
  for (let id = 1; id <= 10; id++) {
    const res = await fetch(`${stack.adminUrl}/debug/timeline?session=${id}`);
    if (!res.ok) continue;
    const body = (await res.json()) as { events: { type: string; details?: string }[] };
    if (body.events.some((e) => e.type === "route")) return body.events;
  }
  return [];
}

export async function connectAdmin(page: Page, stack: Stack) {
  await page.goto(`${stack.uiUrl}/dashboard`);
  await page.getByLabel("Management key").fill(stack.managementKey);
  await page.getByRole("button", { name: "Connect", exact: true }).click();
  await expect(page.getByText("Key in memory")).toBeVisible();
}

export async function openFlowsList(page: Page) {
  await page.getByRole("link", { name: "Flows", exact: true }).first().click();
  await expect(page.getByRole("heading", { name: "Flows", level: 1 })).toBeVisible();
}

// Opens a flow in the editor from the list (or leaves it open if it already is).
export async function openFlow(page: Page, name: string) {
  const heading = page.getByRole("heading", { name, level: 1 });
  if (await heading.isVisible()) return;
  await openFlowsList(page);
  await page.getByRole("link", { name, exact: true }).click();
  await expect(heading).toBeVisible();
}

// The editor's Test call panel, where the voice test lives.
export async function openFlowTest(page: Page, name: string) {
  await openFlow(page, name);
  await page.getByRole("button", { name: "Test call" }).click();
  await expect(page.getByTestId("flow-readiness")).toHaveText(/^ready/);
}

// The bot's reply is final in the transcript, or still streaming.
export function botReply(page: Page, text: RegExp) {
  return page
    .locator('[data-testid="transcript-bot"], [data-testid="bot-streaming-text"]')
    .filter({ hasText: text })
    .first();
}

export async function startCall(page: Page) {
  await page.getByTestId("start-voice-test-btn").click();
  await expect(page.getByTestId("connection-state")).toHaveText(/ready/i, { timeout: 30_000 });
  // The fake microphone loops; mute it so only typed turns reach the flow.
  await page.getByTestId("mute-btn").click();
}

export async function sendText(page: Page, text: string) {
  await page.getByRole("textbox", { name: "Message" }).fill(text);
  await page.getByTestId("send-text-btn").click();
  await expect(page.getByTestId("transcript-user").filter({ hasText: text })).toBeVisible();
}

export const botReplies = (page: Page) => page.getByTestId("transcript-bot");

// A model or voice dropdown: the catalog default, or "Other…" and a typed value.
export async function pick(page: Page, label: string, value: string) {
  // By role: a label's text includes its select's option texts, so getByLabel cannot match exactly.
  const select = page.getByRole("combobox", { name: label, exact: true });
  if (await select.locator(`option[value="${value}"]`).count()) return select.selectOption(value);
  await select.selectOption({ label: "Other…" });
  await page.getByRole("textbox", { name: `${label} (custom)`, exact: true }).fill(value);
}

export const stage = (extra: object = {}) => ({ provider: PROVIDER, ...extra });

export async function openDesigner(page: Page, stack: Stack) {
  await connectAdmin(page, stack);
  await openFlowsList(page);
  await page.getByRole("button", { name: "New flow" }).click();
  await page.getByLabel("Name").fill("Main flow");
  await page.getByRole("button", { name: "Create and open editor" }).click();
  await expect(page.getByRole("heading", { name: "Main flow", level: 1 })).toBeVisible();
  await page.getByRole("button", { name: "Add first step" }).click();
  await page.getByLabel("Step name").fill("Main agent");
}

// A step on the React Flow canvas: clicking it opens its panel.
export const stepCard = (page: Page, name: string) => page.locator(".react-flow__node").filter({ hasText: name });
export const openFlowSettings = (page: Page) => page.getByRole("button", { name: "Flow settings" }).click();
