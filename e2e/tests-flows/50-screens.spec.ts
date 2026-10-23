import { test, expect } from "../lib/flows-stack";
import { step } from "../lib/evidence";
import { api, connectAdmin, stage } from "../lib/flows-helpers";

// Walks canvas boards 3 to 12: a numbered screenshot of each, and the structure that matters. Runs in its own order after the other specs, on a stack that already has flows, so the empty page is checked on a fresh list through the UI.
test.describe.configure({ mode: "serial" });
test.use({ viewport: { width: 1440, height: 1040 } });

const front = {
  name: "Front desk",
  description: "Answers the main line, verifies callers and routes to billing or support.",
  config: {
    start: "reception",
    steps: [
      { id: "reception", name: "Reception", config: { stt: stage({ model: "chirp" }), llm: stage({ model: "m", system_prompt: "You are Reception. Answer in one short sentence." }), tts: stage({ voice: "frontvoice" }) } },
      { id: "billing", name: "Billing", config: { llm: stage({ model: "m", system_prompt: "You are Billing. Answer in one short sentence." }), tts: stage({ voice: "frontvoice" }) } },
      { id: "support", name: "Support", config: { llm: stage({ model: "m", system_prompt: "You are Support. Answer in one short sentence." }), tts: stage({ voice: "frontvoice" }) } },
    ],
    handoffs: [
      { id: "e1", from: "reception", to: "billing", condition: "asks about an invoice" },
      { id: "e2", from: "reception", to: "support", condition: "reports a fault" },
    ],
  },
};

test("boards 3 to 12", async ({ page, stack }) => {
  await connectAdmin(page, stack);
  await page.getByRole("link", { name: "Flows", exact: true }).first().click();
  // Earlier specs leave flows behind: archive them all so board 3 (the empty page) can be shown.
  const existing = (await api(stack, "/agents")).data as { id: string }[];
  for (const a of existing) await api(stack, `/agents/${a.id}/archive`, { method: "POST" });
  await page.reload();
  await page.getByLabel("Management key").fill(stack.managementKey);
  await page.getByRole("button", { name: "Connect", exact: true }).click();
  await expect(page.getByRole("heading", { name: "No flows yet." })).toBeVisible();
  await step(page, "board 3 flows none yet");

  await api(stack, "/agents", { method: "POST", body: JSON.stringify(front) });
  const second = await api(stack, "/agents", { method: "POST", body: JSON.stringify({ name: "Appointments", description: "Books, moves and cancels engineer visits.", config: { stt: stage({ model: "chirp" }), llm: stage({ model: "m", system_prompt: "You book visits." }), tts: stage({ voice: "frontvoice" }) } }) });
  await api(stack, `/agents/${second.data.id}`, { method: "PUT", body: JSON.stringify({ enabled: false }) });
  await page.reload();
  await page.getByLabel("Management key").fill(stack.managementKey);
  await page.getByRole("button", { name: "Connect", exact: true }).click();
  await expect(page.getByRole("link", { name: "Front desk", exact: true })).toBeVisible();
  await step(page, "board 4 flows list");

  await page.getByRole("button", { name: "New flow" }).click();
  await page.getByLabel("Name").fill("After-hours line");
  await step(page, "board 5 new flow dialog");
  await page.getByRole("button", { name: "Create and open editor" }).click();
  await expect(page.getByRole("heading", { name: "After-hours line", level: 1 })).toBeVisible();
  await step(page, "board 6 editor blank");
  await page.getByRole("button", { name: "Add first step" }).click();
  await step(page, "board 7 first step panel");

  await page.getByRole("link", { name: "Flows", exact: true }).first().click();
  await page.getByRole("link", { name: "Front desk", exact: true }).click();
  await expect(page.getByRole("heading", { name: "Front desk", level: 1 })).toBeVisible();
  await page.getByRole("button", { name: "Add step" }).click();
  await page.getByRole("button", { name: "Done" }).click();
  // board 8: the editor uses the width of the screen, and the steps are readable (not zoomed out to specks)
  expect((await page.locator(".flow-editor__stage").boundingBox())!.width).toBeGreaterThan(1000);
  // The flow refits a beat after the panel closes.
  await expect.poll(async () => (await page.locator(".react-flow__node").first().boundingBox())!.width, { timeout: 5000 }).toBeGreaterThan(150);
  await step(page, "board 8 editor full flow one step unfinished");
  await page.getByRole("button", { name: "Flow settings" }).click();
  await step(page, "board 9 flow settings");
  await page.getByRole("button", { name: "Close" }).last().click().catch(() => {});

  // Remove the unfinished step so the flow can be tested and published.
  await page.getByRole("link", { name: "Flows", exact: true }).first().click();
  await page.getByRole("link", { name: "Front desk", exact: true }).click();
  await page.getByRole("button", { name: "Test call" }).click();
  await page.getByTestId("start-voice-test-btn").click();
  await expect(page.getByTestId("connection-state")).toHaveText(/ready/i, { timeout: 30_000 });
  await page.getByTestId("mute-btn").click();
  await page.getByRole("textbox", { name: "Message" }).fill("hello");
  await page.getByTestId("send-text-btn").click();
  await expect(page.getByTestId("transcript-user").first()).toBeVisible();
  await expect(page.getByText(/^Testing v\d+$/)).toBeVisible();
  await step(page, "board 10 test call live");
  await page.getByTestId("disconnect-btn").click();

  await page.getByRole("button", { name: "Flow settings" }).click();
  await page.getByLabel("Flow name").fill("Front desk line");
  await page.getByRole("button", { name: "Publish", exact: true }).click();
  await step(page, "board 11 publish dialog");
  await page.getByRole("button", { name: /^Publish v/ }).click();
  await expect(page.getByRole("status", { name: "Publish confirmation" })).toBeVisible();
  await expect(page.getByRole("status", { name: "Publish confirmation" })).toContainText("Published v2.");
  await step(page, "board 12 published banner");
});
