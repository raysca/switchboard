import { test, expect, LIVE } from "../lib/agents-stack";
import { step } from "../lib/evidence";
import { api, botReply, connectAdmin, openAgent, openAgentTest, sendText, stage, startCall } from "../lib/agents-helpers";

// HTTP tools end to end on the echo stack: define a tool in the UI against the test server's REST endpoint
// (cmd/mcptest), try it, give it to an agent, and have a call use it. The secret is a stored one.
test.skip(LIVE, "the HTTP tool cases run against the echo stack only");
test.describe.configure({ mode: "serial" });

const TOOL = "check_order";
const AGENT = "E2E HTTP Tool";

test("define an HTTP tool in the UI and try it", async ({ page, stack }) => {
  await api(stack, "/secrets/e2e-rest", { method: "PUT", body: JSON.stringify({ value: "e2e-mcp-token" }) });
  await connectAdmin(page, stack);
  await page.getByRole("link", { name: "HTTP tools", exact: true }).first().click();
  await expect(page.getByRole("heading", { name: "HTTP tools", level: 1 })).toBeVisible();
  await page.getByRole("link", { name: "New HTTP tool" }).click();

  await page.getByLabel("Name", { exact: true }).fill(TOOL);
  await page.getByLabel("Description").fill("Check an order by its id.");
  await page.getByLabel("URL").fill(`${stack.mcpUrl}/rest/orders/{order_id}`);
  await page.getByLabel("Input schema").fill(JSON.stringify({ type: "object", properties: { order_id: { type: "string" } }, required: ["order_id"] }));
  await page.getByLabel("Secret (optional)", { exact: true }).fill("e2e-rest");
  await page.getByRole("button", { name: "Create tool" }).click();
  await expect(page.getByRole("navigation", { name: "HTTP tools" }).getByText(TOOL)).toBeVisible();
  await step(page, "HTTP tool created");

  const args = page.getByLabel("JSON arguments");
  await args.fill('{"order_id":"A-7"}');
  await page.getByRole("button", { name: "Run test" }).click();
  await expect(page.getByText(/"status": "shipped"/)).toBeVisible();
  await args.fill('{"order_id":"missing"}');
  await page.getByRole("button", { name: "Run test" }).click();
  await expect(page.getByText(/the service answered HTTP 404/)).toBeVisible();
  await step(page, "HTTP tool test call");

  // a refused definition says why
  await page.getByLabel("URL").fill(`http://203.0.113.9/x/{order_id}`);
  await page.getByRole("button", { name: "Save" }).click();
  await expect(page.getByRole("alert")).toContainText(/https|public/);
});

test("an agent uses the tool in a call, and the tool picker shows it chosen", async ({ page, stack }) => {
  const tools = await api<{ data: { id: string; name: string; hash: string }[] }>(stack, "/http-tools");
  const tool = tools.data.find((t) => t.name === TOOL)!;
  await api(stack, "/agents", {
    method: "POST",
    body: JSON.stringify({
      name: AGENT,
      config: { stt: stage({ model: "chirp" }), llm: stage({ model: "m", system_prompt: "You use tools." }), tts: stage({ voice: "v" }) },
      mcp_tools: [{ mcp_server_id: `http:${tool.id}`, remote_name: tool.name, schema_hash: tool.hash, required: true }],
    }),
  });
  await fetch(`${stack.mcpUrl}/_record`, { method: "DELETE" });
  await connectAdmin(page, stack);
  await openAgentTest(page, AGENT);
  await startCall(page);
  await sendText(page, `call tool ${TOOL} {"order_id":"B-2"}`);
  await expect(botReply(page, /shipped/)).toBeVisible();
  const records = (await (await fetch(`${stack.mcpUrl}/_record`)).json()) as { path: string; auth: string }[];
  expect(records.find((r) => r.path === "/rest/orders/B-2")?.auth).toBe("match");
  await step(page, "a call using the HTTP tool");
  await page.getByTestId("disconnect-btn").click();

  // the same tool, chosen, in the agent's Tools tab
  await openAgent(page, AGENT);
  await page.locator(".react-flow__node").first().click();
  await page.getByRole("tab", { name: /Tools/ }).click();
  const group = page.getByRole("group", { name: "HTTP tools" });
  await expect(group.getByRole("checkbox", { name: new RegExp(`^${TOOL}`) })).toBeChecked();
});
