import { describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router";
import { MCPView } from "./MCPView";
import { ApiError, type ApiClient, type MCPServerDTO, type MCPServerDetailDTO } from "../../api/client";

function renderWithProviders(ui: React.ReactElement) {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={queryClient}>
      <MemoryRouter>{ui}</MemoryRouter>
    </QueryClientProvider>,
  );
}

function createMockServer(overrides: Partial<MCPServerDTO> = {}): MCPServerDTO {
  return {
    id: "mcp-server-1",
    name: "Customer Data MCP",
    description: "Customer lookup tools",
    endpoint: "https://mcp.example.com/rpc",
    transport: "streamable-http",
    validation_state: "valid",
    latest_discovery_id: "disc-hash-123456",
    enabled: true,
    ...overrides,
  };
}

function createMockServerDetail(overrides: Partial<MCPServerDetailDTO> = {}): MCPServerDetailDTO {
  return {
    ...createMockServer(),
    discovery: {
      id: "disc-1",
      discovery_hash: "disc-hash-123456",
      created_at: "2026-10-05T12:00:00Z",
      tools: [
        {
          remote_name: "get_customer",
          description: "Get customer by ID",
          input_schema: {
            type: "object",
            properties: { id: { type: "string" } },
            required: ["id"],
          },
          schema_hash: "hash-tool-v1",
        },
      ],
    },
    ...overrides,
  };
}

function createMockClient(overrides: Partial<ApiClient> = {}): ApiClient {
  return {
    getMCPServers: vi.fn().mockResolvedValue([createMockServer()]),
    getMCPServer: vi.fn().mockResolvedValue(createMockServerDetail()),
    createMCPServer: vi.fn().mockResolvedValue(createMockServer()),
    updateMCPServer: vi.fn().mockResolvedValue(createMockServer()),
    deleteMCPServer: vi.fn().mockResolvedValue({ deleted: true }),
    disableMCPServer: vi.fn().mockResolvedValue(createMockServer({ enabled: false })),
    enableMCPServer: vi.fn().mockResolvedValue(createMockServer({ enabled: true })),
    discoverMCPServer: vi.fn().mockResolvedValue(createMockServer()),
    testMCPTool: vi.fn().mockResolvedValue({
      execution_id: "exec-999",
      mcp_server_id: "mcp-server-1",
      tool_name: "get_customer",
      status: "completed",
      duration_ms: 42,
      result: { name: "Alice", id: "cust-1" },
    }),
    getCredentials: vi.fn().mockResolvedValue([
      { id: "cred-1", provider: "mcp", masked_value: "••••1234" },
    ]),
    getAgents: vi.fn().mockResolvedValue([]),
    ...overrides,
  } as ApiClient;
}

