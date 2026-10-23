import { test, expect, LIVE } from "../lib/agents-stack";
import { step } from "../lib/evidence";
import { api, botReply, connectAdmin, openAgent, openAgentTest, sendText, stage, startCall } from "../lib/agents-helpers";

// A test call runs what is on the canvas, not the published version. The echo LLM labels each
// reply with the step's instructions, so which version answered is visible in the transcript.
test.skip(LIVE, "labelled replies are echo-only");
test.describe.configure({ mode: "serial" });

const NAME = "E2E Draft Test";
const body = {
  name: NAME,
  config: { stt: stage({ model: "chirp" }), llm: stage({ model: "m", system_prompt: "PUBLISHED PROMPT" }), tts: stage({ voice: "frontvoice" }) },
};

test("an unpublished change is heard on a test call and stays unpublished", async ({ page, stack }) => {
  await api(stack, "/agents", { method: "POST", body: JSON.stringify(body) });
  await connectAdmin(page, stack);
  await openAgent(page, NAME);
  await page.locator(".react-flow__node").first().click();
  await page.getByLabel("Instructions").fill("DRAFT PROMPT");
  await page.getByRole("button", { name: "Test call" }).click();
  await expect(page.getByRole("note").filter({ hasText: "This call runs your unpublished changes" })).toBeVisible();
  await expect(page.getByText("Testing draft (based on v1)")).toBeVisible();
  await startCall(page);
  await sendText(page, "hello");
  await expect(botReply(page, /DRAFT PROMPT/)).toBeVisible();
  await expect(page.getByTestId("transcript-bot").filter({ hasText: "PUBLISHED PROMPT" })).toHaveCount(0);
  await step(page, "draft test call answered by the draft");
  await page.getByTestId("disconnect-btn").click();

  // Nothing was published.
  const agents = (await api<{ data: { name: string; current_version: { number: number; config: any } }[] }>(stack, "/agents")).data;
  const agent = agents.find((f) => f.name === NAME)!;
  expect(agent.current_version.number).toBe(1);
  expect(JSON.stringify(agent.current_version.config)).toContain("PUBLISHED PROMPT");
  expect(JSON.stringify(agent.current_version.config)).not.toContain("DRAFT PROMPT");

  // The Calls list marks it as a draft.
  await page.getByRole("link", { name: "Calls", exact: true }).first().click();
  await expect(page.getByRole("row").filter({ hasText: NAME }).first().getByText("Draft", { exact: true })).toBeVisible();
  await step(page, "calls list marks the draft call");
});

test("after publishing, the same words are the published version and the call is not a draft", async ({ page, stack }) => {
  await connectAdmin(page, stack);
  await openAgent(page, NAME);
  await page.locator(".react-flow__node").first().click();
  await page.getByLabel("Instructions").fill("DRAFT PROMPT");
  await page.getByRole("button", { name: "Publish", exact: true }).click();
  await page.getByRole("button", { name: "Publish v2" }).click();
  await openAgentTest(page, NAME);
  await expect(page.getByRole("note").filter({ hasText: "unpublished changes" })).toHaveCount(0);
  await startCall(page);
  await sendText(page, "hello");
  await expect(botReply(page, /DRAFT PROMPT/)).toBeVisible();
  await page.getByTestId("disconnect-btn").click();
  await page.getByRole("link", { name: "Calls", exact: true }).first().click();
  const rows = page.getByRole("row").filter({ hasText: NAME });
  await expect(rows).toHaveCount(2); // the draft call from before and this one
  await expect(rows.getByText("Draft", { exact: true })).toHaveCount(1); // only the earlier one
});
