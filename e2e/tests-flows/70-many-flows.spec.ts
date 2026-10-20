import { test, expect, LIVE } from "../lib/flows-stack";
import { step } from "../lib/evidence";
import { api, connectAdmin, openFlowsList, stage } from "../lib/flows-helpers";

// More flows than one page: the Flows list and the Calls flow filter follow the server's pages.
// Runs last (70-) in the shared flows stack so the extra flows cannot change an earlier spec's list.
test.skip(LIVE, "echo-provider spec");

const name = (n: number) => `E2E Many ${String(n).padStart(3, "0")}`;

test("flow lists reach flow 101, not just the first page", async ({ page, stack }) => {
  const config = { stt: stage({ model: "chirp" }), llm: stage({ model: "m", system_prompt: "You help." }), tts: stage({ voice: "v" }) };
  for (let from = 1; from <= 101; from += 10) {
    await Promise.all(
      Array.from({ length: Math.min(10, 102 - from) }, (_, i) => api(stack, "/flows", { method: "POST", body: JSON.stringify({ name: name(from + i), config }) })),
    );
  }
  await expect
    .poll(async () => (await api<{ data: unknown[] }>(stack, "/flows?limit=100&offset=100")).data.length)
    .toBeGreaterThan(0);

  await connectAdmin(page, stack);
  await openFlowsList(page);
  await page.getByLabel("Filter flows").fill("E2E Many 1");
  // Without `exact`, "E2E Many 10" would match 100 and 101 too.
  await expect(page.getByRole("link", { name: name(100), exact: true })).toBeVisible();
  await expect(page.getByRole("link", { name: name(101), exact: true })).toBeVisible();
  await expect(page.getByText(/Showing the first/)).toHaveCount(0);
  await step(page, "flows list past 100");

  await page.getByRole("link", { name: "Calls", exact: true }).first().click();
  await expect(page.getByRole("option", { name: name(101), exact: true })).toBeAttached();
});
