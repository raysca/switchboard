import { describe, expect, it, vi } from "vitest";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router";
import { useState } from "react";
import { ApiError, type ApiClient, type MCPServerDTO, type MCPServerDetailDTO, type ToolSelection } from "../../../api/client";
import { ToolPicker } from "./ToolPicker";

const server = (over: Partial<MCPServerDTO> = {}): MCPServerDTO => ({
  id: "crm",
  name: "CRM",
  description: "",
  endpoint: "https://mcp.example.com/crm",
  transport: "streamable-http",
  validation_state: "healthy",
  latest_discovery_id: "d1",
  auth_env: "",
  auth_env_set: false,
  enabled: true,
  ...over,
});
const tool = (name: string, hash: string, description = `${name} description`) => ({ remote_name: name, description, input_schema: {}, schema_hash: hash });
const detail = (over: Partial<MCPServerDetailDTO> = {}): MCPServerDetailDTO => ({
  ...server(),
  discovery: { id: "d1", discovery_hash: "dh", created_at: "", tools: [tool("echo", "h-echo"), tool("lookup_order", "h-lookup")] },
  ...over,
});
const sel = (name: string, required = false, hash = `h-${name === "lookup_order" ? "lookup" : name}`): ToolSelection => ({ mcp_server_id: "crm", remote_name: name, schema_hash: hash, required });

function Harness({
  client,
  initial = [],
  onChange,
  initialAsync = [],
  onAsyncChange,
}: {
  client: Partial<ApiClient>;
  initial?: ToolSelection[];
  onChange?: (next: ToolSelection[]) => void;
  initialAsync?: string[];
  onAsyncChange?: (keys: string[]) => void;
}) {
  const [selected, setSelected] = useState(initial);
  const [asyncKeys, setAsyncKeys] = useState(initialAsync);
  return (
    <ToolPicker
      client={client as ApiClient}
      selected={selected}
      asyncKeys={asyncKeys}
      onAsyncChange={(k) => {
        setAsyncKeys(k);
        onAsyncChange?.(k);
      }}
      onChange={(n) => {
        setSelected(n);
        onChange?.(n);
      }}
    />
  );
}
function setup(client: Partial<ApiClient>, initial: ToolSelection[] = [], onChange = vi.fn(), asyncProps: { initialAsync?: string[]; onAsyncChange?: (k: string[]) => void } = {}) {
  render(
    <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
      <MemoryRouter>
        <Harness client={client} initial={initial} onChange={onChange} {...asyncProps} />
      </MemoryRouter>
    </QueryClientProvider>,
  );
  return { user: userEvent.setup(), onChange };
}
const api = (over: Partial<ApiClient> = {}): Partial<ApiClient> => ({
  getMCPServers: vi.fn().mockResolvedValue([server()]),
  getMCPServer: vi.fn().mockResolvedValue(detail()),
  ...over,
});

