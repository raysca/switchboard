import { describe, expect, it, vi } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, Route, Routes, useLocation } from "react-router";
import { MCPView } from "./MCPView";
import { ApiError, type FlowDTO, type ApiClient, type MCPServerDTO, type MCPServerDetailDTO } from "../../api/client";

const server = (over: Partial<MCPServerDTO> = {}): MCPServerDTO => ({
  id: "billing",
  name: "billing",
  description: "Invoices and payment links",
  endpoint: "https://mcp.example.com/billing",
  transport: "streamable-http",
  validation_state: "healthy",
  latest_discovery_id: "d1",
  auth_env: "MCP_TOKEN_BILLING",
  auth_env_set: true,
  enabled: true,
  last_connected_at: "2026-10-12T14:56:00Z",
  last_discovered_at: "2026-10-12T14:56:00Z",
  last_checked_at: "2026-10-12T14:56:00Z",
  ...over,
});
const tool = (name: string, hash: string, description = `${name} description`) => ({ remote_name: name, description, input_schema: { type: "object", properties: { id: { type: "string" } } }, schema_hash: hash });
const detail = (over: Partial<MCPServerDetailDTO> = {}): MCPServerDetailDTO => ({
  ...server(),
  discovery: { id: "d1", discovery_hash: "dh", created_at: "", tools: [tool("latest_invoice", "31f0aaaa11"), tool("send_pay_link", "c9d00000bb")] },
  ...over,
});
const crm = server({ id: "crm", name: "crm", auth_env: "", auth_env_set: false, last_discovered_at: "2026-10-12T14:00:00Z" });
const flow = (id: string, name: string, tools: { server: string; name: string; hash: string; required: boolean }[], deps: object[] = [], archived = false): FlowDTO => ({
  id,
  name,
  description: "",
  enabled: true,
  archived,
  current_version: { id: `${id}-v`, number: 3, schema_version: 1, config: {}, config_hash: "h", created_at: "", mcp_tools: tools.map((t) => ({ mcp_server_id: t.server, remote_name: t.name, schema_hash: t.hash, required: t.required })) },
  readiness: { state: "ready", reasons: [], tool_dependencies: deps as never },
  created_at: "",
  updated_at: "",
});
const drifted = flow("f1", "Front desk", [{ server: "billing", name: "send_pay_link", hash: "7a3e0000aa", required: true }], [{ mcp_server_id: "billing", remote_name: "send_pay_link", required: true, state: "schema_drift", selected_schema_hash: "7a3e0000aa", current_schema_hash: "c9d00000bb" }]);
const steady = flow("f2", "Support triage", [{ server: "billing", name: "latest_invoice", hash: "31f0aaaa11", required: false }], [{ mcp_server_id: "billing", remote_name: "latest_invoice", required: false, state: "ready", selected_schema_hash: "31f0aaaa11", current_schema_hash: "31f0aaaa11" }]);

function Where() {
  const l = useLocation();
  return <p data-testid="where">{l.pathname}</p>;
}
function setup(client: Partial<ApiClient> = {}, path = "/mcp-servers", key: string | null = "k") {
  const full = {
    getMCPServers: vi.fn().mockResolvedValue([server(), crm]),
    getMCPServer: vi.fn().mockImplementation(async (id: string) => (id === "crm" ? detail({ ...crm, discovery: { id: "d2", discovery_hash: "x", created_at: "", tools: [tool("notes", "n1")] } }) : detail())),
    getFlows: vi.fn().mockResolvedValue([drifted, steady]),
    createMCPServer: vi.fn().mockResolvedValue(server({ id: "new", name: "new" })),
    deleteMCPServer: vi.fn().mockResolvedValue({ deleted: true }),
    enableMCPServer: vi.fn().mockResolvedValue(server()),
    disableMCPServer: vi.fn().mockResolvedValue(server({ enabled: false })),
    discoverMCPServer: vi.fn().mockResolvedValue(server()),
    testMCPTool: vi.fn().mockResolvedValue({ execution_id: "exec-9999", mcp_server_id: "billing", tool_name: "latest_invoice", status: "completed", duration_ms: 12, result: { ok: true } }),
    getFlow: vi.fn().mockResolvedValue(drifted),
    updateFlow: vi.fn().mockResolvedValue({}),
    ...client,
  } as unknown as ApiClient;
  render(
    <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
      <MemoryRouter initialEntries={[path]}>
        <Routes>
          <Route
            path="/mcp-servers/:serverId?"
            element={
              <>
                <MCPView client={full} keyValue={key} />
                <Where />
              </>
            }
          />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>,
  );
  return { client: full, user: userEvent.setup() };
}
const serverLink = (name: string) => screen.getByRole("link", { name: new RegExp(`^${name}`) });

