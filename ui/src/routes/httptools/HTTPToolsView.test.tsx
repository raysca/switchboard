import { describe, expect, it, vi } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, Route, Routes, useLocation } from "react-router";
import { HTTPToolsView } from "./HTTPToolsView";
import { ApiError, type AgentDTO, type ApiClient, type HTTPToolDTO } from "../../api/client";

const tool = (over: Partial<HTTPToolDTO> = {}): HTTPToolDTO => ({
  id: "t1",
  name: "get_order",
  enabled: true,
  hash: "h1",
  definition: { name: "get_order", description: "Look up an order.", method: "GET", url: "https://api.example.com/orders/{order_id}", input_schema: { type: "object", properties: { order_id: { type: "string" } }, required: ["order_id"] }, auth: { secret: "crm-api" } },
  auth_secret_set: true,
  created_at: "",
  updated_at: "",
  ...over,
});
const agent = (id: string, name: string, state: string): AgentDTO =>
  ({ id, name, description: "", enabled: true, archived: false, created_at: "", updated_at: "", current_version: null, readiness: { state: "needs_tool_review", reasons: [], tool_dependencies: [{ mcp_server_id: "http:t1", remote_name: "get_order", required: true, state, selected_schema_hash: "h0", current_schema_hash: "h1" }] } }) as unknown as AgentDTO;

function Where() {
  return <p data-testid="where">{useLocation().pathname}</p>;
}
function setup(client: Partial<ApiClient> = {}, path = "/http-tools") {
  const full = {
    getHTTPTools: vi.fn().mockResolvedValue([tool()]),
    getSecrets: vi.fn().mockResolvedValue({ store_enabled: true, secrets: [{ name: "crm-api", source: "store", writable: true }] }),
    getAgents: vi.fn().mockResolvedValue([]),
    createHTTPTool: vi.fn().mockResolvedValue(tool({ id: "new", name: "add_note" })),
    updateHTTPTool: vi.fn().mockResolvedValue(tool()),
    setHTTPToolEnabled: vi.fn().mockResolvedValue(tool({ enabled: false })),
    deleteHTTPTool: vi.fn().mockResolvedValue(undefined),
    testHTTPTool: vi.fn().mockResolvedValue({ status: "completed", duration_ms: 12, result: { status: 200, body: { ok: true } } }),
    ...client,
  } as unknown as ApiClient;
  render(
    <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
      <MemoryRouter initialEntries={[path]}>
        <Routes>
          <Route path="/http-tools" element={<HTTPToolsView client={full} keyValue="k" />} />
          <Route path="/http-tools/:toolId" element={<HTTPToolsView client={full} keyValue="k" />} />
        </Routes>
        <Where />
      </MemoryRouter>
    </QueryClientProvider>,
  );
  return { client: full, user: userEvent.setup() };
}

