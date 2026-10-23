import { test, expect, LIVE } from "../lib/agents-stack";
import { step } from "../lib/evidence";
import { api, botReply, connectAdmin, openAgentTest, sendText, stage, startCall } from "../lib/agents-helpers";
import { connectMCPServer, lastMCPCall, mcpAgent, mcpRecord, pace } from "../lib/mcp-helpers";

// MCP in the admin UI: the MCP tools page and the local test MCP servers (cmd/mcptest, started by
// the global setup). The first test needs a stack with no MCP server connected; fake stack only.
test.describe.configure({ mode: "serial" });

test("mcp tools page: with no server connected it says how to connect one, and the dialog opens and closes", async ({ page, stack }) => {
  await connectAdmin(page, stack);
  await page.getByRole("link", { name: "MCP tools", exact: true }).first().click();
  await expect(page.getByRole("heading", { name: "MCP tools", level: 1 })).toBeVisible();
  await expect(page.getByRole("heading", { name: "No MCP servers yet." })).toBeVisible();
  await page.getByRole("button", { name: "Connect server" }).first().click();
  const dialog = page.getByRole("dialog", { name: "Connect server" });
  await expect(dialog).toBeVisible();
  // The cursor is already in the name field.
  await expect(dialog.getByLabel("Name")).toBeFocused();
  await step(page, "connect server dialog");
  await page.keyboard.press("Escape");
  await expect(dialog).toHaveCount(0);
});

test("mcp tools page: servers, their tools and a changed schema, with the API faked (the stack cannot reach a public MCP host)", async ({ page, stack }) => {
  const server = (id: string, over: object = {}) => ({ id, name: id, description: "", endpoint: `https://mcp.example.com/${id}`, transport: "streamable-http", validation_state: "healthy", latest_discovery_id: "d", auth_env: "MCP_TOKEN_BILLING", auth_env_set: true, enabled: true, last_connected_at: new Date(Date.now() - 4 * 60_000).toISOString(), last_discovered_at: new Date(Date.now() - 4 * 60_000).toISOString(), ...over });
  const schema = { type: "object", properties: { account_id: { type: "string" }, amount_pence: { type: "integer" } } };
  const tools = [
    { remote_name: "latest_invoice", description: "Return the caller's most recent invoice.", input_schema: schema, schema_hash: "31f0aaaa11" },
    { remote_name: "send_pay_link", description: "Send a one-time payment link by SMS or email.", input_schema: schema, schema_hash: "c9d00000bb" },
  ];
  const agent = {
    id: "f1", name: "Front desk", description: "", enabled: true, archived: false, created_at: "", updated_at: "",
    current_version: { id: "v", number: 3, schema_version: 1, config: {}, config_hash: "h", created_at: "", mcp_tools: [{ mcp_server_id: "billing", remote_name: "send_pay_link", schema_hash: "7a3e0000aa", required: true }] },
    readiness: { state: "needs_tool_review", reasons: ["required_tool_unavailable"], tool_dependencies: [{ mcp_server_id: "billing", remote_name: "send_pay_link", required: true, state: "schema_drift", selected_schema_hash: "7a3e0000aa", current_schema_hash: "c9d00000bb" }] },
  };
  await page.route("**/api/v1/mcp-servers", (r) => r.fulfill({ json: { data: [server("billing"), server("crm", { auth_env: "", auth_env_set: false }), server("network", { validation_state: "connection_failed", last_discovered_at: null })] } }));
  await page.route("**/api/v1/mcp-servers/*", (r) => {
    const id = r.request().url().split("/").pop()!;
    return r.fulfill({ json: { data: { ...server(id), discovery: { id: "d", discovery_hash: "x", created_at: "", tools: id === "billing" ? tools : [] } } } });
  });
  await page.route("**/api/v1/agents?*", (r) => r.fulfill({ json: { data: [agent] } }));
  await connectAdmin(page, stack);
  await page.getByRole("link", { name: "MCP tools", exact: true }).first().click();
  await expect(page.getByRole("heading", { name: "billing", level: 2 })).toBeVisible();
  await expect(page.getByRole("link", { name: /^billing/ })).toContainText("1 schema changed");
  await expect(page.getByRole("link", { name: /^network/ })).toContainText("Unreachable");
  await expect(page.getByRole("heading", { name: "send_pay_link changed its input schema" })).toBeVisible();
  await expect(page.getByRole("button", { name: "Accept new schema for Front desk" })).toBeVisible();
  await step(page, "mcp tools page");
});


