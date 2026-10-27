import { test, expect, LIVE } from "../lib/agents-stack";
import type { Stack } from "../lib/agents-stack";
import { agentsStackFile, readStack } from "../lib/stack";
import { step } from "../lib/evidence";
import { api, connectAdmin, openAgent, openAgentTest, openAgentsList, startCall } from "../lib/agents-helpers";
import {
  ATLAS_AGENT,
  type AtlasAgent,
  ATLAS_ECHO_AGENT,
  createEchoAtlasVariant,
  expectActiveStep,
  storedAtlasAgent,
  toolTurn,
  turn,
} from "../lib/atlas-helpers";
import { seedDemo } from "../lib/seed";

// What this verifies: the standalone seed CLI (bin/seed, from demos/seed) publishes the Atlas demo
// on a clean server; the seeded agent is at the platform limits (8 steps, 32 handoffs, 20 tool pins,
// one async tool); the designer draws the whole graph; and a fake-provider call on an echo variant
// walks Concierge -> Flights -> Stays -> Payments -> Human handoff with MCP and HTTP tools and hangs
// up on end_call. The Calls page then shows the path, the step-labelled transcript and how the call
// ended. Fake only: the seeded agent names google, xai and elevenlabs stages, which run on
// placeholder keys (F2).
//
// The Atlas fixture is the real demos/atlas/cmd/atlas-mcp server the global setup starts; the
// call's tool replies come from it, so the demo data is the same as a person would see. Seeding
// happens here, not in global setup: 05-first-run needs an empty store and 50-screens archives
// every agent before this spec runs.
test.describe.configure({ mode: "serial" });
test.skip(LIVE, "the Atlas demo uses the fake stack's placeholder provider keys");

const STEP_NAMES = ["Concierge", "Flights", "Stays", "Ground transport", "Payments", "Loyalty", "Disruptions", "Human handoff"];

test.beforeAll(() => {
  if (LIVE) return; // the tests are skipped: the seeded stages need the fake stack's placeholder keys
  seedDemo(readStack(agentsStackFile), "atlas");
});

test("the CLI seed publishes the demo: eight steps, thirty-two handoffs and twenty tool pins", async ({ page, stack }) => {
  const agent = await storedAtlasAgent(stack);
  const { config, mcp_tools } = agent.current_version;
  expect(agent.name).toBe(ATLAS_AGENT);
  expect(config.steps).toHaveLength(8);
  expect(config.steps.map((s) => s.name)).toEqual(STEP_NAMES);
  expect(config.handoffs).toHaveLength(32);
  expect(config.steps.every((s) => s.end_call)).toBe(true);

  // Eighteen MCP tools plus the two HTTP tools; an HTTP tool's pin is the pseudo server id "http:<id>".
  expect(mcp_tools).toHaveLength(20);
  expect(mcp_tools.filter((t) => t.mcp_server_id.startsWith("http:"))).toHaveLength(2);
  expect(mcp_tools.filter((t) => !t.mcp_server_id.startsWith("http:"))).toHaveLength(18);

  // Only the start step may set call-level settings, so its async tool list is the agent's one.
  const start = config.steps.find((s) => s.id === config.start)!;
  expect(start.name).toBe("Concierge");
  expect(start.config.async_tools).toHaveLength(1);

  // The handoff graph is at the limit: every ordered pair is unique...
  const pairs = config.handoffs.map((h) => `${h.from}->${h.to}`);
  expect(new Set(pairs).size).toBe(32);
  // ...and every step is reachable from the start.
  const seen = new Set([config.start]);
  const queue = [config.start];
  while (queue.length > 0) {
    const from = queue.shift()!;
    for (const h of config.handoffs) if (h.from === from && !seen.has(h.to)) { seen.add(h.to); queue.push(h.to); }
  }
  expect([...seen].sort()).toEqual(config.steps.map((s) => s.id).sort());

  await connectAdmin(page, stack);
  await openAgentsList(page);
  await expect(page.getByRole("link", { name: ATLAS_AGENT, exact: true })).toBeVisible();
  await step(page, "atlas demo seeded");

  // The seeded stages name real providers. A fake stack cannot build them, so the call test
  // drives this variant instead: same steps, handoffs, end_call flags, step tools and tool pins.
  const variant = await createEchoAtlasVariant(stack, agent.id);
  // Read the variant back from the store: the helper's own object is not evidence.
  const stored = await api<{ data: AtlasAgent }>(stack, `/agents/${variant.id}`);
  expect(stored.data.current_version.config.steps).toHaveLength(8);
  expect(stored.data.current_version.mcp_tools).toHaveLength(20);
});

