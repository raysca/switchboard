import { test, expect, LIVE } from "../lib/agents-stack";
import { step } from "../lib/evidence";
import { api, connectAdmin, stage } from "../lib/agents-helpers";

// An agent's test suite, run from the console on the echo providers: free and deterministic. The echo
// reply contains "You said", so one case passes and one (never_say "You said") fails.
test.skip(LIVE, "echo-provider spec");
test.describe.configure({ mode: "serial" });

// Unique per run so reruns and other specs' agents never collide with this one.
const NAME = `E2E Eval Agent ${Date.now().toString(36)}${Math.random().toString(36).slice(2, 6)}`;
const CASES = `name: greets
caller:
  script: ["hello there"]
expect:
  say: ["(?i)hello"]
---
name: never-says-you-said
caller:
  script: ["hello there"]
expect:
  never_say: ["(?i)you said"]
`;

test("a suite runs on echo through the console, fails the broken case and shows in History and the fleet", async ({ page, stack }) => {
  await api(stack, "/agents", {
    method: "POST",
    body: JSON.stringify({ name: NAME, config: { stt: stage({ model: "chirp" }), llm: stage({ model: "m", system_prompt: "You help." }), tts: stage({ voice: "v" }) } }),
  });
  await connectAdmin(page, stack);
  await page.getByRole("link", { name: "Evaluation", exact: true }).first().click();
  const fleet = page.getByRole("table", { name: "Latest suite result per agent" });
  await expect(fleet).toBeVisible();
  // The fleet is paged by name: the agent may be on a later page.
  const link = fleet.getByRole("link", { name: NAME });
  const next = page.getByRole("button", { name: "Next" });
  while (!(await link.isVisible()) && (await next.isEnabled())) await next.click();
  await link.click();
  await expect(page).toHaveURL(/\/evaluation\/[^/]+$/);
  await page.getByRole("tab", { name: "Test suite" }).click();
  await expect(page.getByRole("note").first()).toContainText("Eval calls run this agent's real tools");

  await page.getByLabel("Add cases").fill(CASES);
  await page.getByRole("button", { name: "Add cases" }).click();
  await expect(page.getByRole("table", { name: "Test cases" }).getByRole("row")).toHaveCount(3);

  await page.getByLabel("Models").selectOption("echo");
  await page.getByRole("button", { name: "Run suite" }).click();
  const runs = page.getByRole("table", { name: "Runs" });
  await expect(runs.getByRole("cell", { name: "1 of 2 cases", exact: true })).toBeVisible({ timeout: 60_000 });

  await runs.getByRole("button", { name: /^View/ }).first().click();
  const detail = page.getByRole("region", { name: "Run detail" });
  const failing = detail.getByRole("row").filter({ hasText: "never-says-you-said" });
  await expect(failing).toContainText("Failed");
  await expect(failing).toContainText("never_say:");
  await expect(detail.getByText("correctness: skipped — no judge configured").first()).toBeVisible();
  await step(page, "45-evaluation-run");

  await failing.getByRole("link", { name: "Open call" }).click();
  await expect(page).toHaveURL(/\/sessions\/[^/]+$/);
  await expect(page.locator(".call-turns")).toBeVisible();

  await page.goBack();
  await page.getByRole("tab", { name: "History" }).click();
  await expect(page.getByRole("table", { name: "Finished suite runs" }).getByRole("row")).toHaveCount(2);
  await expect(page.getByRole("table", { name: "Finished suite runs" })).toContainText("v1");

  await page.getByRole("link", { name: "Evaluation", exact: true }).first().click();
  const row = page.getByRole("row").filter({ hasText: NAME });
  while (!(await row.isVisible()) && (await next.isEnabled())) await next.click(); // the row may sit on a later page
  await expect(row).toContainText("50%");
  await expect(row).toContainText("Below threshold");
  // Other agents may be below threshold too: assert the banner exists and counts at least this one.
  await expect(page.getByRole("status").filter({ hasText: /\d+ agents? below/ })).toBeVisible();
});
