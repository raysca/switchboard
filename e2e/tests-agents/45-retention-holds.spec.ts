import { test, expect, LIVE } from "../lib/agents-stack";
import { step } from "../lib/evidence";
import { api, botReplies, connectAdmin, openAgentTest, sendText, stage, startCall } from "../lib/agents-helpers";

// Holding a call keeps it out of retention, an erasure removes it for good, and the Server page lists both.
// Echo providers: free and deterministic.
test.skip(LIVE, "echo-provider spec");
test.describe.configure({ mode: "serial" });

const NAME = "E2E Retention Holds";

test("a call can be held, found by the filter, released and erased from its page", async ({ page, stack }) => {
  await api(stack, "/agents", {
    method: "POST",
    body: JSON.stringify({ name: NAME, config: { stt: stage({ model: "chirp" }), llm: stage({ model: "m", system_prompt: "You help." }), tts: stage({ voice: "v" }) } }),
  });
  await connectAdmin(page, stack);
  await openAgentTest(page, NAME);
  await startCall(page);
  await sendText(page, "Say hello");
  await expect(botReplies(page).first()).toBeVisible();
  await page.getByTestId("disconnect-btn").click();

  await page.getByRole("link", { name: "Calls", exact: true }).first().click();
  const row = page.getByRole("row").nth(1);
  await expect(row).toContainText("Ended · Caller hung up", { timeout: 15_000 });
  await row.getByRole("link").click();
  await expect(page).toHaveURL(/\/sessions\/[^/]+$/);
  const callName = (await page.locator("h1.call-id").textContent())!.trim();

  // Hold it, with a reason.
  const retention = page.getByRole("heading", { name: "Retention", level: 2 }).locator("..");
  await expect(retention).toContainText("follows the retention policy");
  await retention.getByRole("button", { name: "Hold this call…" }).click();
  await page.getByLabel("Reason (optional)").fill("Dispute #4821");
  await step(page, "holding a call");
  await page.getByRole("button", { name: "Hold call" }).click();
  await expect(retention.getByText("On hold", { exact: true })).toBeVisible();
  await expect(retention).toContainText("Dispute #4821");
  await expect(retention.getByRole("button", { name: "Erase this call…" })).toBeDisabled();
  await step(page, "a call on hold");

  // The list marks it and the filter finds it.
  await page.getByRole("link", { name: "Calls", exact: true }).first().click();
  await expect(page.getByRole("row").nth(1).locator(".chip", { hasText: "On hold" })).toBeVisible();
  await page.getByRole("button", { name: "On hold", exact: true }).click();
  await expect(page).toHaveURL(/held=1/);
  await expect(page.getByRole("button", { name: "On hold", exact: true })).toHaveAttribute("aria-pressed", "true");
  await expect(page.getByRole("row")).toHaveCount(2); // header + the held call
  await step(page, "the calls on hold");

  // The Server page counts it and lists the hold.
  await page.getByRole("link", { name: "Server", exact: true }).first().click();
  const card = page.getByRole("region", { name: "Data retention" });
  await expect(card).toContainText("Calls on hold1");
  await expect(card.getByRole("region", { name: "Recent holds and erasures" })).toContainText(`Held ${callName}`);
  await step(page, "the server page lists the hold");

  // Release, then erase: the dialog wants the call's name typed.
  await page.getByRole("link", { name: "Calls", exact: true }).first().click();
  await page.getByRole("row").nth(1).getByRole("link").click();
  await retention.getByRole("button", { name: "Release hold" }).click();
  await expect(retention).toContainText("follows the retention policy");
  await retention.getByRole("button", { name: "Erase this call…" }).click();
  const erase = page.getByRole("button", { name: "Erase call" });
  await expect(erase).toBeDisabled();
  await page.getByLabel(/^Type/).fill(callName);
  await step(page, "erasing a call");
  await erase.click();
  await expect(page.getByRole("heading", { name: "Calls", level: 1 })).toBeVisible();
  await expect(page.getByText("No calls yet.").or(page.getByText("No calls match these filters."))).toBeVisible();

  await page.getByRole("link", { name: "Server", exact: true }).first().click();
  await expect(page.getByRole("region", { name: "Data retention" })).toContainText(`Erased ${callName}`);
  await step(page, "the erasure in the retention history");
});
