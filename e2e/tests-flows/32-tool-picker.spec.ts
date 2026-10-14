import { test, expect, LIVE } from "../lib/flows-stack";
import { step } from "../lib/evidence";
import { api, botReply, connectAdmin, openFlow, sendText, stage, startCall } from "../lib/flows-helpers";
import { lastMCPCall } from "../lib/mcp-helpers";

// Choosing MCP tools in the designer: they are tested as a draft, published with the flow, and removable.
test.skip(LIVE, "the MCP cases run against the echo stack only");
test.describe.configure({ mode: "serial" });

const NAME = "E2E Tool Picker";
type Flow = { name: string; current_version: { number: number; mcp_tools: { remote_name: string }[] } };
const current = async (stack: Parameters<typeof api>[0]) => ((await api<{ data: Flow[] }>(stack, "/flows")).data.find((f) => f.name === NAME)!).current_version;

test("tools chosen in the Tools tab are tested as a draft, then published with the flow", async ({ page, stack }) => {
  const server = await api<{ data: { id: string } }>(stack, "/mcp-servers", {
    method: "POST",
    body: JSON.stringify({ name: "Picker ok", endpoint: `${stack.mcpUrl}/ok`, auth_env: "MCP_TOKEN_TEST", enabled: true }),
  });
  await api(stack, `/mcp-servers/${server.data.id}/validate`, { method: "POST" });
  await api(stack, "/flows", {
    method: "POST",
    body: JSON.stringify({ name: NAME, config: { stt: stage({ model: "chirp" }), llm: stage({ model: "m", system_prompt: "You use tools." }), tts: stage({ voice: "v" }) } }),
  });
  await connectAdmin(page, stack);
  await openFlow(page, NAME);
  await page.locator(".react-flow__node").first().click();
  await page.getByRole("tab", { name: /Tools/ }).click();
  await expect(page.getByRole("group", { name: "Picker ok" })).toBeVisible();
  await page.getByRole("group", { name: "Picker ok" }).getByRole("checkbox", { name: /^lookup_order/ }).check();
  await step(page, "tool chosen in the designer");

  // Test call: the draft uses the tool although nothing is published yet.
  await page.getByRole("button", { name: "Test call" }).click();
  await expect(page.getByRole("note").filter({ hasText: "This call runs your unpublished changes" })).toBeVisible();
  await startCall(page);
  await sendText(page, 'call tool lookup_order {"order_id":"42"}');
  await expect(botReply(page, /shipped/)).toBeVisible();
  expect((await lastMCPCall(stack, "lookup_order"))?.auth).toBe("match");
  await page.getByTestId("disconnect-btn").click();
  expect((await current(stack)).mcp_tools).toEqual([]); // still nothing published
  await step(page, "draft call used the unpublished tool");

  // Publish: one new version carrying the tool.
  await page.getByRole("button", { name: "Publish", exact: true }).click();
  await expect(page.getByRole("dialog")).toContainText("Added tool");
  await page.getByRole("button", { name: "Publish v2" }).click();
  await expect.poll(async () => (await current(stack)).mcp_tools.map((t) => t.remote_name)).toEqual(["lookup_order"]);
  expect((await current(stack)).number).toBe(2);
});

test("a chosen tool can be taken off again, and the next version has none", async ({ page, stack }) => {
  await connectAdmin(page, stack);
  await openFlow(page, NAME);
  await page.locator(".react-flow__node").first().click();
  await page.getByRole("tab", { name: /Tools/ }).click();
  const box = page.getByRole("group", { name: "Picker ok" }).getByRole("checkbox", { name: /^lookup_order/ });
  await expect(box).toBeChecked();
  await box.uncheck();
  await page.getByRole("button", { name: "Publish", exact: true }).click();
  await expect(page.getByRole("dialog")).toContainText("Removed tool");
  await page.getByRole("button", { name: "Publish v3" }).click();
  await expect.poll(async () => (await current(stack)).number).toBe(3);
  expect((await current(stack)).mcp_tools).toEqual([]);
  await step(page, "tool removed and published");
});
