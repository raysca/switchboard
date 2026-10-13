import { test, expect, LIVE } from "../lib/flows-stack";
import { step } from "../lib/evidence";
import { api, botReplies, connectAdmin, openFlowTest, startCall, stepCard } from "../lib/flows-helpers";
import { createFakeDentalVariant, toolTurn, turn } from "../lib/dental-helpers";

// What this verifies: the demo installs from the Dashboard; a call is handed off by route (front
// desk -> New patient / Returning patient), the new step takes the next turn, its MCP tools run
// against the real dental server, end_call hangs the call up, and the Calls page shows the path,
// the step-labelled transcript and how it ended. NOT verified here: that the transfer note (the
// caller's name) reaches the new step's greeting. The fake LLM sends no note; that plumbing is
// covered by Go unit tests and is to be verified in the live rehearsal.
//
// The dentistry demo, end to end: Load demo from the Dashboard (the real dental MCP server from
// cmd/dental-mcp, started by the global setup), then two fake-provider calls on a variant of the
// installed flow. Fake only: the installed flow's xai/elevenlabs stages cannot take a fake call.
test.describe.configure({ mode: "serial" });
test.skip(LIVE, "the dentistry demo runs against the fake stack only");

test("load the demo from the Dashboard: three steps, two routes", async ({ page, stack }) => {
  await connectAdmin(page, stack);
  await expect(page.getByRole("heading", { name: "Dentistry demo" })).toBeVisible();
  await page.getByRole("button", { name: "Load demo" }).click();
  await expect(page.getByRole("link", { name: "Smile Dental front desk" })).toBeVisible();
  await step(page, "dentistry demo loaded");

  const flows = await api<{ data: { id: string; name: string }[] }>(stack, "/flows");
  const installed = flows.data.find((f) => f.name === "Smile Dental front desk")!;
  const { config, mcp_tools } = await createFakeDentalVariant(stack, installed.id);
  expect(config.steps).toHaveLength(3);
  expect(config.routes).toHaveLength(2);
  expect(config.steps.every((s: any) => s.end_call)).toBe(true);
  expect(mcp_tools).toHaveLength(7);
});

test("a new patient call: handed off by route, MCP tools called, hung up by end_call", async ({ page, stack }) => {
  await connectAdmin(page, stack);
  await openFlowTest(page, "Dental (fake)");
  await startCall(page);

  await turn(page, "hello");
  await turn(page, "transfer to new patient");
  await expect(stepCard(page, "New patient").locator(".step-node")).toHaveAttribute("data-state", "active", { timeout: 15_000 });
  await toolTurn(page, "register_patient", { name: "Sam Taylor", phone: "555 0100" }, /Tool register_patient returned.*Sam Taylor/);
  const patient = (await botReplies(page).last().innerText()).match(/"id":\s*"(p\d+)"/)![1];
  await toolTurn(page, "find_slots", {});
  const slot = (await botReplies(page).last().innerText()).match(/\d{4}-\d{2}-\d{2}T\d{2}:\d{2}/)![0];
  await toolTurn(page, "book_appointment", { patient_id: patient, slot_id: slot, kind: "checkup" });
  await step(page, "new patient booked");

  await turn(page, "end the call");
  await expect(page.getByTestId("connection-state")).toHaveText(/disconnected/i, { timeout: 20_000 });
});

async function openLatestCall(page: import("@playwright/test").Page, stack: any, nth: number) {
  await connectAdmin(page, stack);
  await page.getByRole("link", { name: "Calls", exact: true }).first().click();
  await expect(page.getByRole("heading", { name: "Calls", level: 1 })).toBeVisible();
  await page.getByRole("combobox", { name: "Flow", exact: true }).selectOption({ label: "Dental (fake)" });
  const rows = page.getByRole("row").filter({ hasText: "Dental (fake)" });
  await expect(rows).toHaveCount(nth, { timeout: 20_000 });
  await expect(rows.first()).toContainText("Ended", { timeout: 20_000 });
  await rows.first().getByRole("link").click();
  await expect(page.getByRole("heading", { level: 1 })).toContainText("Call ");
}

// The tool calls themselves are mcp.tool.execute spans with no turn id and no tool name (only a
// status), so the page's per-turn timeline cannot show them: count them from the traces API, as
// 30-mcp does.
async function expectToolSpans(page: import("@playwright/test").Page, stack: any, count: number) {
  await page.getByText("Technical trace", { exact: true }).click();
  const id = (await page.locator(".call-figures code").first().innerText()).trim();
  const traces = await api<{ data: { spans: { name: string; attributes?: Record<string, unknown> }[] } }>(stack, `/sessions/${id}/traces`);
  const called = traces.data.spans.filter((sp) => sp.name === "mcp.tool.execute").map((sp) => sp.attributes?.status);
  expect(called).toEqual(Array(count).fill("ok"));
}

test("calls: the new patient call shows its path, step labels, how it ended and its tool-call spans", async ({ page, stack }) => {
  await openLatestCall(page, stack, 1);
  await expect(page.getByText("Front desk → New patient", { exact: true })).toBeVisible();
  await expect(page.getByText("By the flow (New patient)")).toBeVisible();
  await expect(page.getByRole("list", { name: "Transcript" }).getByText(/^New patient · /).first()).toBeVisible();
  await expectToolSpans(page, stack, 3); // register_patient, find_slots, book_appointment
  await step(page, "new patient call detail");
});

test("a returning patient call: handed off by route, MCP tools called, hung up by end_call", async ({ page, stack }) => {
  await connectAdmin(page, stack);
  await openFlowTest(page, "Dental (fake)");
  await startCall(page);
  await turn(page, "transfer to returning patient");
  await expect(stepCard(page, "Returning patient").locator(".step-node")).toHaveAttribute("data-state", "active", { timeout: 15_000 });
  await toolTurn(page, "find_patient", { name: "Maria Lopez" }, /Tool find_patient returned.*Maria/);
  await toolTurn(page, "list_appointments", { patient_id: "p1" });
  await turn(page, "end the call");
  await expect(page.getByTestId("connection-state")).toHaveText(/disconnected/i, { timeout: 20_000 });

  await openLatestCall(page, stack, 2);
  await expect(page.getByText("Front desk → Returning patient", { exact: true })).toBeVisible();
  await expect(page.getByText("By the flow (Returning patient)")).toBeVisible();
  await expectToolSpans(page, stack, 2); // find_patient, list_appointments
  await step(page, "returning patient call detail");
});
