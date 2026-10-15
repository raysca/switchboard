import { test, expect, LIVE } from "../lib/flows-stack";
import { step } from "../lib/evidence";
import { api, botReplies, connectAdmin, openFlowTest, sendText, stage, startCall } from "../lib/flows-helpers";

// What a call leaves in the console: who called, how it ended, a median reply, a waterfall per reply, and
// reply times in the Test call panel. Echo providers: free and deterministic.
test.skip(LIVE, "echo-provider spec");
test.describe.configure({ mode: "serial" });

const PLAIN = "E2E Call Records";
const QUIET = "E2E Quiet Records";
const create = (stack: Parameters<typeof api>[0], name: string, extra: object = {}) =>
  api(stack, "/flows", {
    method: "POST",
    body: JSON.stringify({ name, config: { stt: stage({ model: "chirp" }), llm: stage({ model: "m", system_prompt: "You help." }), tts: stage({ voice: "v" }), ...extra } }),
  });

test("a console test call shows reply times, then Calls shows who called and how it ended", async ({ page, stack }) => {
  await create(stack, PLAIN);
  await connectAdmin(page, stack);
  await openFlowTest(page, PLAIN);
  await startCall(page);
  await sendText(page, "Say hello");
  await expect(botReplies(page).first()).toBeVisible();
  // The server's own measurement of that reply, in the panel (it polls the turn rows).
  await expect(page.getByText("Last reply", { exact: true })).toBeVisible({ timeout: 15_000 });
  await expect(page.getByText("Median reply", { exact: true })).toBeVisible();
  await expect(page.getByText(/^Replied in /).first()).toBeVisible();
  await step(page, "reply times in the test call panel");
  await page.getByTestId("disconnect-btn").click();

  await page.getByRole("link", { name: "Calls", exact: true }).first().click();
  const row = page.getByRole("row").nth(1);
  await expect(row).toContainText("Console test");
  await expect(row).toContainText("Ended · Caller hung up", { timeout: 15_000 });
  await expect(page.getByRole("columnheader", { name: "Median reply" })).toBeVisible();
  await step(page, "the call in the list");

  await row.getByRole("link").click();
  await expect(page).toHaveURL(/\/sessions\/[^/]+$/);
  const figures = page.locator(".call-figures");
  await expect(figures.getByText("Console test", { exact: true })).toBeVisible();
  await expect(figures.getByText("Caller hung up", { exact: true })).toBeVisible();
  // The reply's time is on its turn. (The echo providers answer instantly, so every stage may take 0 ms and
  // there is then nothing to draw as a waterfall: the unit tests pin the drawing.)
  await expect(page.locator(".call-turns").getByText(/^Replied in /)).toBeVisible();
  await step(page, "call detail");
});

test("a caller who stays silent is recorded as having gone silent", async ({ page, stack }) => {
  await create(stack, QUIET, { idle: { user_timeout_ms: 1000, max_prompts: 1, prompt_mode: "say" } });
  await connectAdmin(page, stack);
  await openFlowTest(page, QUIET);
  await startCall(page); // nobody speaks: the flow's own wait ends the call
  await expect(page.getByText("user_idle")).toBeVisible({ timeout: 30_000 });
  await page.getByRole("link", { name: "Calls", exact: true }).first().click();
  await expect(page.getByRole("row").nth(1)).toContainText("Ended · Caller went silent", { timeout: 15_000 });
  await step(page, "a silent caller in the list");
});