test("mcp: healthy, failing and unreachable servers show their state", async ({ page, stack }) => {
  test.skip(LIVE, "the MCP cases run against the fake stack only");
  await connectAdmin(page, stack);
  const status = page.locator(".mcp-detail__status");

  await connectMCPServer(page, "Orders", `${stack.mcpUrl}/ok`, "MCP_TOKEN_TEST");
  await expect(status).toContainText("Connected");
  await expect(page.getByText("set on the server", { exact: false }).first()).toBeVisible();
  await expect(page.getByText("lookup_order").first()).toBeVisible();

  await connectMCPServer(page, "Locked", `${stack.mcpUrl}/fail/auth`);
  await expect(status).toContainText("Authentication failed");

  await connectMCPServer(page, "Gone", "http://127.0.0.1:1/mcp");
  await expect(status).toContainText("Unreachable");
  await step(page, "mcp server states");
});


test("mcp: an agent calls an MCP tool during a test call", async ({ page, stack }) => {
  test.skip(LIVE, "the MCP cases run against the fake stack only");
  await mcpAgent(stack, "E2E Orders", "/ok", ["lookup_order"], "MCP_TOKEN_TEST");
  await connectAdmin(page, stack);
  await openAgentTest(page, "E2E Orders");
  await startCall(page);
  await sendText(page, 'call tool lookup_order {"order_id":"42"}');
  await expect(botReply(page, /shipped/)).toBeVisible();
  await step(page, "mcp tool answered during a call");

  const call = await lastMCPCall(stack, "lookup_order");
  expect(call?.auth).toBe("match");
  expect(call?.arguments).toEqual({ order_id: "42" });
  await page.getByTestId("disconnect-btn").click();
});

test("mcp: each kind of tool failure is answered and the call goes on", async ({ page, stack }) => {
  test.skip(LIVE, "the MCP cases run against the fake stack only");
  const tools = ["tool_error", "protocol_error", "drop", "too_big"];
  await mcpAgent(stack, "E2E Failing Tools", "/fail/tools", tools);
  await connectAdmin(page, stack);
  await openAgentTest(page, "E2E Failing Tools");
  await startCall(page);
  for (const tool of tools) {
    await sendText(page, `call tool ${tool}`);
    await expect(botReply(page, new RegExp(`Tool ${tool} failed`))).toBeVisible();
    await pace(page);
    await sendText(page, `hello after ${tool}`);
    await expect(botReply(page, new RegExp(`hello after ${tool}`))).toBeVisible();
    await pace(page);
  }
  await step(page, "mcp tool failures answered");
  // Each failure reached the test server (none was refused before calling it).
  const seen = new Set((await mcpRecord(stack)).map((r) => r.tool));
  for (const tool of tools) expect(seen.has(tool)).toBe(true);

  const sessionId = (await page.getByTestId("session-summary").locator("code").innerText()).trim();
  await expect
    .poll(async () => {
      const t = await api<{ data: { spans: { name: string }[] } }>(stack, `/sessions/${sessionId}/traces`);
      return t.data.spans.filter((sp) => sp.name === "mcp.tool.execute").length;
    })
    .toBeGreaterThanOrEqual(tools.length);
  await page.getByTestId("disconnect-btn").click();
});