describe("HTTPToolsView", () => {
  it("lists tools and shows the first one's definition", async () => {
    setup();
    const nav = await screen.findByRole("navigation", { name: "HTTP tools" });
    expect(within(nav).getByText("get_order")).toBeVisible();
    expect(within(nav).getByText("api.example.com")).toBeVisible();
    expect(await screen.findByLabelText("URL")).toHaveValue("https://api.example.com/orders/{order_id}");
    expect(screen.getByLabelText("Secret (optional)")).toHaveValue("crm-api");
  });

  it("warns in the list when a tool's secret does not resolve", async () => {
    setup({ getHTTPTools: vi.fn().mockResolvedValue([tool({ auth_secret_set: false })]) });
    expect(await screen.findByText("Secret crm-api is not set")).toBeVisible();
  });

  it("says how to start when there are none", async () => {
    setup({ getHTTPTools: vi.fn().mockResolvedValue([]) });
    expect(await screen.findByRole("heading", { name: "No HTTP tools yet." })).toBeVisible();
  });

  it("creates a tool from the form and shows the server's reason when it is refused", async () => {
    const createHTTPTool = vi
      .fn()
      .mockRejectedValueOnce(new ApiError("the tool definition is not valid", "invalid_http_tool", 400, "r1", { reason: "url must use https" }))
      .mockResolvedValueOnce(tool({ id: "new", name: "add_note" }));
    const { user } = setup({ createHTTPTool }, "/http-tools/new");
    await user.type(await screen.findByLabelText("Name"), "add_note");
    await user.type(screen.getByLabelText("Description"), "Add a note.");
    await user.clear(screen.getByLabelText("URL"));
    await user.type(screen.getByLabelText("URL"), "http://a.example/notes");
    await user.click(screen.getByRole("button", { name: "Create tool" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("url must use https (Request ID: r1)");
    await user.clear(screen.getByLabelText("URL"));
    await user.type(screen.getByLabelText("URL"), "https://a.example/notes");
    await user.click(screen.getByRole("button", { name: "Create tool" }));
    await waitFor(() => expect(screen.getByTestId("where")).toHaveTextContent("/http-tools/new"));
    expect(createHTTPTool).toHaveBeenLastCalledWith(expect.objectContaining({ name: "add_note", method: "GET", url: "https://a.example/notes", description: "Add a note." }));
  });

  it("refuses bad form input before calling the server", async () => {
    const { user, client } = setup({}, "/http-tools/new");
    const schema = await screen.findByLabelText("Input schema");
    await user.clear(schema);
    await user.type(schema, "not json");
    await user.click(screen.getByRole("button", { name: "Create tool" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("Input schema is not valid JSON.");
    expect(client.createHTTPTool).not.toHaveBeenCalled();
  });

  it("saves edits to the selected tool", async () => {
    const { user, client } = setup();
    const desc = await screen.findByLabelText("Description");
    await user.clear(desc);
    await user.type(desc, "Find an order.");
    await user.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(client.updateHTTPTool).toHaveBeenCalledWith("t1", expect.objectContaining({ description: "Find an order." })));
  });

  it("runs a test call and shows the result, or the category of the failure", async () => {
    const testHTTPTool = vi
      .fn()
      .mockResolvedValueOnce({ status: "completed", duration_ms: 12, result: { status: 200, body: { ok: true } } })
      .mockResolvedValueOnce({ status: "failed", duration_ms: 5, error_category: "tool_error", message: "the service answered HTTP 404" });
    const { user } = setup({ testHTTPTool });
    const box = await screen.findByLabelText("JSON arguments");
    await user.clear(box);
    await user.type(box, "{{\"order_id\":\"A-1\"}");
    await user.click(screen.getByRole("button", { name: "Run test" }));
    expect(await screen.findByText(/"ok": true/)).toBeVisible();
    expect(testHTTPTool).toHaveBeenLastCalledWith("t1", { order_id: "A-1" });
    await user.click(screen.getByRole("button", { name: "Run test" }));
    expect(await screen.findByText(/The endpoint answered with an error: the service answered HTTP 404/)).toBeVisible();
  });

  it("names the agents that block a delete", async () => {
    const deleteHTTPTool = vi.fn().mockRejectedValue(new ApiError("the tool is pinned", "http_tool_in_use", 409, "r2", [{ AgentID: "a1", AgentName: "Front desk" }]));
    const { user } = setup({ deleteHTTPTool });
    await user.click(await screen.findByRole("button", { name: "Delete" }));
    await user.click(screen.getByRole("button", { name: "Delete tool" }));
    expect(await screen.findByRole("alert")).toHaveTextContent(/Still used by|the tool is pinned/);
  });

  it("tells which agents are waiting to choose an edited tool again", async () => {
    setup({ getAgents: vi.fn().mockResolvedValue([agent("a1", "Front desk", "tool_edited"), agent("a2", "Other", "ready")]) });
    expect(await screen.findByRole("link", { name: "Front desk" })).toHaveAttribute("href", "/agents/a1");
    expect(screen.getByText(/need to choose it again/)).toBeVisible();
    expect(screen.queryByRole("link", { name: "Other" })).toBeNull();
  });

  it("disables and enables", async () => {
    const { user, client } = setup();
    await user.click(await screen.findByRole("button", { name: "Disable" }));
    await waitFor(() => expect(client.setHTTPToolEnabled).toHaveBeenCalledWith("t1", false));
  });
});