describe("ToolPicker", () => {
  it("lists a server's tools by name, with what the model sees", async () => {
    setup(api());
    const group = await screen.findByRole("group", { name: "CRM" });
    expect(within(group).getByRole("checkbox", { name: /^echo/ })).not.toBeChecked();
    expect(within(group).getByText("lookup_order description")).toBeVisible();
  });

  it("selects a tool with the schema hash the server offers now, and takes it off again", async () => {
    const { user, onChange } = setup(api());
    await user.click(await screen.findByRole("checkbox", { name: /^echo/ }));
    expect(onChange).toHaveBeenLastCalledWith([sel("echo")]);
    await user.click(screen.getByRole("checkbox", { name: /^echo/ }));
    expect(onChange).toHaveBeenLastCalledWith([]);
  });

  it("lets a chosen tool run in the background, and only a chosen tool", async () => {
    const onAsyncChange = vi.fn();
    const { user } = setup(api(), [sel("echo"), sel("lookup_order")], vi.fn(), { onAsyncChange });
    expect(await screen.findByText("The model gets an immediate acknowledgement and is told the result when it arrives.")).toBeVisible();
    await user.click(await screen.findByRole("checkbox", { name: "Background: lookup_order" }));
    expect(onAsyncChange).toHaveBeenLastCalledWith(["crm/lookup_order"]);
    await user.click(screen.getByRole("checkbox", { name: "Background: echo" }));
    expect(onAsyncChange).toHaveBeenLastCalledWith(["crm/echo", "crm/lookup_order"]);
    await user.click(screen.getByRole("checkbox", { name: "Background: echo" }));
    expect(onAsyncChange).toHaveBeenLastCalledWith(["crm/lookup_order"]);
  });

  it("offers no background box for a tool that is not chosen, and a stored one shows ticked", async () => {
    setup(api(), [sel("echo")], vi.fn(), { initialAsync: ["crm/echo"] });
    expect(await screen.findByRole("checkbox", { name: "Background: echo" })).toBeChecked();
    expect(screen.queryByRole("checkbox", { name: "Background: lookup_order" })).toBeNull();
    expect(screen.getByRole("button", { name: "Explain Runs in the background" })).toBeInTheDocument();
  });

  it("lets a chosen tool be marked required", async () => {
    const { user, onChange } = setup(api(), [sel("echo")]);
    await user.click(await screen.findByRole("checkbox", { name: "Required: echo" }));
    expect(onChange).toHaveBeenLastCalledWith([sel("echo", true)]);
  });

  it("ties a server's reason to its disabled tools", async () => {
    setup(api({ getMCPServers: vi.fn().mockResolvedValue([server({ enabled: false })]), getMCPServer: vi.fn().mockResolvedValue(detail({ enabled: false })) }));
    const box = await screen.findByRole("checkbox", { name: /^echo/ });
    expect(box).toHaveAccessibleDescription("Disabled");
  });

  it("shows a chosen tool of a disabled server as unavailable, and still lets it be taken off", async () => {
    const { user, onChange } = setup(
      api({ getMCPServers: vi.fn().mockResolvedValue([server({ enabled: false })]), getMCPServer: vi.fn().mockResolvedValue(detail({ enabled: false })) }),
      [sel("echo")],
    );
    const group = await screen.findByRole("group", { name: "CRM" });
    expect(within(group).getByText("Disabled")).toBeVisible();
    expect(within(group).getByRole("checkbox", { name: /^lookup_order/ })).toBeDisabled(); // cannot be newly chosen
    await user.click(within(group).getByRole("checkbox", { name: /^echo/ }));
    expect(onChange).toHaveBeenLastCalledWith([]);
  });

  it("says when a chosen tool is no longer offered, and when its server was removed", async () => {
    setup(api(), [sel("gone"), { ...sel("echo"), mcp_server_id: "deleted" }]);
    const group = await screen.findByRole("group", { name: "CRM" });
    expect(within(group).getByText("No longer offered")).toBeVisible();
    const removed = await screen.findByRole("group", { name: "Removed servers" });
    expect(within(removed).getByRole("checkbox", { name: /deleted\/echo/ })).toBeChecked();
  });

  it("names a server whose tools could not be read and offers to try again, without hiding the others", async () => {
    const other = server({ id: "orders", name: "Orders" });
    const getMCPServer = vi.fn().mockImplementation(async (id: string) => {
      if (id === "crm") throw new ApiError("boom", "internal_error", 500, "r1");
      return detail({ ...other, discovery: { id: "d2", discovery_hash: "x", created_at: "", tools: [tool("ship", "h-ship")] } });
    });
    const { user } = setup(api({ getMCPServers: vi.fn().mockResolvedValue([server(), other]), getMCPServer }));
    const broken = await screen.findByRole("group", { name: "CRM" });
    expect(within(broken).getByText("Could not load tools")).toBeVisible();
    expect(await screen.findByRole("checkbox", { name: /^ship/ })).toBeVisible();
    await user.click(within(broken).getByRole("button", { name: "Try again" }));
    expect(getMCPServer.mock.calls.filter(([id]) => id === "crm").length).toBeGreaterThan(1);
  });

  it("says how to get a server when none is connected", async () => {
    setup(api({ getMCPServers: vi.fn().mockResolvedValue([]) }));
    expect(await screen.findByText(/No MCP server is connected\./)).toBeVisible();
    expect(screen.getByRole("link", { name: "Connect a server" })).toHaveAttribute("href", "/mcp-servers");
  });

  it("makes no request until it is shown, and says it is loading while it waits", async () => {
    const getMCPServers = vi.fn().mockReturnValue(new Promise(() => {}));
    setup(api({ getMCPServers }));
    expect(screen.getByText(/Loading MCP servers/)).toBeVisible();
    expect(getMCPServers).toHaveBeenCalledTimes(1);
  });
});
