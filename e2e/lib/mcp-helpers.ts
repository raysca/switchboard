import type { Page } from "@playwright/test";
import { expect, type Stack } from "./flows-stack";
import { api, stage } from "./flows-helpers";

// MCP integration against the local test MCP servers (cmd/mcptest, started by the global setup).
export async function connectMCPServer(page: Page, name: string, endpoint: string, tokenVariable = "") {
  await page.getByRole("link", { name: "MCP tools", exact: true }).first().click();
  await page.getByRole("button", { name: "Connect server" }).click();
  const dialog = page.getByRole("dialog");
  await dialog.getByLabel("Name").fill(name);
  await dialog.getByLabel("Endpoint URL").fill(endpoint);
  if (tokenVariable) await dialog.getByLabel("Token variable (optional)").fill(tokenVariable);
  await dialog.getByRole("button", { name: "Connect", exact: true }).click();
  await expect(page.getByRole("heading", { name, level: 2 })).toBeVisible();
  await page.getByRole("button", { name: "Sync tools" }).click();
}


// Registers an MCP server on the test servers, syncs it, and creates a flow with the named
// tools attached (each tool's schema hash comes from the discovery).
export async function mcpFlow(stack: Stack, flow: string, path: string, tools: string[], authEnv = "") {
  const server = await api<{ data: { id: string } }>(stack, "/mcp-servers", {
    method: "POST",
    body: JSON.stringify({ name: `${flow} server`, endpoint: `${stack.mcpUrl}${path}`, auth_env: authEnv, enabled: true }),
  });
  const id = server.data.id;
  await api(stack, `/mcp-servers/${id}/validate`, { method: "POST" });
  const detail = await api<{ data: { discovery: { tools: { remote_name: string; schema_hash: string }[] } } }>(stack, `/mcp-servers/${id}`);
  const hash = (name: string) => detail.data.discovery.tools.find((t) => t.remote_name === name)!.schema_hash;
  await api(stack, "/flows", {
    method: "POST",
    body: JSON.stringify({
      name: flow,
      config: { stt: stage({ model: "chirp" }), llm: stage({ model: "m", system_prompt: "You use tools." }), tts: stage({ voice: "v" }) },
      mcp_tools: tools.map((t) => ({ mcp_server_id: id, remote_name: t, schema_hash: hash(t), required: false })),
    }),
  });
}

export type MCPRecord = { tool?: string; auth: string; arguments?: unknown; cancelled: boolean };
export const mcpRecord = async (stack: Stack): Promise<MCPRecord[]> => (await fetch(`${stack.mcpUrl}/_record`)).json();
export const lastMCPCall = async (stack: Stack, tool: string) => (await mcpRecord(stack)).filter((r) => r.tool === tool).pop();

// A text turn sent the instant a reply arrives can be lost (a tool-independent issue tracked
// separately); the MCP tests pace their turns so they test MCP, not that.
export const pace = (page: Page) => page.waitForTimeout(1000);
