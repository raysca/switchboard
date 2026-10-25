import { test, expect, LIVE } from "../lib/agents-stack";
import { step } from "../lib/evidence";
import { api, connectAdmin, stage } from "../lib/agents-helpers";

// An agent's test suite, run from the console on the echo providers: free and deterministic. The echo
// reply contains "You said", so one case passes and one (never_say "You said") fails.
test.skip(LIVE, "echo-provider spec");
test.describe.configure({ mode: "serial" });

const NAME = "E2E Eval Agent";
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
  const agent = ((await api(stack, "/agents")).data as { id: string; name: string }[]).find((a) => a.name === NAME)!;
  await connectAdmin(page, stack);
  await page.getByRole("link", { name: "Evaluation", exact: true }).first().click();
  // The fleet page lists only agents that have a finished suite, so a new agent's suite has no link yet
  // (a console gap): enter it in-app by route, keeping the key held in memory.
  await page.evaluate((id) => {
    history.pushState({}, "", `/evaluation/${id}`);
    dispatchEvent(new PopStateEvent("popstate"));
  }, agent.id);
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
  await expect(row).toContainText("50%");
  await expect(row).toContainText("Below threshold");
  await expect(page.getByText("1 agent below its suite threshold")).toBeVisible();
});
