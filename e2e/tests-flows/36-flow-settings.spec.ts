import type { Page } from "@playwright/test";
import { test, expect, LIVE } from "../lib/flows-stack";
import { step } from "../lib/evidence";
import { api, botReplies, connectAdmin, openFlow, openFlowSettings, openFlowsList, sendText, stage, startCall } from "../lib/flows-helpers";

// The flow settings the editor used to hide: summarisation details, the late-transcript wait, the failover
// policy and background tools. Set in the editor, published, read back from the server, reopened, and run
// on a test call. Echo providers: free and deterministic.
test.skip(LIVE, "echo-provider spec");
test.describe.configure({ mode: "serial" });

const NAME = "E2E Flow Settings";

// Every Explain button on the open panel opens a region with its default and range, and closes again.
async function explainAll(page: Page, atLeast: number) {
  const buttons = page.getByRole("button", { name: /^Explain / });
  const n = await buttons.count();
  expect(n).toBeGreaterThanOrEqual(atLeast);
  for (let i = 0; i < n; i++) {
    const button = buttons.nth(i);
    const name = (await button.getAttribute("aria-label")) ?? "";
    await button.scrollIntoViewIfNeeded();
    await button.click();
    const region = page.getByRole("region", { name, exact: true });
    await expect(region).toContainText("Default and range");
    await button.click();
    await expect(region).toHaveCount(0);
  }
}
type Config = { context?: { max_tokens?: number; summarization?: { trigger_tokens?: number; keep_turns?: number; prompt?: string } }; stt_p99_ms?: number; failover?: { on: string[] }; async_tools?: string[] };
type Flow = { name: string; current_version: { number: number; config: Config & { steps?: { config: Config }[] } } };
const current = async (stack: Parameters<typeof api>[0]) => (await api<{ data: Flow[] }>(stack, "/flows")).data.find((f) => f.name === NAME)!.current_version;
const startConfig = (c: Flow["current_version"]["config"]): Config => c.steps?.[0]?.config ?? c;

test("settings set in the editor are published, read back, reopened and run on a call", async ({ page, stack }) => {
  const server = await api<{ data: { id: string } }>(stack, "/mcp-servers", {
    method: "POST",
    body: JSON.stringify({ name: "Settings ok", endpoint: `${stack.mcpUrl}/ok`, auth_env: "MCP_TOKEN_TEST", enabled: true }),
  });
  await api(stack, `/mcp-servers/${server.data.id}/validate`, { method: "POST" });
  await api(stack, "/flows", {
    method: "POST",
    body: JSON.stringify({ name: NAME, config: { stt: stage({ model: "chirp" }), llm: stage({ model: "m", system_prompt: "You help." }), tts: stage({ voice: "v" }) } }),
  });
  await connectAdmin(page, stack);
  await openFlow(page, NAME);

  // Flow settings: the advanced group is closed until opened, and each obscure setting explains itself.
  await openFlowSettings(page);
  const advanced = page.locator("details.advanced");
  await expect(advanced).not.toHaveAttribute("open", "");
  await advanced.getByText("Advanced", { exact: true }).click();
  await page.getByLabel("Summarise older turns", { exact: true }).check();
  await page.getByLabel("Max tokens", { exact: true }).fill("1000");
  await page.getByLabel("Summarise when history reaches (tokens)", { exact: true }).fill("500");
  await page.getByLabel("Wait for a late transcript (ms)", { exact: true }).fill("800");
  await page.getByLabel("Keep the latest turns", { exact: true }).fill("2");
  await page.locator("#flow-summary-prompt").fill("Keep names and amounts.");
  await page.getByRole("button", { name: "Explain Wait for a late transcript" }).click();
  await expect(page.getByRole("region", { name: "Explain Wait for a late transcript" })).toContainText("Default and range");
  await page.keyboard.press("Escape");
  await expect(page.getByRole("region", { name: "Explain Wait for a late transcript" })).toHaveCount(0);
  await explainAll(page, 10); // every obscure setting in Flow settings explains itself
  await step(page, "advanced flow settings");
  await page.getByRole("button", { name: "Done" }).click();

  // The step: the failover policy and a background tool.
  await page.locator(".react-flow__node").first().click();
  await page.getByRole("tab", { name: /Advanced/ }).click();
  await page.getByRole("checkbox", { name: "Rejected requests (bad request, bad key)" }).check();
  await explainAll(page, 4); // fallbacks, failover, tool calls at once, text cleaning
  await page.getByRole("tab", { name: /Tools/ }).click();
  await page.getByRole("group", { name: "Settings ok" }).getByRole("checkbox", { name: /^lookup_order/ }).check();
  await page.getByRole("checkbox", { name: "Background: lookup_order" }).check();
  await explainAll(page, 1);
  await step(page, "failover and a background tool");

  await page.getByRole("button", { name: "Publish", exact: true }).click();
  await expect(page.getByRole("dialog")).toContainText("Flow settings: background tools changed");
  await page.getByRole("button", { name: "Publish v2" }).click();
  await expect.poll(async () => (await current(stack)).number).toBe(2);
  const cfg = startConfig((await current(stack)).config);
  expect(cfg.context?.summarization?.trigger_tokens).toBe(500);
  expect(cfg.context?.summarization?.keep_turns).toBe(2);
  expect(cfg.context?.summarization?.prompt).toBe("Keep names and amounts.");
  expect(cfg.context?.max_tokens).toBe(1000);
  expect(cfg.stt_p99_ms).toBe(800);
  expect(cfg.failover?.on).toHaveLength(4);
  expect(cfg.async_tools).toEqual([`${server.data.id}/lookup_order`]);

  // The published flow, carrying every new key, takes a call.
  await page.getByRole("button", { name: "Test call" }).click();
  await startCall(page);
  await sendText(page, "Say hello");
  await expect(botReplies(page).first()).toBeVisible();
  await page.getByTestId("disconnect-btn").click();

  // Reopened in the app: Advanced is already open and shows what is stored; a trigger above Max tokens blocks Publish.
  await openFlowsList(page);
  await openFlow(page, NAME);
  await openFlowSettings(page);
  await expect(page.locator("details.advanced")).toHaveAttribute("open", "");
  await expect(page.getByLabel("Wait for a late transcript (ms)", { exact: true })).toHaveValue("800");
  await expect(page.getByLabel("Summarise when history reaches (tokens)", { exact: true })).toHaveValue("500");
  await expect(page.getByLabel("Keep the latest turns", { exact: true })).toHaveValue("2");
  await expect(page.locator("#flow-summary-prompt")).toHaveValue("Keep names and amounts.");
  await page.getByLabel("Summarise when history reaches (tokens)", { exact: true }).fill("1500");
  await expect(page.getByRole("note")).toContainText("Summarise-at must be below Max tokens (1000).");
  await page.getByRole("button", { name: "Done" }).click();
  await page.getByRole("button", { name: "Publish", exact: true }).click();
  await expect(page.getByRole("dialog")).toContainText("Summarise-at must be below Max tokens (1000).");
  await expect(page.getByRole("button", { name: "Publish v3" })).toBeDisabled();
  await step(page, "an invalid trigger blocks publishing");
});
