import { test, expect, PROVIDER } from "../lib/agents-stack";
import { step } from "../lib/evidence";
import { api, connectAdmin, loadProviders, providers, sendText, startCall } from "../lib/agents-helpers";

// First run end to end on a fresh stack (the 05- prefix runs it before any spec that creates an agent).
test.describe.configure({ mode: "serial" });
test.use({ viewport: { width: 1440, height: 1040 } });
test.beforeAll(loadProviders);

test("dashboard: a detected provider and no agent is one step from a first call", async ({ page, stack }) => {
  const agents = (await api(stack, "/agents")).data as unknown[];
  test.skip(agents.length > 0, "needs a stack with no agents yet");
  await connectAdmin(page, stack);
  await expect(page.getByRole("heading", { name: "One step to your first call." })).toBeVisible();
  await step(page, "dashboard checklist");
});

test("first run: stack, agent, test call, connect", async ({ page, stack }) => {
  const agents = (await api(stack, "/agents")).data as unknown[];
  test.skip(agents.length > 0, "needs a stack with no agents yet");
  await connectAdmin(page, stack);
  await page.getByRole("link", { name: "Start first run" }).click();

  const name = providers.find((p) => p.id === PROVIDER)!.display_name;
  await expect(page.getByRole("heading", { name: "Choose how your agent hears, thinks and speaks." })).toBeVisible();
  // The stack also has placeholder google and xai keys (for the dentistry demo), and the simulated
  // provider is listed after real ones: choose it, since only echo can take a test call here.
  await page.getByRole("radio", { name }).check();
  await expect(page.getByRole("radio", { name })).toBeChecked();
  await expect(page.locator('input[type="password"]')).toHaveCount(0);
  await step(page, "first run step 1 voice stack");
  await page.getByRole("button", { name: "Continue" }).click();

  await page.getByLabel("Name your agent", { exact: true }).fill("First run agent");
  await step(page, "first run step 2 agent");
  await page.getByRole("button", { name: "Create and call it" }).click();

  await expect(page.getByRole("heading", { name: "Say hello." })).toBeVisible();
  await startCall(page);
  await sendText(page, "hello");
  await step(page, "first run step 3 test call");
  await page.getByTestId("disconnect-btn").click();
  await page.getByRole("button", { name: "Next: connect" }).click();

  await expect(page.getByRole("heading", { name: "Put it in front of callers." })).toBeVisible();
  const created = ((await api(stack, "/agents")).data as { id: string; name: string }[]).find((f) => f.name === "First run agent")!;
  await expect(page.getByLabel("Token request")).toContainText(`"agent_id": "${created.id}"`);
  await step(page, "first run step 4 connect");

  // A reload resumes on the same step (the key is in memory only, so connect again).
  await page.reload();
  await page.getByLabel("Management key").fill(stack.managementKey);
  await page.getByRole("button", { name: "Connect", exact: true }).click();
  await expect(page.getByRole("heading", { name: "Put it in front of callers." })).toBeVisible();
});
