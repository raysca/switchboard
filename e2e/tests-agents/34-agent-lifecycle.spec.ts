import { test, expect, LIVE } from "../lib/agents-stack";
import { step } from "../lib/evidence";
import { api, connectAdmin, openAgent, stage } from "../lib/agents-helpers";

// Version history, restore, archive and unarchive in the console, against the real server.
test.skip(LIVE, "the lifecycle cases run against the echo stack only");

const NAME = "E2E Lifecycle";
const config = (prompt: string) => ({ stt: stage({ model: "chirp" }), llm: stage({ model: "m", system_prompt: prompt }), tts: stage({ voice: "v" }) });

test("restore an earlier version from the history, then archive and unarchive the agent", async ({ page, stack }) => {
  const created = await api<{ data: { id: string } }>(stack, "/agents", { method: "POST", body: JSON.stringify({ name: NAME, config: config("first prompt") }) });
  const id = created.data.id;
  await api(stack, `/agents/${id}`, { method: "PUT", body: JSON.stringify({ config: config("second prompt") }) });

  await connectAdmin(page, stack);
  await openAgent(page, NAME);
  await page.getByRole("button", { name: "Agent settings" }).click();
  await page.getByRole("button", { name: "Version history" }).click();
  const items = page.locator(".version-history__list li");
  await expect(items).toHaveCount(2);
  await expect(items.first()).toContainText("v2 · current");
  await step(page, "version history");
  await page.getByRole("button", { name: "Restore v1…" }).click();
  await page.getByRole("button", { name: "Restore v1", exact: true }).click();

  // the server published v1's content as v3
  await expect
    .poll(async () => (await api<{ data: { current_version: { number: number; config: { llm: { system_prompt: string } } } } }>(stack, `/agents/${id}`)).data.current_version.number)
    .toBe(3);
  const now = await api<{ data: { current_version: { config: { llm: { system_prompt: string } } } } }>(stack, `/agents/${id}`);
  expect(now.data.current_version.config.llm.system_prompt).toBe("first prompt");
  const versions = await api<{ data: { number: number; current: boolean }[] }>(stack, `/agents/${id}/versions`);
  expect(versions.data.map((v) => v.number)).toEqual([3, 2, 1]);

  // archive through the API, then bring the agent back from the list
  await api(stack, `/agents/${id}/archive`, { method: "POST" });
  await page.getByRole("link", { name: "Agents", exact: true }).first().click();
  await page.getByRole("button", { name: /Show archived agents/ }).click();
  await step(page, "archived agents");
  await page.getByRole("button", { name: `Unarchive ${NAME}` }).click();
  await expect(page.getByRole("link", { name: NAME, exact: true })).toBeVisible();
  const back = await api<{ data: { archived: boolean; enabled: boolean } }>(stack, `/agents/${id}`);
  expect(back.data).toMatchObject({ archived: false, enabled: false });
});