test("mcp: a slow tool answers under the timeout and is abandoned over it", async ({ page, stack }) => {
  test.skip(LIVE, "the MCP cases run against the fake stack only");
  await mcpAgent(stack, "E2E Slow Tool", "/slow", ["slow_lookup"]);
  await connectAdmin(page, stack);
  await openAgentTest(page, "E2E Slow Tool");
  await startCall(page);

  await sendText(page, 'call tool slow_lookup {"delay_ms":300}');
  await expect(botReply(page, /Tool slow_lookup returned/)).toBeVisible();
  await pace(page);

  // The stack's mcp.timeout is 2 s: a 6 s tool is abandoned and the caller hears so.
  const began = Date.now();
  await sendText(page, 'call tool slow_lookup {"delay_ms":6000}');
  await expect(botReply(page, /Tool slow_lookup failed/)).toBeVisible({ timeout: 5_000 });
  expect(Date.now() - began).toBeLessThan(4_500);
  await expect.poll(async () => (await lastMCPCall(stack, "slow_lookup"))?.cancelled).toBe(true);
  await step(page, "slow tool abandoned at the timeout");

  await pace(page);
  await sendText(page, "still there");
  await expect(botReply(page, /still there/)).toBeVisible();
  await page.getByTestId("disconnect-btn").click();
});

// Barge-in during a slow MCP tool is tested in Go (server/mcp_call_e2e_test.go
// TestMCPCallBargeInCancelsSlowTool): in this fake stack the microphone's speech is heard by the
// VAD but the fake STT produces no words, so no user turn starts and nothing interrupts.

test("mcp: editing a failing server's endpoint to a healthy one connects it, and an agent that uses it is ready", async ({ page, stack }) => {
  test.skip(LIVE, "the MCP cases run against the fake stack only");
  await connectAdmin(page, stack);
  const status = page.locator(".mcp-detail__status");
  await connectMCPServer(page, "Edited Orders", `${stack.mcpUrl}/fail/list`, "MCP_TOKEN_TEST");
  await expect(status).not.toContainText("Connected");

  await page.getByRole("button", { name: "Edit", exact: true }).click();
  const dialog = page.getByRole("dialog");
  await dialog.getByLabel("Endpoint URL").fill(`${stack.mcpUrl}/ok`);
  await dialog.getByRole("button", { name: "Save", exact: true }).click();
  await expect(dialog).toHaveCount(0);
  // The edit re-syncs by itself: no Sync tools click, no new server.
  await expect(status).toContainText("Connected");
  await expect(page.getByRole("heading", { name: "Edited Orders", level: 2 })).toBeVisible();
  await expect(page.getByText("lookup_order").first()).toBeVisible();
  await step(page, "edited server connected");

  const servers = await api<{ data: { id: string; name: string }[] }>(stack, "/mcp-servers");
  const id = servers.data.find((s) => s.name === "Edited Orders")!.id;
  const detail = await api<{ data: { discovery: { tools: { remote_name: string; schema_hash: string }[] } } }>(stack, `/mcp-servers/${id}`);
  const hash = detail.data.discovery.tools.find((t) => t.remote_name === "lookup_order")!.schema_hash;
  const made = await api<{ data: { readiness: { state: string } } }>(stack, "/agents", {
    method: "POST",
    body: JSON.stringify({
      name: "E2E Edited Orders",
      config: { stt: stage({ model: "chirp" }), llm: stage({ model: "m", system_prompt: "You use tools." }), tts: stage({ voice: "v" }) },
      mcp_tools: [{ mcp_server_id: id, remote_name: "lookup_order", schema_hash: hash, required: true }],
    }),
  });
  expect(made.data.readiness.state).toBe("ready");
});

test("mcp: Try it on a tool that errors shows the tool's own message", async ({ page, stack }) => {
  test.skip(LIVE, "the MCP cases run against the fake stack only");
  await connectAdmin(page, stack);
  await connectMCPServer(page, "Failing Tools", `${stack.mcpUrl}/fail/tools`);
  await page.getByRole("button", { name: "Try it for tool_error" }).click();
  await page.getByRole("button", { name: "Run tool" }).click();
  await expect(page.getByRole("alert").filter({ hasText: "The tool reported an error: order system unavailable" })).toBeVisible();
  await step(page, "try it shows the tool error message");
});
