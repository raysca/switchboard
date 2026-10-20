import { test, expect, LIVE } from "../lib/flows-stack";
import { api, connectAdmin, openFlow, openFlowSettings, stage } from "../lib/flows-helpers";

// Per-flow conversation settings (Phase 7b). Each test creates its own flow, so it runs alone with --grep.
test.skip(LIVE, "echo-provider spec");

type Step = { id: string; config: { tts?: { clean_text?: boolean }; failover?: { on?: string[] } } };
type Flow = { id: string; name: string; current_version: { config: { steps: Step[] } } };
const find = async (stack: Parameters<typeof api>[0], name: string) => (await api<{ data: Flow[] }>(stack, "/flows")).data.find((f) => f.name === name)!;

test("text cleaning and fallback rules are flow settings that survive a reload", async ({ page, stack }) => {
  const name = "E2E Call-wide Settings";
  const cfg = (extra: object = {}) => ({ llm: stage({ model: "m", system_prompt: "You help." }), tts: stage({ voice: "v" }), ...extra });
  await api(stack, "/flows", {
    method: "POST",
    body: JSON.stringify({
      name,
      config: {
        start: "front",
        steps: [
          { id: "front", name: "Front", config: { stt: stage({ model: "chirp" }), ...cfg() } },
          { id: "sales", name: "Sales", config: cfg() },
        ],
        routes: [{ id: "e1", from: "front", to: "sales", condition: "wants to buy" }],
      },
    }),
  });
  await connectAdmin(page, stack);
  await openFlow(page, name);
  await openFlowSettings(page);
  await page.locator("details.advanced").getByText("Advanced", { exact: true }).click();
  await page.getByLabel(/Clean text before speaking/).uncheck();
  await page.getByLabel("Rejected requests (bad request, bad key)", { exact: true }).check();
  await page.getByRole("button", { name: "Done" }).click();
  await page.getByRole("button", { name: "Publish", exact: true }).click();
  await page.getByRole("button", { name: "Publish v2" }).click();

  await expect
    .poll(async () => (await find(stack, name)).current_version.config.steps[0].config.tts?.clean_text)
    .toBe(false);
  const [start, other] = (await find(stack, name)).current_version.config.steps;
  expect(start.config.failover?.on).toContain("client_error");
  expect(other.config.tts?.clean_text).toBeUndefined();
  expect(other.config.failover).toBeUndefined();

  await page.reload();
  await connectAdmin(page, stack);
  await openFlow(page, name);
  await openFlowSettings(page);
  await expect(page.locator("details.advanced")).toHaveAttribute("open", "");
  await expect(page.getByLabel(/Clean text before speaking/)).not.toBeChecked();
  await expect(page.getByLabel("Rejected requests (bad request, bad key)", { exact: true })).toBeChecked();
});
