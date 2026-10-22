import { test, expect, LIVE } from "../lib/flows-stack";
import { step } from "../lib/evidence";
import { api, botReply, connectAdmin, openFlow, openFlowTest, sendText, stage, startCall } from "../lib/flows-helpers";

// LLM sampling (temperature, top P, max output tokens): set on the step in the editor, proven to reach the
// model on a draft test call (the echo model, with labelled replies, says what it was given), published,
// read back from the server and shown again after a reload. Echo providers: free and deterministic.
test.skip(LIVE, "echo-provider spec");
test.describe.configure({ mode: "serial" });

const NAME = "E2E LLM Sampling";
type Flow = { name: string; current_version: { number: number; config: { llm?: { temperature?: number; top_p?: number; max_output_tokens?: number } } } };
const FIELDS = { Temperature: "0.3", "Top P": "0.9", "Max output tokens": "200" };
const openAdvanced = async (page: import("@playwright/test").Page) => {
  await page.locator(".react-flow__node").first().click();
  await page.getByRole("tab", { name: /Advanced/ }).click();
};

test("sampling reaches the model on a test call, is published, read back and shown after a reload", async ({ page, stack }) => {
  await api(stack, "/flows", {
    method: "POST",
    body: JSON.stringify({ name: NAME, config: { stt: stage({ model: "chirp" }), llm: stage({ model: "m", system_prompt: "You help." }), tts: stage({ voice: "v" }) } }),
  });
  await connectAdmin(page, stack);
  await openFlow(page, NAME);
  await openAdvanced(page);
  for (const [label, value] of Object.entries(FIELDS)) await page.getByLabel(label, { exact: true }).fill(value);
  await page.getByRole("button", { name: "Explain Max output tokens" }).click();
  await expect(page.getByRole("region", { name: "Explain Max output tokens" })).toContainText("at least 256");
  await step(page, "llm sampling settings");
  await page.getByRole("button", { name: "Done" }).click();

  // The unpublished draft is what the test call runs: the model reports the sampling it was given.
  await openFlowTest(page, NAME);
  await startCall(page);
  await sendText(page, "hello");
  await expect(botReply(page, /temperature=0\.3 top_p=0\.9 max_output_tokens=200/)).toBeVisible({ timeout: 15_000 });
  await step(page, "the model received the sampling");
  await page.getByTestId("disconnect-btn").click();

  await page.getByRole("button", { name: "Publish", exact: true }).click();
  await page.getByRole("button", { name: "Publish v2" }).click();
  const read = async () => {
    const v = (await api<{ data: Flow[] }>(stack, "/flows")).data.find((x) => x.name === NAME)!.current_version;
    return v.number === 2 ? v.config.llm : undefined;
  };
  await expect.poll(read).toMatchObject({ temperature: 0.3, top_p: 0.9, max_output_tokens: 200 });

  await page.reload();
  await connectAdmin(page, stack);
  await openFlow(page, NAME);
  await openAdvanced(page);
  for (const [label, value] of Object.entries(FIELDS)) await expect(page.getByLabel(label, { exact: true })).toHaveValue(value);
});
