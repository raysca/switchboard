import { test, expect, LIVE } from "../lib/agents-stack";
import type { Stack } from "../lib/agents-stack";
import { agentsStackFile, readStack } from "../lib/stack";
import { step } from "../lib/evidence";
import { api, botReplies, connectAdmin, openAgent, openAgentTest, openAgentsList, startCall, stepCard } from "../lib/agents-helpers";
import { createEchoDentalVariant, toolTurn, turn } from "../lib/dental-helpers";
import { seedDemo } from "../lib/seed";

// What this verifies: the standalone seed CLI (bin/seed, from demos/seed) publishes the demo on a
// clean server; a call is handed off between steps (front desk -> New patient / Returning patient),
// the new step takes the next turn, its MCP tools run against the real dental server, end_call hangs
// the call up, and the Calls page shows the path, the step-labelled transcript and how it ended.
// NOT verified here: that the transfer note (the caller's name) reaches the new step's greeting.
// The fake LLM sends no note; that plumbing is covered by Go unit tests and is to be verified in
// the live rehearsal.
//
// The dentistry demo, end to end: seeded in beforeAll (the real dental MCP server from
// demos/dental/cmd/dental-mcp, started by the global setup), then two fake-provider calls on a
// variant of the seeded agent. Fake only: the seeded agent's xai/elevenlabs stages cannot take a
// fake call. Seeding here, not in global setup, keeps 05-first-run's empty store and 50-screens'
// archive-every-agent assumption intact.
test.describe.configure({ mode: "serial" });
test.skip(LIVE, "the dentistry demo runs against the fake stack only");

const AGENT = "Smile Dental front desk";

type StoredDental = {
  id: string;
  name: string;
  current_version: {
    config: { start: string; steps: { name: string; end_call?: boolean }[]; handoffs: unknown[] };
    mcp_tools: unknown[];
  };
};

test.beforeAll(() => {
  if (LIVE) return; // the tests are skipped: the seeded stages need the fake stack's placeholder keys
  seedDemo(readStack(agentsStackFile), "dental");
});

test("the CLI seed publishes the demo: three steps, two handoffs and seven tool pins", async ({ page, stack }) => {
  const agents = await api<{ data: { id: string; name: string }[] }>(stack, "/agents");
  const live = agents.data.filter((a) => a.name === AGENT);
  expect(live, `bin/seed leaves exactly one live ${AGENT} agent`).toHaveLength(1);
  const { data: stored } = await api<{ data: StoredDental }>(stack, `/agents/${live[0].id}`);
  const { config, mcp_tools } = stored.current_version;
  expect(stored.name).toBe(AGENT);
  expect(config.steps).toHaveLength(3);
  expect(config.handoffs).toHaveLength(2);
  expect(config.steps.every((s) => s.end_call)).toBe(true);
  expect(mcp_tools).toHaveLength(7);

  // The seeded agent is in the list and its three steps and two handoffs draw on the canvas;
  // the demo card that used to install it from the Dashboard is gone.
  await connectAdmin(page, stack);
  await openAgentsList(page);
  await expect(page.getByRole("link", { name: AGENT, exact: true })).toBeVisible();
  await step(page, "dentistry demo seeded");

  await openAgent(page, AGENT);
  const canvas = page.getByTestId("agent-canvas");
  await expect(canvas).toBeVisible();
  await expect(canvas.locator(".react-flow__node")).toHaveCount(3);
  await expect(canvas.locator(".react-flow__edge")).toHaveCount(2);
  await step(page, "dentistry demo canvas");

  await createEchoDentalVariant(stack, live[0].id);
});

