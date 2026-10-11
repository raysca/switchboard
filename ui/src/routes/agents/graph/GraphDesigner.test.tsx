import { describe, expect, it, vi } from "vitest";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router";
import type { AgentDTO, CredentialDTO, ProviderDefinition } from "../../../api/client";
import { GraphDesigner } from "./GraphDesigner";

const providers: ProviderDefinition[] = [
  { id: "google", display_name: "Google", capabilities: ["llm", "stt", "tts"], auth_required: true },
];
const credentials: CredentialDTO[] = [
  { id: "cred-g1", provider: "google", masked_value: "…abcd", validation_state: "valid", capabilities: [], created_at: "", updated_at: "" },
];
const flat = {
  stt: { provider: "google", model: "chirp", credential_id: "cred-g1" },
  llm: { provider: "google", model: "gemini", system_prompt: "Front desk.", credential_id: "cred-g1" },
  tts: { provider: "google", voice: "v", credential_id: "cred-g1" },
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

function setup(a: AgentDTO | null) {
  const onSubmit = vi.fn();
  render(
    <MemoryRouter>
      <GraphDesigner agent={a} providers={providers} credentials={credentials} pending={false} submitLabel="Publish" onSubmit={onSubmit} onCancel={vi.fn()} />
    </MemoryRouter>,
  );
  return { onSubmit, user: userEvent.setup() };
}

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