describe("MCPView", () => {
  it("asks for a management key first", () => {
    setup({}, "/mcp-servers", null);
    expect(screen.getByText(/Enter a management key/)).toBeVisible();
  });

  it("says how to connect a first server, and opens the dialog", async () => {
    const { user } = setup({ getMCPServers: vi.fn().mockResolvedValue([]) });
    expect(await screen.findByRole("heading", { name: "No MCP servers yet." })).toBeVisible();
    await user.click(screen.getAllByRole("button", { name: "Connect server" })[0]);
    expect(screen.getByRole("dialog", { name: "Connect server" })).toBeVisible();
  });

  it("lists servers with the server's own states in words, tool counts, and when they last synced", async () => {
    setup({
      getMCPServers: vi.fn().mockResolvedValue([server(), crm, server({ id: "net", name: "network", validation_state: "connection_failed", last_connected_at: "2026-10-12T12:00:00Z", last_discovered_at: null }), server({ id: "off", name: "legacy", enabled: false })]),
      getMCPServer: vi.fn().mockImplementation(async (id: string) => detail({ id, name: id })),
    });
    await screen.findByRole("heading", { name: "billing", level: 2 });
    expect(within(serverLink("billing")).getByText("2 tools")).toBeVisible();
    expect(within(serverLink("crm")).getByText(/^Synced /)).toBeVisible();
    const net = within(serverLink("network"));
    expect(net.getByText(/^Unreachable · last ok /)).toBeVisible();
    expect(net.getByText(/^Unreachable/).closest("a")!.querySelector('.lamp[data-tone="bad"]')).not.toBeNull();
    expect(within(serverLink("legacy")).getByText("Disabled")).toBeVisible();
  });

  it("opens the first server by default, another from the URL, and says so for an unknown one", async () => {
    setup({}, "/mcp-servers/crm");
    expect(await screen.findByRole("heading", { name: "crm", level: 2 })).toBeVisible();
    expect(serverLink("crm")).toHaveAttribute("aria-current", "page");
  });

  it("opens the first server when no server is in the URL", async () => {
    setup({});
    expect(await screen.findByRole("heading", { name: "billing", level: 2 })).toBeVisible();
  });

  it("says a server is not found, with a way back", async () => {
    setup({}, "/mcp-servers/nope");
    expect(await screen.findByText("That MCP server was not found.")).toBeVisible();
    expect(screen.getByRole("link", { name: "Back to MCP tools" })).toHaveAttribute("href", "/mcp-servers");
  });

  it("shows the endpoint, the token variable and whether it is set, and which flows use the server", async () => {
    setup({});
    const panel = (await screen.findByRole("heading", { name: "billing", level: 2 })).closest("section") as HTMLElement;
    expect(within(panel).getByText("https://mcp.example.com/billing")).toBeVisible();
    expect(within(panel).getByText("MCP_TOKEN_BILLING")).toBeVisible();
    expect(within(panel).getByText(/set on the server/)).toBeVisible();
    const used = within(panel).getByText("Used by", { selector: "dt" }).closest("div") as HTMLElement;
    expect(within(used).getAllByRole("link").map((a) => a.textContent)).toEqual(["Front desk", "Support triage"]);
  });

  it("names a token variable that is not set", async () => {
    setup({ getMCPServer: vi.fn().mockResolvedValue(detail({ auth_env_set: false })) });
    expect((await screen.findAllByText(/not set on the server/)).length).toBeGreaterThan(0);
  });

  it("shows a missing token variable as the status text, in the list and the detail", async () => {
    setup({ getFlows: vi.fn().mockResolvedValue([]), getMCPServers: vi.fn().mockResolvedValue([server({ auth_env_set: false })]), getMCPServer: vi.fn().mockResolvedValue(detail({ auth_env_set: false })) });
    expect(await within(await screen.findByRole("link", { name: /^billing/ })).findByText(/^Token variable MCP_TOKEN_BILLING is not set on the server/)).toBeVisible();
    expect(screen.getAllByText(/Token variable MCP_TOKEN_BILLING is not set on the server/).length).toBeGreaterThan(1);
  });

  it("shows when the server was last checked and how often it checks by itself", async () => {
    const cfg = (s: number) => ({ getServerConfig: vi.fn().mockResolvedValue({ profile: "production", mcp_sync_interval_seconds: s }) });
    const old = vi.spyOn(Date, "now").mockReturnValue(Date.parse("2026-10-12T15:00:00Z"));
    setup({ ...cfg(300), getMCPServer: vi.fn().mockResolvedValue(detail({ validation_state: "connection_failed", last_connected_at: "2026-10-09T14:56:00Z" })) });
    expect(await screen.findByText("Last checked 4 min ago")).toBeVisible();
    expect(await screen.findByText("Checked automatically every 5 minutes")).toBeVisible();
    old.mockRestore();
  });

  it("says automatic checks are off, and which addresses are refused", async () => {
    setup({ getServerConfig: vi.fn().mockResolvedValue({ profile: "production", mcp_sync_interval_seconds: 0 }) });
    expect(await screen.findByText("Automatic checks are off")).toBeVisible();
    expect(screen.getByText(/refused in production/)).toBeVisible();
  });

  it("says a development server allows loopback", async () => {
    setup({ getServerConfig: vi.fn().mockResolvedValue({ profile: "development", mcp_sync_interval_seconds: 60 }) });
    expect(await screen.findByText(/this development server allows loopback/)).toBeVisible();
    expect(screen.getByText("Checked automatically every 1 minute")).toBeVisible();
  });

  it("edits a server in place: prefilled, only changed fields sent, the returned state shown", async () => {
    // After the save the server's own GET (re-read by the edit) returns the saved state, as the real one does.
    const saved = server({ name: "billing2", validation_state: "connection_failed" });
    let done = false;
    const updateMCPServer = vi.fn().mockImplementation(async () => ((done = true), saved));
    const getMCPServers = vi.fn().mockImplementation(async () => (done ? [saved, crm] : [server(), crm]));
    const getMCPServer = vi.fn().mockImplementation(async () => (done ? detail({ ...saved, discovery: { id: "d2", discovery_hash: "x", created_at: "", tools: [tool("fresh_tool", "f1")] } }) : detail()));
    const { user } = setup({ updateMCPServer, getMCPServers, getMCPServer });
    await user.click(await screen.findByRole("button", { name: "Edit" }));
    const name = screen.getByLabelText("Name");
    expect(name).toHaveValue("billing");
    expect(screen.getByLabelText("Endpoint URL")).toHaveValue("https://mcp.example.com/billing");
    await user.clear(name);
    await user.type(name, "billing2");
    await user.click(screen.getByRole("button", { name: "Save" }));
    expect(updateMCPServer).toHaveBeenCalledWith("billing", { name: "billing2" });
    expect(await screen.findByRole("heading", { name: "billing2", level: 2 })).toBeVisible();
    expect(await screen.findByText("fresh_tool")).toBeVisible(); // the tools the edit's re-sync found are re-read
    expect(screen.getAllByText(/^Unreachable/).length).toBeGreaterThan(0);
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it("clears the token variable with an empty string", async () => {
    const updateMCPServer = vi.fn().mockResolvedValue(server({ auth_env: "" }));
    const { user } = setup({ updateMCPServer });
    await user.click(await screen.findByRole("button", { name: "Edit" }));
    await user.clear(screen.getByLabelText("Token variable (optional)"));
    await user.click(screen.getByRole("button", { name: "Save" }));
    expect(updateMCPServer).toHaveBeenCalledWith("billing", { auth_env: "" });
  });

  it("explains a refused edit by its reason", async () => {
    const updateMCPServer = vi.fn().mockRejectedValue(new ApiError("MCP server configuration is invalid", "invalid_mcp_server", 400, "r1", { reason: "destination_denied" }));
    const { user } = setup({ updateMCPServer });
    await user.click(await screen.findByRole("button", { name: "Edit" }));
    await user.type(screen.getByLabelText("Description (optional)"), "x");
    await user.click(screen.getByRole("button", { name: "Save" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("That address is not allowed: MCP servers must be public HTTPS addresses.");
  });

  it("gives the same sentence when connecting is refused", async () => {
    const createMCPServer = vi.fn().mockRejectedValue(new ApiError("MCP server configuration is invalid", "invalid_mcp_server", 400, "r1", { reason: "invalid_auth_env" }));
    const { user } = setup({ createMCPServer });
    await user.click((await screen.findAllByRole("button", { name: "Connect server" }))[0]);
    await user.type(screen.getByLabelText("Name"), "x");
    await user.type(screen.getByLabelText("Endpoint URL"), "https://a.example.com");
    await user.click(screen.getByRole("button", { name: "Connect" }));
    expect(await screen.findByRole("alert")).toHaveTextContent(/token variable/i);
  });

  it("lists the tools with what the model sees, the schema and how many flows use each", async () => {
    setup({});
    const table = await screen.findByRole("table");
    const row = within(table).getByText("latest_invoice").closest("tr") as HTMLElement;
    expect(within(row).getByText("latest_invoice description")).toBeVisible();
    expect(within(row).getByText("31f0aaaa")).toBeVisible();
    expect(within(row).getByText("1")).toBeVisible();
  });

  it("shows a changed schema in the list, the table and a review panel", async () => {
    setup({});
    await screen.findByRole("heading", { name: "billing", level: 2 });
    expect(within(serverLink("billing")).getByText("1 schema changed")).toBeVisible();
    expect(screen.getByRole("heading", { name: "send_pay_link changed its input schema", level: 3 })).toBeVisible();
    const row = within(screen.getByRole("table")).getByText("send_pay_link").closest("tr") as HTMLElement;
    expect(within(row).getByText("changed")).toBeVisible();
  });

  it("accepts a changed schema for one flow through the dialog", async () => {
    const { client, user } = setup({});
    await user.click(await screen.findByRole("button", { name: "Accept new schema for Front desk" }));
    await user.click(screen.getByRole("button", { name: "Publish new version" }));
    expect(client.updateFlow).toHaveBeenCalledWith("f1", { config: {}, mcp_tools: [{ mcp_server_id: "billing", remote_name: "send_pay_link", schema_hash: "c9d00000bb", required: true }] });
  });

  it("toggles a tool's schema, and opens the test console", async () => {
    const { client, user } = setup({});
    await user.click(await screen.findByRole("button", { name: "Schema for latest_invoice" }));
    // The review panel for the changed tool also shows a schema: look inside the tools table.
    const table = screen.getByRole("table");
    expect(within(table).getByText(/"properties"/)).toBeVisible();
    await user.click(screen.getByRole("button", { name: "Hide schema for latest_invoice" }));
    expect(within(table).queryByText(/"properties"/)).toBeNull();
    await user.click(screen.getByRole("button", { name: "Try it for latest_invoice" }));
    await user.click(screen.getByRole("button", { name: "Run tool" }));
    expect(client.testMCPTool).toHaveBeenCalledWith("billing", "latest_invoice", {});
  });

  it("says when a server has no tools yet", async () => {
    setup({ getMCPServer: vi.fn().mockResolvedValue(detail({ discovery: null })) });
    expect(await screen.findByText("No tools discovered")).toBeVisible();
  });

  it("syncs, disables and enables with the right calls", async () => {
    const { client, user } = setup({});
    await user.click(await screen.findByRole("button", { name: "Sync tools" }));
    expect(client.discoverMCPServer).toHaveBeenCalledWith("billing");
    await user.click(screen.getByRole("button", { name: "Disable" }));
    expect(client.disableMCPServer).toHaveBeenCalledWith("billing");
  });

  it("offers Enable for a disabled server", async () => {
    const { client, user } = setup({ getMCPServer: vi.fn().mockResolvedValue(detail({ enabled: false })) });
    await user.click(await screen.findByRole("button", { name: "Enable" }));
    expect(client.enableMCPServer).toHaveBeenCalledWith("billing");
  });

  it("shows a failed action with its request id", async () => {
    const { user } = setup({ discoverMCPServer: vi.fn().mockRejectedValue(new ApiError("not found", "not_found", 404, "req-1")) });
    await user.click(await screen.findByRole("button", { name: "Sync tools" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("not found (Request ID: req-1)");
  });

  it("connects a server and opens it", async () => {
    const { client, user } = setup({});
    await screen.findByRole("heading", { name: "billing", level: 2 });
    await user.click(screen.getAllByRole("button", { name: "Connect server" })[0]);
    await user.type(screen.getByLabelText("Name"), "new");
    await user.type(screen.getByLabelText("Endpoint URL"), "https://mcp.example.com/new");
    await user.click(screen.getByRole("button", { name: "Connect" }));
    await waitFor(() => expect(client.createMCPServer).toHaveBeenCalledWith(expect.objectContaining({ name: "new", endpoint: "https://mcp.example.com/new", transport: "streamable-http" })));
    await waitFor(() => expect(screen.getByTestId("where")).toHaveTextContent("/mcp-servers/new"));
  });

  it("shows the server's refusal inside the connect dialog", async () => {
    const { user } = setup({ createMCPServer: vi.fn().mockRejectedValue(new ApiError("MCP server configuration is invalid", "invalid_mcp_server", 400, "req-2")) });
    await screen.findByRole("heading", { name: "billing", level: 2 });
    await user.click(screen.getAllByRole("button", { name: "Connect server" })[0]);
    await user.type(screen.getByLabelText("Name"), "x");
    await user.type(screen.getByLabelText("Endpoint URL"), "http://10.0.0.1");
    await user.click(screen.getByRole("button", { name: "Connect" }));
    expect(await within(screen.getByRole("dialog")).findByRole("alert")).toHaveTextContent("MCP server configuration is invalid (Request ID: req-2)");
  });

  it("deletes only after asking, then returns to the list", async () => {
    const { client, user } = setup({});
    await user.click(await screen.findByRole("button", { name: "Delete" }));
    expect(client.deleteMCPServer).not.toHaveBeenCalled();
    await user.click(screen.getByRole("button", { name: "Delete server" }));
    await waitFor(() => expect(client.deleteMCPServer).toHaveBeenCalledWith("billing"));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  });

  it("when delete is refused because the server is in use, names the flows and detaches only on a second confirmation", async () => {
    const inUse = new ApiError("MCP server is referenced by flows or active sessions", "mcp_server_in_use", 409, "req-3");
    const deleteMCPServer = vi.fn().mockRejectedValueOnce(inUse).mockResolvedValue({ deleted: true });
    const { user } = setup({ deleteMCPServer });
    await user.click(await screen.findByRole("button", { name: "Delete" }));
    await user.click(screen.getByRole("button", { name: "Delete server" }));
    const dialog = await screen.findByRole("dialog", { name: "billing is still in use" });
    expect(within(dialog).getByRole("link", { name: "Front desk" })).toBeVisible();
    expect(deleteMCPServer).toHaveBeenCalledTimes(1);
    await user.click(within(dialog).getByRole("button", { name: "Detach and delete" }));
    await waitFor(() => expect(deleteMCPServer).toHaveBeenLastCalledWith("billing", true));
  });

  it("still lists and opens servers when the flow list fails, saying usage is unknown", async () => {
    setup({ getFlows: vi.fn().mockRejectedValue(new ApiError("down", "internal", 500, "r")) });
    const panel = (await screen.findByRole("heading", { name: "billing", level: 2 })).closest("section") as HTMLElement;
    expect(within(panel).getByText("The flows could not be loaded, so which flows use this server, and whether any schema changed, is unknown.")).toBeVisible();
    expect(within(within(panel).getByRole("table")).getAllByText("—").length).toBeGreaterThan(0);
    expect(screen.queryByRole("heading", { name: /changed its input schema/ })).toBeNull();
  });

  it("keeps the list when one server's detail fails to load", async () => {
    setup({ getMCPServer: vi.fn().mockRejectedValue(new ApiError("down", "internal", 500, "r")) });
    expect(await screen.findByRole("alert")).toHaveTextContent("down");
    expect(serverLink("billing")).toBeVisible();
  });

  it("shows a load failure of the server list, with a retry", async () => {
    setup({ getMCPServers: vi.fn().mockRejectedValue(new ApiError("down", "internal", 500, "r")) });
    expect(await screen.findByRole("alert")).toHaveTextContent("down");
    expect(screen.getByRole("button", { name: "Try again" })).toBeVisible();
  });

  it("does not call the flow list a failure while it is still loading", async () => {
    setup({ getFlows: vi.fn().mockReturnValue(new Promise(() => {})) });
    const panel = (await screen.findByRole("heading", { name: "billing", level: 2 })).closest("section") as HTMLElement;
    expect(within(panel).queryByText(/could not be loaded/)).toBeNull();
  });

  it("says counts are a floor, and drift may be incomplete, when the flow list reached the API's page size", async () => {
    const filler = Array.from({ length: 99 }, (_, i) => flow(`x${i}`, `Flow ${i}`, []));
    setup({ getFlows: vi.fn().mockResolvedValue([drifted, ...filler]) });
    const table = await screen.findByRole("table");
    const row = within(table).getByText("send_pay_link").closest("tr") as HTMLElement;
    expect(within(row).getByText("1+")).toBeVisible();
    expect(screen.getByText("Only the first 100 flows were checked, so some changed schemas may not be shown.")).toBeVisible();
  });

  it("looks at the flows again after a sync, so a new schema shows up", async () => {
    const { client, user } = setup({});
    await screen.findByRole("heading", { name: "billing", level: 2 });
    await user.click(screen.getByRole("button", { name: "Sync tools" }));
    await waitFor(() => expect(client.getFlows).toHaveBeenCalledTimes(2));
  });

  it("names the flows the server says use the server, archived ones included", async () => {
    const inUse = new ApiError("in use", "mcp_server_in_use", 409, "req-4", [{ FlowID: "a9", FlowName: "Archived line", RemoteName: "latest_invoice", Required: false }]);
    const { user } = setup({ deleteMCPServer: vi.fn().mockRejectedValue(inUse), getFlows: vi.fn().mockResolvedValue([]) });
    await user.click(await screen.findByRole("button", { name: "Delete" }));
    await user.click(screen.getByRole("button", { name: "Delete server" }));
    const dialog = await screen.findByRole("dialog", { name: "billing is still in use" });
    expect(within(dialog).getByRole("link", { name: "Archived line" })).toHaveAttribute("href", "/flows/a9");
  });

  it("shows a detach that is waiting on calls as Being removed, without offering more actions", async () => {
    const inUse = new ApiError("in use", "mcp_server_in_use", 409, "req-4");
    const pending = server({ pending_deletion: true });
    const deleteMCPServer = vi.fn().mockRejectedValueOnce(inUse).mockResolvedValue({ pending: true, server: pending });
    const getMCPServer = vi.fn().mockResolvedValueOnce(detail()).mockResolvedValue(detail({ pending_deletion: true }));
    const { user } = setup({ deleteMCPServer, getMCPServer });
    await user.click(await screen.findByRole("button", { name: "Delete" }));
    await user.click(screen.getByRole("button", { name: "Delete server" }));
    await user.click(await screen.findByRole("button", { name: "Detach and delete" }));
    expect(await screen.findByText("Removal is waiting for calls in progress to finish.")).toBeVisible();
    expect(screen.queryByRole("button", { name: "Enable" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Delete" })).toBeNull();
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it("names its buttons so that the visible words are in them, and says which is open", async () => {
    const { user } = setup({});
    const schema = await screen.findByRole("button", { name: "Schema for latest_invoice" });
    expect(schema).toHaveAttribute("aria-expanded", "false");
    await user.click(schema);
    expect(screen.getByRole("button", { name: "Hide schema for latest_invoice" })).toHaveAttribute("aria-expanded", "true");
    expect(screen.getByRole("button", { name: "Try it for latest_invoice" })).toHaveAttribute("aria-expanded", "false");
  });
});