test("a new patient call: handed off between steps, MCP tools called, hung up by end_call", async ({ page, stack }) => {
  await connectAdmin(page, stack);
  await openAgentTest(page, "Dental (echo)");
  await startCall(page);

  await turn(page, "hello");
  await turn(page, "transfer to new patient");
  await expect(stepCard(page, "New patient").locator(".step-node")).toHaveAttribute("data-state", "active", { timeout: 15_000 });
  await toolTurn(page, "register_patient", { name: "Sam Taylor", phone: "555 0100" }, /Tool register_patient returned.*Sam Taylor/);
  const patient = (await botReplies(page).last().innerText()).match(/"id":\s*"(p\d+)"/)?.[1];
  expect(patient, "register_patient reply carries a patient id").toBeTruthy();
  await toolTurn(page, "find_slots", {});
  const slot = (await botReplies(page).last().innerText()).match(/\d{4}-\d{2}-\d{2}T\d{2}:\d{2}/)?.[0];
  expect(slot, "find_slots reply carries a slot").toBeTruthy();
  await toolTurn(page, "book_appointment", { patient_id: patient, slot_id: slot, kind: "checkup" });
  await step(page, "new patient booked");

  await turn(page, "end the call");
  await expect(page.getByTestId("connection-state")).toHaveText(/disconnected/i, { timeout: 20_000 });
});

async function openLatestCall(page: import("@playwright/test").Page, stack: Stack, nth: number) {
  await connectAdmin(page, stack);
  await page.getByRole("link", { name: "Calls", exact: true }).first().click();
  await expect(page.getByRole("heading", { name: "Calls", level: 1 })).toBeVisible();
  await page.getByRole("combobox", { name: "Agent", exact: true }).selectOption({ label: "Dental (echo)" });
  const rows = page.getByRole("row").filter({ hasText: "Dental (echo)" });
  await expect(rows).toHaveCount(nth, { timeout: 20_000 });
  await expect(rows.first()).toContainText("Ended", { timeout: 20_000 });
  await rows.first().getByRole("link").click();
  await expect(page.getByRole("heading", { level: 1 })).toContainText("Call ");
}

// The tool calls themselves are mcp.tool.execute spans with no turn id and no tool name (only a
// status), so the page's per-turn timeline cannot show them: count them from the traces API, as
// 30-mcp does.
async function expectToolSpans(page: import("@playwright/test").Page, stack: Stack, count: number) {
  await page.getByText("Technical trace", { exact: true }).click();
  const id = (await page.locator(".call-figures code").first().innerText()).trim();
  const traces = await api<{ data: { spans: { name: string; attributes?: Record<string, unknown> }[] } }>(stack, `/sessions/${id}/traces`);
  const called = traces.data.spans.filter((sp) => sp.name === "mcp.tool.execute").map((sp) => sp.attributes?.status);
  expect(called).toEqual(Array(count).fill("ok"));
}

test("calls: the new patient call shows its path, step labels, how it ended and its tool-call spans", async ({ page, stack }) => {
  await openLatestCall(page, stack, 1);
  await expect(page.getByText("Front desk → New patient", { exact: true })).toBeVisible();
  await expect(page.getByText("By the agent (New patient)")).toBeVisible();
  await expect(page.getByRole("list", { name: "Transcript" }).getByText(/^New patient · /).first()).toBeVisible();
  await expectToolSpans(page, stack, 3); // register_patient, find_slots, book_appointment
  await step(page, "new patient call detail");
});

test("a returning patient call: handed off between steps, MCP tools called, hung up by end_call", async ({ page, stack }) => {
  await connectAdmin(page, stack);
  await openAgentTest(page, "Dental (echo)");
  await startCall(page);
  await turn(page, "transfer to returning patient");
  await expect(stepCard(page, "Returning patient").locator(".step-node")).toHaveAttribute("data-state", "active", { timeout: 15_000 });
  await toolTurn(page, "find_patient", { name: "Maria Lopez" }, /Tool find_patient returned.*Maria/);
  await toolTurn(page, "list_appointments", { patient_id: "p1" });
  await turn(page, "end the call");
  await expect(page.getByTestId("connection-state")).toHaveText(/disconnected/i, { timeout: 20_000 });

  await openLatestCall(page, stack, 2);
  await expect(page.getByText("Front desk → Returning patient", { exact: true })).toBeVisible();
  await expect(page.getByText("By the agent (Returning patient)")).toBeVisible();
  await expectToolSpans(page, stack, 2); // find_patient, list_appointments
  await step(page, "returning patient call detail");
});
