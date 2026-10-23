import { test, expect, LIVE } from "../lib/agents-stack";
import { step } from "../lib/evidence";
import { api, botReply, connectAdmin, openCallPage, openAgentTest, sendText, stage, startCall } from "../lib/agents-helpers";

// The call page's "Guide this call": an operator note and a reply request reach a running call held in
// another tab, and the page refreshes the transcript when the call ends. Echo providers only: the echo
// repeats the last user message, which here is the note.
test.skip(LIVE, "echo-provider spec");

const NAME = "E2E Live Notes";
const NOTE = "the caller is a VIP";

test("an operator note and a reply request from the call page reach the running call", async ({ page, stack }) => {
  await api(stack, "/agents", {
    method: "POST",
    body: JSON.stringify({ name: NAME, config: { stt: stage({ model: "chirp" }), llm: stage({ model: "m", system_prompt: "You help." }), tts: stage({ voice: "v" }) } }),
  });
  await connectAdmin(page, stack);
  await openAgentTest(page, NAME);
  await startCall(page);
  await sendText(page, "hello");
  await expect(botReply(page, /hello/)).toBeVisible();
  const sessionId = (await page.getByTestId("session-summary").locator("code").innerText()).trim();

  const ops = await openCallPage(page, stack, sessionId);
  const region = ops.getByRole("region", { name: "Guide this call" });
  await expect(region).toBeVisible();
  await region.getByLabel("Note for the agent").fill(NOTE);
  await region.getByRole("checkbox", { name: "Reply now" }).check();
  await region.getByRole("button", { name: "Send note" }).click();
  await expect(region.getByRole("status")).toHaveText("Note sent.");
  await step(ops, "note sent from the call page");

  await expect(botReply(page, /the caller is a VIP/)).toBeVisible();
  await region.getByRole("button", { name: "Ask for a reply" }).click();
  await expect(region.getByRole("status")).toHaveText("Reply requested.");
  await expect(page.getByTestId("transcript-bot").filter({ hasText: /the caller is a VIP/ })).toHaveCount(2);
  await step(page, "both replies in the live call");

  await page.getByTestId("disconnect-btn").click();
  // Same tab, no reload: the poll sees the call end, then the transcript is fetched once more.
  await expect(region).toBeHidden({ timeout: 15_000 });
  const transcript = ops.getByRole("list", { name: "Transcript" });
  await expect(transcript).toContainText("hello");
  await expect(transcript.getByRole("listitem").filter({ hasText: /the caller is a VIP/ })).not.toHaveCount(0);
  await expect(transcript.locator("li:has(.call-transcript__who--caller) p").filter({ hasText: /^\[note/ })).toHaveCount(0);
  await step(ops, "ended call keeps the replies");
});
