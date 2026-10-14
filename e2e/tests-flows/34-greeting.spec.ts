import { test, expect, LIVE } from "../lib/flows-stack";
import { step } from "../lib/evidence";
import { BUY, api, botReply, botReplies, connectAdmin, openFlow, openFlowTest, sendText, stage, startCall, stepCard } from "../lib/flows-helpers";

// A step's greeting is spoken word for word: the start step's when the call connects, another step's when
// a route hands the call to it. Draft test calls use the unpublished greetings.
test.skip(LIVE, "labelled replies and instant voices are echo-only");
test.describe.configure({ mode: "serial" });

const NAME = "E2E Greeting";
const OPENING = "Hello from the front desk.";
const SALES = "Sales here, how can I help?";
type Flow = { name: string; current_version: { number: number; config: { steps?: { config: { greeting?: string } }[] } } };
const current = async (stack: Parameters<typeof api>[0]) => (await api<{ data: Flow[] }>(stack, "/flows")).data.find((f) => f.name === NAME)!.current_version;

test("greetings typed in the designer are spoken on a draft test call, opening and handoff", async ({ page, stack }) => {
  await api(stack, "/flows", {
    method: "POST",
    body: JSON.stringify({
      name: NAME,
      config: {
        start: "front",
        steps: [
          { id: "front", name: "Front", config: { stt: stage({ model: "chirp" }), llm: stage({ model: "m", system_prompt: "FRONT" }), tts: stage({ voice: "frontvoice" }) } },
          { id: "sales", name: "Sales", config: { llm: stage({ model: "m", system_prompt: "SALESPROMPT" }), tts: stage({ voice: "salesvoice" }) } },
        ],
        routes: [{ id: "e1", from: "front", to: "sales", condition: "The caller wants to buy." }],
      },
    }),
  });
  await connectAdmin(page, stack);
  await openFlow(page, NAME);
  await stepCard(page, "Front").click();
  await page.getByLabel("Greeting").fill(OPENING);
  await expect(page.getByText("Said word for word as soon as the call connects.")).toBeVisible();
  await stepCard(page, "Sales").click();
  await page.getByLabel("Greeting").fill(SALES);
  await expect(page.getByText("when a call is transferred here")).toBeVisible();
  await step(page, "greetings typed in the designer");

  await page.getByRole("button", { name: "Test call" }).click();
  await expect(page.getByRole("note").filter({ hasText: "This call runs your unpublished changes" })).toBeVisible();
  await startCall(page);
  // Nothing was typed: the agent spoke first.
  await expect(botReply(page, new RegExp(OPENING))).toBeVisible();
  await sendText(page, BUY);
  await expect(botReply(page, new RegExp(SALES))).toBeVisible();
  await page.waitForTimeout(1000);
  await expect(botReplies(page).filter({ hasText: "[SALESPROMPT" })).toHaveCount(0); // the model did not greet as well
  await step(page, "opening and handoff greetings on a draft call");
  await page.getByTestId("disconnect-btn").click();
  expect(JSON.stringify((await current(stack)).config)).not.toContain("greeting"); // nothing is published yet

  await page.getByRole("button", { name: "Publish", exact: true }).click();
  await expect(page.getByRole("dialog")).toContainText("greeting changed");
  await page.getByRole("button", { name: "Publish v2" }).click();
  await expect.poll(async () => (await current(stack)).number).toBe(2);
  expect((await current(stack)).config.steps![0].config.greeting).toBe(OPENING);
  expect((await current(stack)).config.steps![1].config.greeting).toBe(SALES);
});

test("the published flow greets, and clearing the greeting makes the next call silent", async ({ page, stack }) => {
  await connectAdmin(page, stack);
  await openFlowTest(page, NAME);
  await startCall(page);
  await expect(botReply(page, new RegExp(OPENING))).toBeVisible();
  await page.getByTestId("disconnect-btn").click();

  await stepCard(page, "Front").click();
  await page.getByLabel("Greeting").fill("");
  await page.getByRole("button", { name: "Publish", exact: true }).click();
  await page.getByRole("button", { name: "Publish v3" }).click();
  await expect.poll(async () => (await current(stack)).number).toBe(3);
  await openFlowTest(page, NAME);
  await startCall(page);
  await page.waitForTimeout(1500);
  await expect(botReplies(page)).toHaveCount(0); // the caller speaks first again
  await step(page, "no greeting, no first words");
});
