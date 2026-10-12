import { describe, expect, it, vi } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router";
import { ApiError, type AgentDTO, type ApiClient, type ToolDependencyDTO } from "../../api/client";
import type { Drift } from "./mcpStats";
import { DriftPanel, MissingTools } from "./DriftPanel";

const dep = (over: Partial<ToolDependencyDTO> = {}): ToolDependencyDTO => ({
  mcp_server_id: "billing",
  remote_name: "send_pay_link",
  required: true,
  state: "schema_drift",
  selected_schema_hash: "7a3e0000aa",
  current_schema_hash: "c9d00000bb",
  ...over,
});
const flow = (id: string, name: string, deps: ToolDependencyDTO[]): AgentDTO => ({
  id,
  name,
  description: "",
  enabled: true,
  archived: false,
  current_version: {
    id: `${id}-v`,
    number: 3,
    schema_version: 1,
    config: {},
    config_hash: "h",
    created_at: "",
    mcp_tools: [
      { mcp_server_id: "billing", remote_name: "send_pay_link", schema_hash: "7a3e0000aa", required: deps[0]?.required ?? true },
      { mcp_server_id: "crm", remote_name: "lookup", schema_hash: "keep", required: false },
    ],
  },
  readiness: { state: "needs_tool_review", reasons: [], tool_dependencies: deps },
  created_at: "",
  updated_at: "",
});
const front = flow("f1", "Front desk", [dep()]);
const triage = flow("f2", "Support triage", [dep({ required: false })]);
const items: Drift[] = [
  { flow: front, dep: front.readiness.tool_dependencies![0], kind: "changed" },
  { flow: triage, dep: triage.readiness.tool_dependencies![0], kind: "changed" },
];
const schema = { type: "object", properties: { amount_pence: { type: "integer" } } };

function setup(setAgentMCPTools = vi.fn().mockResolvedValue({})) {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const invalidate = vi.spyOn(queryClient, "invalidateQueries");
  const client = { setAgentMCPTools } as unknown as ApiClient;
  render(
    <QueryClientProvider client={queryClient}>
      <MemoryRouter>
        <DriftPanel client={client} tool="send_pay_link" schema={schema} items={items} />
      </MemoryRouter>
    </QueryClientProvider>,
  );
  return { setAgentMCPTools, invalidate, user: userEvent.setup() };
}

describe("DriftPanel", () => {
  it("says what changed, and what it means: required flows are blocked, optional ones carry on", () => {
    setup();
    expect(screen.getByRole("heading", { name: "send_pay_link changed its input schema", level: 3 })).toBeVisible();
    expect(screen.getByText(/Flows that require it need your review before they take calls again/)).toBeVisible();
    const rows = screen.getAllByRole("listitem");
    expect(within(rows[0]).getByRole("link", { name: "Front desk" })).toHaveAttribute("href", "/flows/f1");
    expect(within(rows[0]).getByText("requires it")).toBeVisible();
    expect(within(rows[0]).getByText("7a3e0000 → c9d00000")).toBeVisible();
    expect(within(rows[1]).getByText("uses it optionally")).toBeVisible();
  });

  it("shows the schema the server offers now", () => {
    setup();
    expect(screen.getByText(/"amount_pence"/)).toBeVisible();
  });

  it("accepts for one flow only after confirmation, sending the full list with just that hash replaced", async () => {
    const { setAgentMCPTools, invalidate, user } = setup();
    await user.click(screen.getByRole("button", { name: "Accept new schema for Front desk" }));
    expect(screen.getByRole("dialog", { name: "Accept the new schema for Front desk?" })).toBeVisible();
    expect(setAgentMCPTools).not.toHaveBeenCalled();
    await user.click(screen.getByRole("button", { name: "Publish new version" }));
    expect(setAgentMCPTools).toHaveBeenCalledWith("f1", [
      { mcp_server_id: "billing", remote_name: "send_pay_link", schema_hash: "c9d00000bb", required: true },
      { mcp_server_id: "crm", remote_name: "lookup", schema_hash: "keep", required: false },
    ]);
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(invalidate).toHaveBeenCalledWith({ queryKey: ["agents"] });
  });

  it("does nothing when the dialog is cancelled", async () => {
    const { setAgentMCPTools, user } = setup();
    await user.click(screen.getByRole("button", { name: "Accept new schema for Front desk" }));
    await user.click(screen.getByRole("button", { name: "Cancel" }));
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(setAgentMCPTools).not.toHaveBeenCalled();
  });

  it("keeps the dialog open and shows the server's refusal", async () => {
    const { user } = setup(vi.fn().mockRejectedValue(new ApiError("tool is not referenced by any step", "invalid_request", 400, "req-5")));
    await user.click(screen.getByRole("button", { name: "Accept new schema for Support triage" }));
    await user.click(screen.getByRole("button", { name: "Publish new version" }));
    expect(await within(screen.getByRole("dialog")).findByRole("alert")).toHaveTextContent("tool is not referenced by any step (Request ID: req-5)");
    expect(screen.getByRole("dialog")).toBeVisible();
  });

  it("offers no accept when the server's current hash is unknown", () => {
    const noHash = flow("f3", "No hash", [dep({ current_schema_hash: undefined })]);
    const queryClient = new QueryClient();
    render(
      <QueryClientProvider client={queryClient}>
        <MemoryRouter>
          <DriftPanel client={{} as ApiClient} tool="send_pay_link" schema={schema} items={[{ flow: noHash, dep: noHash.readiness.tool_dependencies![0], kind: "changed" }]} />
        </MemoryRouter>
      </QueryClientProvider>,
    );
    expect(screen.queryByRole("button", { name: /Accept new schema/ })).toBeNull();
  });
});

describe("MissingTools", () => {
  it("names tools the server no longer offers and the flows that pin them", () => {
    const f = flow("f4", "Billing line", [dep({ remote_name: "gone", state: "tool_missing", current_schema_hash: undefined })]);
    render(
      <MemoryRouter>
        <MissingTools items={[{ flow: f, dep: f.readiness.tool_dependencies![0], kind: "missing" }]} />
      </MemoryRouter>,
    );
    expect(screen.getByRole("heading", { name: "gone is no longer offered by this server" })).toBeVisible();
    expect(screen.getByRole("link", { name: "Billing line" })).toHaveAttribute("href", "/flows/f4");
  });
});