describe("MCPView", () => {
  it("renders prompt when key is absent", () => {
    const client = createMockClient();
    renderWithProviders(<MCPView client={client} keyValue={null} />);
    expect(screen.getByText("Connect to inspect MCP servers")).toBeVisible();
  });

  it("renders empty state when no servers exist", async () => {
    const client = createMockClient({
      getMCPServers: vi.fn().mockResolvedValue([]),
    });
    renderWithProviders(<MCPView client={client} keyValue="test-key" />);
    expect(await screen.findByText("No MCP servers configured")).toBeVisible();
  });

  it("renders list of servers with status badges for connected, error, and disabled", async () => {
    const servers: MCPServerDTO[] = [
      createMockServer({ id: "s-1", name: "Valid Server", validation_state: "valid", enabled: true }),
      createMockServer({ id: "s-2", name: "Error Server", validation_state: "schema_invalid", enabled: true }),
      createMockServer({ id: "s-3", name: "Disabled Server", validation_state: "disabled", enabled: false }),
    ];
    const client = createMockClient({
      getMCPServers: vi.fn().mockResolvedValue(servers),
    });

    renderWithProviders(<MCPView client={client} keyValue="test-key" />);

    expect(await screen.findByText("Valid Server")).toBeVisible();
    expect(screen.getByText("Error Server")).toBeVisible();
    expect(screen.getByText("Disabled Server")).toBeVisible();

    expect(screen.getByText("connected")).toBeVisible();
    expect(screen.getByText("error")).toBeVisible();
    expect(screen.getByText("disabled")).toBeVisible();
  });

  it("supports creating a new MCP server with transport and credential", async () => {
    const user = userEvent.setup();
    const createMCPServer = vi.fn().mockResolvedValue(createMockServer({ id: "s-new", name: "New Server" }));
    const client = createMockClient({ createMCPServer });

    renderWithProviders(<MCPView client={client} keyValue="test-key" />);

    await user.click(await screen.findByRole("button", { name: "Add MCP Server" }));

    await user.type(screen.getByLabelText("Server Name"), "New Server");
    await user.type(screen.getByLabelText("Endpoint URL"), "https://mcp.example.com/new");

    await user.click(screen.getByRole("button", { name: "Create Server" }));

    expect(createMCPServer).toHaveBeenCalledWith(
      expect.objectContaining({
        name: "New Server",
        endpoint: "https://mcp.example.com/new",
        transport: "streamable-http",
      }),
    );
  });

  it("displays server detail view, discovered tools, and JSON schema viewer", async () => {
    const user = userEvent.setup();
    const client = createMockClient();

    renderWithProviders(<MCPView client={client} keyValue="test-key" />);

    const inspectBtn = await screen.findByRole("button", { name: "Inspect" });
    await user.click(inspectBtn);

    expect(await screen.findByText(/get_customer/)).toBeVisible();
    expect(screen.getByText("Get customer by ID")).toBeVisible();

    const viewSchemaBtn = screen.getByRole("button", { name: "View Schema" });
    await user.click(viewSchemaBtn);

    expect(screen.getByTestId("tool-schema-viewer")).toBeVisible();
    expect(screen.getByText(/"required": \[/)).toBeVisible();
  });

  it("displays schema drift badge when tool hash does not match agent selection", async () => {
    const client = createMockClient({
      getAgents: vi.fn().mockResolvedValue([
        {
          id: "agent-1",
          name: "Agent with Drifted Tool",
          current_version: {
            mcp_tools: [
              {
                mcp_server_id: "mcp-server-1",
                remote_name: "get_customer",
                schema_hash: "different-old-hash",
                required: true,
              },
            ],
          },
          readiness: { state: "needs_tool_review", reasons: ["needs_tool_review"] },
        },
      ]),
    });

    renderWithProviders(
      <MCPView client={client} keyValue="test-key" initialServerId="mcp-server-1" />,
    );

    expect(await screen.findByTestId("schema-drift-badge")).toBeVisible();
    expect(screen.getByText("Schema drift")).toBeVisible();
  });

  it("executes interactive tool test and displays results and execution time", async () => {
    const user = userEvent.setup();
    const testMCPTool = vi.fn().mockResolvedValue({
      execution_id: "exec-1234",
      mcp_server_id: "mcp-server-1",
      tool_name: "get_customer",
      status: "completed",
      duration_ms: 55,
      result: { customer_id: "cust-1", name: "Alice Doe" },
    });
    const client = createMockClient({ testMCPTool });

    renderWithProviders(
      <MCPView client={client} keyValue="test-key" initialServerId="mcp-server-1" />,
    );

    const testConsoleBtn = await screen.findByRole("button", { name: "Test Console" });
    await user.click(testConsoleBtn);

    expect(screen.getByTestId("test-console")).toBeVisible();

    const input = screen.getByTestId("test-arguments-input");
    fireEvent.change(input, { target: { value: '{"id":"cust-1"}' } });

    const executeBtn = screen.getByTestId("execute-tool-button");
    await user.click(executeBtn);

    expect(testMCPTool).toHaveBeenCalledWith("mcp-server-1", "get_customer", { id: "cust-1" });

    expect(await screen.findByTestId("execution-result")).toBeVisible();
    expect(screen.getByText(/55\s*ms/)).toBeVisible();
    expect(screen.getByText(/"name": "Alice Doe"/)).toBeVisible();
  });

  it("handles tool test error when arguments are invalid JSON or execution fails", async () => {
    const user = userEvent.setup();
    const testMCPTool = vi.fn().mockRejectedValue(
      new ApiError("Remote tool returned status 502", "mcp_tool_failed", 502, "req-fail"),
    );
    const client = createMockClient({ testMCPTool });

    renderWithProviders(
      <MCPView client={client} keyValue="test-key" initialServerId="mcp-server-1" />,
    );

    await user.click(await screen.findByRole("button", { name: "Test Console" }));

    const input = screen.getByTestId("test-arguments-input");

    // Case 1: Invalid JSON syntax error handled safely
    fireEvent.change(input, { target: { value: "{bad_json" } });
    await user.click(screen.getByTestId("execute-tool-button"));
    expect(screen.getByText("Arguments must be valid JSON")).toBeVisible();

    // Case 2: API execution error handled safely
    fireEvent.change(input, { target: { value: '{"valid":"json"}' } });
    await user.click(screen.getByTestId("execute-tool-button"));
    expect(await screen.findByText("Remote tool returned status 502 (Request ID: req-fail)")).toBeVisible();
  });

  it("handles enable, disable, and discover server actions", async () => {
    const user = userEvent.setup();
    const disableMCPServer = vi.fn().mockResolvedValue(createMockServer({ enabled: false }));
    const discoverMCPServer = vi.fn().mockResolvedValue(createMockServer());
    const client = createMockClient({ disableMCPServer, discoverMCPServer });

    renderWithProviders(<MCPView client={client} keyValue="test-key" />);

    const disableBtn = await screen.findByRole("button", { name: "Disable" });
    await user.click(disableBtn);
    expect(disableMCPServer).toHaveBeenCalledWith("mcp-server-1");

    const discoverBtn = screen.getByRole("button", { name: "Discover" });
    await user.click(discoverBtn);
    expect(discoverMCPServer).toHaveBeenCalledWith("mcp-server-1");
  });

  it("displays conflict warning alert when delete is blocked and supports confirm detach-and-delete", async () => {
    const user = userEvent.setup();
    const conflictError = new ApiError(
      "MCP server is referenced by agents or active sessions",
      "mcp_server_in_use",
      409,
      "req-conflict",
    );
    const deleteMCPServer = vi.fn()
      .mockRejectedValueOnce(conflictError)
      .mockResolvedValueOnce({ deleted: true });
    const client = createMockClient({ deleteMCPServer });

    renderWithProviders(<MCPView client={client} keyValue="test-key" />);

    const deleteBtn = await screen.findByRole("button", { name: "Delete" });
    await user.click(deleteBtn);

    // Conflict alert must appear
    expect(await screen.findByTestId("delete-conflict-alert")).toBeVisible();
    expect(screen.getByText("Server in Use: Deletion Blocked")).toBeVisible();

    // Click confirm detach & delete
    const detachBtn = screen.getByRole("button", { name: "Confirm Detach & Delete" });
    await user.click(detachBtn);

    expect(deleteMCPServer).toHaveBeenCalledWith("mcp-server-1", true);
  });

  it("triggers an error indicating arguments must be a JSON object when given array or primitive", async () => {
    const user = userEvent.setup();
    const testMCPTool = vi.fn();
    const client = createMockClient({ testMCPTool });

    renderWithProviders(
      <MCPView client={client} keyValue="test-key" initialServerId="mcp-server-1" />,
    );

    await user.click(await screen.findByRole("button", { name: "Test Console" }));

    const input = screen.getByTestId("test-arguments-input");
    const executeBtn = screen.getByTestId("execute-tool-button");

    // Case 1: JSON array
    fireEvent.change(input, { target: { value: '["foo", "bar"]' } });
    await user.click(executeBtn);
    expect(screen.getByText("Arguments must be a valid JSON object")).toBeVisible();
    expect(testMCPTool).not.toHaveBeenCalled();

    // Case 2: Primitive number
    fireEvent.change(input, { target: { value: "12345" } });
    await user.click(executeBtn);
    expect(screen.getByText("Arguments must be a valid JSON object")).toBeVisible();
    expect(testMCPTool).not.toHaveBeenCalled();

    // Case 3: Primitive string
    fireEvent.change(input, { target: { value: '"a string"' } });
    await user.click(executeBtn);
    expect(screen.getByText("Arguments must be a valid JSON object")).toBeVisible();
    expect(testMCPTool).not.toHaveBeenCalled();

    // Case 4: null
    fireEvent.change(input, { target: { value: "null" } });
    await user.click(executeBtn);
    expect(screen.getByText("Arguments must be a valid JSON object")).toBeVisible();
    expect(testMCPTool).not.toHaveBeenCalled();
  });

  it("displays X-Request-ID in error alert when ApiError occurs during create or test", async () => {
    const user = userEvent.setup();
    const createMCPServer = vi.fn().mockRejectedValue(
      new ApiError("Invalid schema payload", "invalid_payload", 400, "req-create-999"),
    );
    const testMCPTool = vi.fn().mockRejectedValue(
      new ApiError("Remote tool timeout", "tool_timeout", 504, "req-tool-888"),
    );
    const client = createMockClient({ createMCPServer, testMCPTool });

    // Test during create
    renderWithProviders(<MCPView client={client} keyValue="test-key" />);

    await user.click(await screen.findByRole("button", { name: "Add MCP Server" }));
    await user.type(screen.getByLabelText("Server Name"), "Test Server");
    await user.type(screen.getByLabelText("Endpoint URL"), "https://mcp.example.com");
    await user.click(screen.getByRole("button", { name: "Create Server" }));

    expect(
      await screen.findByText("Invalid schema payload (Request ID: req-create-999)"),
    ).toBeVisible();

    // Test during tool test
    await user.click(screen.getByRole("button", { name: "Cancel" }));

    const inspectBtn = await screen.findByRole("button", { name: "Inspect" });
    await user.click(inspectBtn);

    await user.click(await screen.findByRole("button", { name: "Test Console" }));
    const input = screen.getByTestId("test-arguments-input");
    fireEvent.change(input, { target: { value: '{"key": "val"}' } });
    await user.click(screen.getByTestId("execute-tool-button"));

    expect(
      await screen.findByText("Remote tool timeout (Request ID: req-tool-888)"),
    ).toBeVisible();
  });
});
