import { test, expect, LIVE } from "../lib/agents-stack";
import { step } from "../lib/evidence";
import { api, connectAdmin, openAgentsList, stage } from "../lib/agents-helpers";

// More agents than one page: the Agents list and the Calls agent filter follow the server's pages.
// Runs last (70-) in the shared agents stack so the extra agents cannot change an earlier spec's list.
test.skip(LIVE, "echo-provider spec");

const name = (n: number) => `E2E Many ${String(n).padStart(3, "0")}`;

test("agent lists reach agent 101, not just the first page", async ({ page, stack }) => {
  const config = { stt: stage({ model: "chirp" }), llm: stage({ model: "m", system_prompt: "You help." }), tts: stage({ voice: "v" }) };
  for (let from = 1; from <= 101; from += 10) {
    await Promise.all(
      Array.from({ length: Math.min(10, 102 - from) }, (_, i) => api(stack, "/agents", { method: "POST", body: JSON.stringify({ name: name(from + i), config }) })),
    );
  }
  await expect
    .poll(async () => (await api<{ data: unknown[] }>(stack, "/agents?limit=100&offset=100")).data.length)
    .toBeGreaterThan(0);

  await connectAdmin(page, stack);
  await openAgentsList(page);
  await page.getByLabel("Filter agents").fill("E2E Many 1");
  // Without `exact`, "E2E Many 10" would match 100 and 101 too.
  await expect(page.getByRole("link", { name: name(100), exact: true })).toBeVisible();
  await expect(page.getByRole("link", { name: name(101), exact: true })).toBeVisible();
  await expect(page.getByText(/Showing the first/)).toHaveCount(0);
  await step(page, "agents list past 100");

  await page.getByRole("link", { name: "Calls", exact: true }).first().click();
  await expect(page.getByRole("option", { name: name(101), exact: true })).toBeAttached();
});
