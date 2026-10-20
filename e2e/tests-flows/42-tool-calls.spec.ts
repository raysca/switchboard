import { test, expect, LIVE } from "../lib/flows-stack";
import { step } from "../lib/evidence";
import { botReply, connectAdmin, openFlowTest, sendText, startCall } from "../lib/flows-helpers";
import { mcpFlow, pace } from "../lib/mcp-helpers";

// A tool call seen live in the test call's RTVI event log, then on the call's page: its turn and its technical trace.
// The local test MCP server (cmd/mcptest) and the echo providers: free and deterministic.
test.skip(LIVE, "the MCP cases run against the echo stack only");

const NAME = "E2E Tool Records";

test("a tool call shows in the test call's events and on the call's page", async ({ page, stack }) => {
  await mcpFlow(stack, NAME, "/ok", ["lookup_order"], "MCP_TOKEN_TEST");
  await connectAdmin(page, stack);
  await openFlowTest(page, NAME);
  await startCall(page);
  await sendText(page, 'call tool lookup_order {"order_id":"42"}');
  await expect(botReply(page, /shipped/)).toBeVisible();

  // The client received the call's three events (newest first in the log).
  await page.getByTestId("toggle-debug-btn").click();
  await page.getByLabel("Filter events").fill("llm-function-call");
  const events = ["llm-function-call-stopped", "llm-function-call-in-progress", "llm-function-call-started"];
  await expect(page.getByLabel("RTVI event log").locator("code")).toHaveText(events);
  // Ordering barrier: after one more answered turn the log must still hold exactly those three.
  await pace(page); // the stack's guard against a text turn sent while the last reply commits (mcp-helpers.ts)
  await sendText(page, "barrier");
  await expect(botReply(page, /You said: barrier/)).toBeVisible();
  await expect(page.getByLabel("RTVI event log").locator("code")).toHaveText(events);
  // The spoken text follows the reply's audio as bot-tts-text (the wait is the assertion).
  await page.getByLabel("Filter events").fill("bot-tts-text");
  await expect(page.getByLabel("RTVI event log").locator("code").first()).toHaveText("bot-tts-text");
  await page.getByLabel("Filter events").fill("llm-function-call");
  await step(page, "tool call events in the test call");

  await page.getByTestId("disconnect-btn").click();
  await page.getByTestId("view-trace-link").click();
  await expect(page).toHaveURL(/\/sessions\/[^/]+$/);
  // The note sits on the turn that made the call (the one that answered "shipped"), not on the barrier turn.
  const turns = page.locator(".call-turns li");
  const callTurn = turns.filter({ hasText: "Tool lookup_order · " });
  await expect(callTurn).toHaveCount(1, { timeout: 15_000 });
  await expect(callTurn.getByText(/^Tool lookup_order · \d+ ms$/)).toBeVisible();
  await expect(callTurn).toContainText("shipped");
  const barrierTurn = turns.filter({ hasText: "barrier" });
  await expect(barrierTurn).toHaveCount(1);
  await expect(barrierTurn).not.toContainText("lookup_order");
  await step(page, "the tool call on its turn");

  await page.getByText("Technical trace").click();
  await page.getByRole("button", { name: /Show traces \(.*1 tool calls/ }).click();
  const item = page.getByTestId("tool-call-item");
  await expect(item).toContainText("lookup_order");
  await expect(item.getByRole("status")).toHaveText("succeeded");
  // Never the arguments or the result.
  await expect(item).not.toContainText("order_id");
  await expect(item).not.toContainText("shipped");
  // (The echo model repeats the tool result as its reply, so the turn text itself may carry it: only the note is the tool call.)
  await expect(page.locator(".call-turns__note").filter({ hasText: "Tool" })).toHaveText(/^Tool lookup_order · \d+ ms$/);
  await step(page, "the tool call in the technical trace");
});
