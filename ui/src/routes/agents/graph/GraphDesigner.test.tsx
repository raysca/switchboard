import { describe, expect, it, vi } from "vitest";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";
import { MemoryRouter } from "react-router";
import type { AgentDTO, ProviderStatus } from "../../../api/client";
import { GraphDesigner } from "./GraphDesigner";

const svc = { available: true, custom: true };
const providers: ProviderStatus[] = [
  { id: "google", display_name: "Google", capabilities: ["llm", "stt", "tts"], detected: true, env: ["GOOGLE_API_KEY"], services: { stt: svc, llm: svc, tts: svc } },
];
const flat = {
  stt: { provider: "google", model: "chirp" },
  llm: { provider: "google", model: "gemini", system_prompt: "Front desk." },
  tts: { provider: "google", voice: "v" },
};
const agent = (config: unknown): AgentDTO => ({
  id: "a1",
  name: "Support",
  description: "",
  enabled: true,
  archived: false,
  current_version: { id: "v1", number: 1, schema_version: 1, config: config as Record<string, unknown>, config_hash: "h", created_at: "", mcp_tools: [] },
  readiness: { state: "ready", reasons: [] },
  created_at: "",
  updated_at: "",
});

function setup(a: AgentDTO | null, testPanel?: ReactNode) {
  const onSubmit = vi.fn();
  render(
    <MemoryRouter>
      <GraphDesigner agent={a} providers={providers} pending={false} submitLabel="Publish" onSubmit={onSubmit} onCancel={vi.fn()} testPanel={testPanel} />
    </MemoryRouter>,
  );
  return { onSubmit, user: userEvent.setup() };
}

describe("GraphDesigner test panel", () => {
  const panel = <div data-testid="test-panel">voice test</div>;

  it("shows the test panel for a stored agent, outside the designer form", () => {
    setup(agent(flat), panel);
    const p = screen.getByTestId("test-panel");
    expect(p).toBeVisible();
    // The panel holds forms of its own: nested in the designer's form, a text send would publish.
    expect(p.closest("form")).toBeNull();
    expect(screen.queryByText(/unpublished edits/i)).toBeNull();
  });

  it("says a new agent must be published before it can be tested", () => {
    setup(null, panel);
    expect(screen.queryByTestId("test-panel")).toBeNull();
    expect(screen.getByText(/publish once to test/i)).toBeVisible();
  });

  it("warns while the canvas differs from the published version, and clears when it matches again", async () => {
    const { user } = setup(agent(flat), panel);
    await user.type(screen.getByLabelText("New agent node"), "Sales");
    await user.click(screen.getByRole("button", { name: "Add agent" }));
    expect(screen.getByText(/unpublished edits are not in this test/i)).toBeVisible();
    await user.click(screen.getByRole("button", { name: "Remove node" }));
    expect(screen.queryByText(/unpublished edits/i)).toBeNull();
  });

  it("does not call a published edit unpublished when the server stores the keys sorted", async () => {
    const bare = { stt: flat.stt, llm: { provider: "google", model: "gemini" }, tts: flat.tts };
    const user = userEvent.setup();
    const ui = (a: AgentDTO) => (
      <MemoryRouter>
        <GraphDesigner agent={a} providers={providers} pending={false} submitLabel="Publish" onSubmit={vi.fn()} onCancel={vi.fn()} testPanel={panel} />
      </MemoryRouter>
    );
    const { rerender } = render(ui(agent(bare)));
    await user.type(screen.getByLabelText("System prompt"), "Hi.");
    expect(screen.getByText(/unpublished edits are not in this test/i)).toBeVisible();
    // After Publish the refetched config has the same content with sorted keys.
    rerender(ui(agent({ llm: { model: "gemini", provider: "google", system_prompt: "Hi." }, stt: flat.stt, tts: flat.tts })));
    expect(screen.queryByText(/unpublished edits/i)).toBeNull();
  });
});

describe("GraphDesigner", () => {
  it("publishes a plain agent unchanged, as the flat config", async () => {
    const { onSubmit, user } = setup(agent(flat));
    await user.click(screen.getByRole("button", { name: "Publish" }));
    expect(onSubmit).toHaveBeenCalledTimes(1);
    expect(onSubmit.mock.calls[0][0].config).toEqual(flat);
  });

  it("adds an agent and a handoff, then publishes a graph", async () => {
    const { onSubmit, user } = setup(agent(flat));
    await user.type(screen.getByLabelText("New agent node"), "Sales");
    await user.click(screen.getByRole("button", { name: "Add agent" }));
    await user.selectOptions(screen.getByLabelText("From"), "main");
    const to = screen.getByLabelText("To");
    await user.selectOptions(to, within(to).getByRole("option", { name: "Sales" }));
    await user.type(screen.getByLabelText("Hand off when"), "The caller wants to buy.");
    await user.click(screen.getByRole("button", { name: "Add handoff" }));
    await user.click(screen.getByRole("button", { name: "Publish" }));

    const config = onSubmit.mock.calls[0][0].config as {
      entry: string;
      nodes: { name: string }[];
      edges: { condition: string }[];
    };
    expect(config.nodes.map((n) => n.name)).toEqual(["main", "Sales"]);
    expect(config.edges).toHaveLength(1);
    expect(config.edges[0].condition).toBe("The caller wants to buy.");
    expect(config.entry).toBe("main");
  });

  it("keeps the agent's MCP tools on the entry node when a node is added", async () => {
    const withTools = agent(flat);
    withTools.current_version!.mcp_tools = [{ mcp_server_id: "srv", remote_name: "lookup", schema_hash: "h", required: false }];
    const { onSubmit, user } = setup(withTools);
    await user.type(screen.getByLabelText("New agent node"), "Sales");
    await user.click(screen.getByRole("button", { name: "Add agent" }));
    await user.click(screen.getByRole("button", { name: "Publish" }));
    const config = onSubmit.mock.calls[0][0].config as { nodes: { tools?: string[] }[] };
    expect(config.nodes[0].tools).toEqual(["srv/lookup"]);
  });

  it("refuses to publish an incomplete new agent and lists what is missing", async () => {
    const { onSubmit, user } = setup(null);
    await user.click(screen.getByRole("button", { name: "Publish" }));
    expect(onSubmit).not.toHaveBeenCalled();
    const alert = screen.getByRole("alert");
    expect(alert).toHaveTextContent("Agent name is required.");
    expect(alert).toHaveTextContent("LLM model is required.");
  });

  it("does not offer a handoff from a node to itself", async () => {
    const { user } = setup(agent(flat));
    await user.selectOptions(screen.getByLabelText("From"), "main");
    await user.selectOptions(screen.getByLabelText("To"), "main");
    await user.type(screen.getByLabelText("Hand off when"), "x");
    expect(screen.getByRole("button", { name: "Add handoff" })).toBeDisabled();
    expect(screen.getByRole("note")).toHaveTextContent("itself");
  });
});