test("the agent page draws the whole graph: eight steps and thirty-two handoffs", async ({ page, stack }) => {
  await connectAdmin(page, stack);
  await openAgent(page, ATLAS_AGENT);
  const canvas = page.getByTestId("agent-canvas");
  await expect(canvas).toBeVisible();
  await expect(canvas.locator(".react-flow__node")).toHaveCount(8);
  await expect(canvas.locator(".react-flow__edge")).toHaveCount(32);
  await step(page, "atlas eight-step canvas");
});

test("a call through the eight steps: handoffs, MCP and HTTP tools, end_call", async ({ page, stack }) => {
  await connectAdmin(page, stack);
  await openAgentTest(page, ATLAS_ECHO_AGENT);
  await startCall(page);
  await expectActiveStep(page, "Concierge");

  // The transfer phrase is "transfer to <step name>"; the echo model calls that handoff's tool.
  // Wait for the target step to be active before the next turn, so it is that step's tool set.
  await turn(page, "transfer to flights");
  await expectActiveStep(page, "Flights");
  await toolTurn(page, "search_flights", { from: "London", to: "New York" }, /Tool search_flights returned.*London/);

  await turn(page, "transfer to stays");
  await expectActiveStep(page, "Stays");
  // weather_forecast is one of the two HTTP endpoint tools, not an MCP tool.
  await toolTurn(page, "weather_forecast", { city: "Paris" }, /Tool weather_forecast returned.*Paris/);

  await turn(page, "transfer to payments");
  await expectActiveStep(page, "Payments");
  await toolTurn(page, "list_payment_methods", { traveller_id: "t1" }, /Tool list_payment_methods returned/);

  await turn(page, "transfer to human handoff");
  await expectActiveStep(page, "Human handoff");
  await toolTurn(page, "create_callback", { name: "Maria Lopez", phone: "555-0142", topic: "refund" }, /Tool create_callback returned.*Maria Lopez/);

  // Every step has end_call: the agent hangs up after the caller says goodbye.
  await turn(page, "end the call");
  await expect(page.getByTestId("connection-state")).toHaveText(/disconnected/i, { timeout: 20_000 });
});

async function openLatestCall(page: import("@playwright/test").Page, stack: Stack, nth: number) {
  await connectAdmin(page, stack);
  await page.getByRole("link", { name: "Calls", exact: true }).first().click();
  await expect(page.getByRole("heading", { name: "Calls", level: 1 })).toBeVisible();
  await page.getByRole("combobox", { name: "Agent", exact: true }).selectOption({ label: ATLAS_ECHO_AGENT });
  const rows = page.getByRole("row").filter({ hasText: ATLAS_ECHO_AGENT });
  await expect(rows).toHaveCount(nth, { timeout: 20_000 });
  await expect(rows.first()).toContainText("Ended", { timeout: 20_000 });
  await rows.first().getByRole("link").click();
  await expect(page.getByRole("heading", { level: 1 })).toHaveText(/^call_/);
}

test("calls: the path through five steps, the transcript labels and how it ended", async ({ page, stack }) => {
  await openLatestCall(page, stack, 1);
  await expect(page.getByText("Concierge → Flights → Stays → Payments → Human handoff", { exact: true })).toBeVisible();
  await expect(page.getByText("By the agent (Human handoff)")).toBeVisible();
  await expect(page.getByRole("list", { name: "Transcript" }).getByText(/^Human handoff · /).first()).toBeVisible();
  await step(page, "atlas call detail");
});
