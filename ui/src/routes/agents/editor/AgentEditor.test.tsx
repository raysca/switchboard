import { describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router";
import { ReactFlowProvider } from "@xyflow/react";
import type { AgentDTO, ApiClient, ProviderStatus, MCPServerDTO, MCPServerDetailDTO } from "../../../api/client";
import { AgentEditor, type AgentEditorProps } from "./AgentEditor";

const svc = {
  available: true,
  custom: true,
  models: [{ id: "gemini", label: "Gemini", default: true }],
  voices: [{ id: "aoede", label: "Aoede", default: true }],
  languages: [{ id: "en", label: "English", default: true }],
};
const providers: ProviderStatus[] = [
  { id: "google", display_name: "Google", capabilities: ["llm", "stt", "tts"], detected: true, env: [], services: { stt: svc, llm: svc, tts: svc } },
];
const flat = {
  stt: { provider: "google", model: "gemini" },
  llm: { provider: "google", model: "gemini", system_prompt: "Front desk." },
  tts: { provider: "google", voice: "aoede" },
};
const agent: AgentDTO = {
  id: "a1",
  name: "Front desk",
  description: "",
  enabled: true,
  archived: false,
  current_version: { id: "v1", number: 1, schema_version: 1, config: flat, config_hash: "h", created_at: "", mcp_tools: [] },
  readiness: { state: "ready", reasons: [] },
  created_at: "",
  updated_at: "",
};
const client = { issueVoiceToken: vi.fn(), getSessionTraces: vi.fn() } as unknown as ApiClient;

function setup(props: Partial<AgentEditorProps> = {}) {
  const onSubmit = vi.fn();
  render(
    <QueryClientProvider client={new QueryClient()}>
      <MemoryRouter>
        <ReactFlowProvider>
          <AgentEditor agent={null} providers={providers} client={client} keyValue="k" pending={false} onSubmit={onSubmit} onCancel={vi.fn()} {...props} />
        </ReactFlowProvider>
      </MemoryRouter>
    </QueryClientProvider>,
  );
  return { onSubmit, user: userEvent.setup() };
}

describe("AgentEditor", () => {
  it("starts a new agent blank and opens the first step's panel on Basics, name focused", async () => {
    const { user } = setup();
    expect(screen.getByRole("heading", { name: "New agent" })).toBeVisible();
    expect(screen.getByRole("status")).toHaveTextContent("No steps yet");
    await user.click(screen.getByRole("button", { name: "Add first step" }));
    expect(screen.getByRole("complementary", { name: "New step" })).toBeVisible();
    expect(screen.getByLabelText("Step name")).toHaveFocus();
    expect(screen.getByRole("status")).toHaveTextContent("1 step needs setup");
  });

  it("cannot test an agent that was never published", () => {
    setup();
    expect(screen.getByRole("button", { name: "Test call" })).toBeDisabled();
  });

  it("blocks publish until every step and the agent are set up, then sends the graph", async () => {
    const { user, onSubmit } = setup();
    await user.click(screen.getByRole("button", { name: "Add first step" }));
    await user.click(screen.getByRole("button", { name: "Publish" }));
    const dialog = screen.getByRole("dialog");
    expect(within(dialog).getByRole("alert")).toBeVisible();
    expect(within(dialog).getByRole("button", { name: "Publish v1" })).toBeDisabled();
    await user.click(within(dialog).getByRole("button", { name: "Keep editing" }));

    const step = screen.getByRole("complementary", { name: "New step" });
    await user.selectOptions(within(step).getByLabelText("LLM provider"), "google");
    await user.selectOptions(within(step).getByLabelText("TTS provider"), "google");
    await user.click(screen.getByRole("button", { name: "Agent settings" }));
    await user.type(screen.getByLabelText("Agent name"), "After hours");
    await user.selectOptions(screen.getByLabelText("STT provider"), "google");
    await user.click(screen.getByRole("button", { name: "Publish" }));
    await user.click(screen.getByRole("button", { name: "Publish v1" }));
    // A named step goes in the steps form, so its name is kept.
    const sent = onSubmit.mock.calls[0][0] as { name: string; config: { steps: { name: string; config: { llm: { provider: string } } }[] } };
    expect(sent.name).toBe("After hours");
    expect(sent.config.steps).toEqual([expect.objectContaining({ name: "New step", config: expect.objectContaining({ llm: expect.objectContaining({ provider: "google" }) }) })]);
  });

  it("tests the unpublished changes on the canvas, and says they are not published", async () => {
    const { user } = setup({ agent });
    // A click without a mousedown: d3-drag (React Flow's step drag) reads event.view, which jsdom leaves null.
    fireEvent.click(screen.getByText("main"));
    await user.type(screen.getByLabelText("Instructions"), " Be brief.");
    await user.click(screen.getByRole("button", { name: "Test call" }));
    const panel = screen.getByRole("complementary", { name: "Test call" });
    expect(within(panel).getByRole("note")).toHaveTextContent("This call runs your unpublished changes. They are not saved or published.");
    expect(within(panel).getByText("Testing draft (based on v1)")).toBeVisible();
    expect(within(panel).getByTestId("start-voice-test-btn")).toBeEnabled();
  });

  it("tests the published version, with no note, when nothing changed", async () => {
    const { user } = setup({ agent });
    await user.click(screen.getByRole("button", { name: "Test call" }));
    const panel = screen.getByRole("complementary", { name: "Test call" });
    expect(within(panel).queryByRole("note")).toBeNull();
    expect(within(panel).getByText("Testing v1")).toBeVisible();
  });

  it("will not test a draft that cannot be published yet", async () => {
    const { user } = setup({ agent });
    await user.click(screen.getByRole("button", { name: "Agent settings" }));
    await user.clear(screen.getByLabelText("Agent name"));
    await user.click(screen.getByRole("button", { name: "Test call" }));
    const panel = screen.getByRole("complementary", { name: "Test call" });
    expect(within(panel).getByTestId("start-voice-test-btn")).toBeDisabled();
    expect(within(panel).getByRole("note")).toHaveTextContent(/before you can test your changes: Give the agent a name/i);
  });

  it("lists the changes since the published version in the publish dialog", async () => {
    const { user } = setup({ agent });
    await user.click(screen.getByRole("button", { name: "Add step" }));
    await user.click(screen.getByRole("button", { name: "Publish" }));
    expect(screen.getByRole("dialog", { name: "Publish Front desk v2?" })).toHaveTextContent("Added step New step");
  });
});

describe("AgentEditor payloads", () => {
  it("keeps the agent's tools on the start step when a step is added", async () => {
    const withTools: AgentDTO = {
      ...agent,
      current_version: { ...agent.current_version!, mcp_tools: [{ mcp_server_id: "crm", remote_name: "lookup", schema_hash: "h", required: false }] },
    };
    const { user, onSubmit } = setup({ agent: withTools });
    await user.click(screen.getByRole("button", { name: "Add step" }));
    await user.click(screen.getByRole("button", { name: "Publish" }));
    await user.click(screen.getByRole("button", { name: "Publish v2" }));
    const config = onSubmit.mock.calls[0][0].config as { start: string; steps: { id: string; tools?: string[] }[] };
    expect(config.steps.find((n) => n.id === config.start)!.tools).toEqual(["crm/lookup"]);
  });

  it("leaves the tool selection out of a publish that did not touch it, so a drifted pin cannot block an unrelated edit", async () => {
    const tools = [{ mcp_server_id: "crm", remote_name: "lookup", schema_hash: "h", required: true }];
    const withTools: AgentDTO = { ...agent, current_version: { ...agent.current_version!, mcp_tools: tools } };
    const { user, onSubmit } = setup({ agent: withTools });
    await user.click(screen.getByRole("button", { name: "Add step" }));
    await user.click(screen.getByRole("button", { name: "Publish" }));
    await user.click(screen.getByRole("button", { name: "Publish v2" }));
    expect(onSubmit.mock.calls[0][0]).not.toHaveProperty("mcp_tools");
  });

  it("sends a plain one-step agent as a flat config", async () => {
    const { user, onSubmit } = setup({ agent });
    await user.click(screen.getByRole("button", { name: "Publish" }));
    await user.click(screen.getByRole("button", { name: "Publish v2" }));
    expect(onSubmit.mock.calls[0][0].config).toEqual(flat);
  });
});

describe("AgentEditor focus and keyboard", () => {
  const step = (name: string) => screen.getByText(name).closest(".react-flow__node") as HTMLElement;

  it("opens an existing step from the keyboard, and returns focus to the title when that step is deleted", async () => {
    const { user } = setup({ agent });
    await user.click(screen.getByRole("button", { name: "Add step" }));
    await user.click(screen.getByRole("button", { name: "Done" }));
    step("New step").focus();
    fireEvent.keyDown(step("New step"), { key: "Enter" });
    expect(await screen.findByRole("complementary", { name: "New step" })).toBeVisible();
    await user.click(screen.getByRole("button", { name: "Delete step" }));
    expect(screen.getByRole("heading", { name: "Front desk", level: 1 })).toHaveFocus();
  });

  it("puts focus on a new step's name even when another panel was open", async () => {
    const { user } = setup({ agent });
    await user.click(screen.getByRole("button", { name: "Agent settings" }));
    await user.click(screen.getByRole("button", { name: "Add step" }));
    expect(screen.getByLabelText("Step name")).toHaveFocus();
  });

  it("counts renaming the agent as a change to publish", async () => {
    const { user } = setup({ agent });
    await user.click(screen.getByRole("button", { name: "Agent settings" }));
    await user.clear(screen.getByLabelText("Agent name"));
    await user.type(screen.getByLabelText("Agent name"), "Main line");
    expect(screen.getByText("v1 · unpublished changes")).toBeVisible();
    await user.click(screen.getByRole("button", { name: "Publish" }));
    expect(screen.getByRole("dialog")).toHaveTextContent("Renamed the agent to Main line");
  });
});

describe("AgentEditor start state", () => {
  it("starts a new agent with the name it was given, exactly", () => {
    setup({ initialName: "R&D line ñ" });
    expect(screen.getByRole("heading", { name: "R&D line ñ", level: 1 })).toBeVisible();
  });

  it("ignores the starting name for a stored agent", () => {
    setup({ agent, initialName: "Other" });
    expect(screen.getByRole("heading", { name: "Front desk", level: 1 })).toBeVisible();
  });

  it("opens the test call panel on arrival for an agent that can take calls", () => {
    setup({ agent, initialPanel: "call" });
    expect(screen.getByRole("complementary", { name: "Test call" })).toBeVisible();
  });

  it("does not open the test call panel for an agent that cannot take calls", () => {
    setup({ agent: { ...agent, readiness: { state: "not_ready", reasons: ["missing_configuration"] } }, initialPanel: "call" });
    expect(screen.queryByRole("complementary", { name: "Test call" })).toBeNull();
  });

  it("shows the published version in Agent settings", async () => {
    const { user } = setup({ agent });
    await user.click(screen.getByRole("button", { name: "Agent settings" }));
    expect(screen.getByRole("group", { name: "Published version" })).toHaveTextContent("v1");
  });
});

describe("AgentEditor chrome", () => {
  it("has the breadcrumb in its header bar, the step count and the handoff hint in the toolbar", () => {
    setup({ agent });
    const head = screen.getByRole("heading", { name: "Front desk", level: 1 }).closest("header") as HTMLElement;
    expect(within(head).getByRole("link", { name: "Agents" })).toHaveAttribute("href", "/agents");
    expect(screen.getByText("1 of 8 steps")).toBeVisible();
    expect(screen.getByText(/Drag from the dot on a step to another step to add a handoff/)).toBeVisible();
  });

  it("explains the blank state with the canvas's words", () => {
    setup({ initialName: "After hours" });
    expect(screen.getByText(/that's enough to make a test call/)).toBeVisible();
    expect(screen.queryByText(/Drag from the dot/)).toBeNull();
  });

  it("refits the graph when a panel opens or closes, so the steps stay in view", async () => {
    const { user } = setup({ agent });
    const canvas = screen.getByTestId("agent-canvas");
    const before = canvas.getAttribute("data-fit");
    await user.click(screen.getByRole("button", { name: "Agent settings" }));
    expect(canvas.getAttribute("data-fit")).not.toBe(before);
    const open = canvas.getAttribute("data-fit");
    await user.click(screen.getByRole("button", { name: "Close Agent settings" }));
    expect(canvas.getAttribute("data-fit")).not.toBe(open);
  });
});

describe("AgentEditor MCP tools", () => {
  const crm: MCPServerDTO = { id: "crm", name: "CRM", description: "", endpoint: "https://mcp.example.com", transport: "streamable-http", validation_state: "healthy", latest_discovery_id: "d1", auth_env: "", auth_env_set: false, enabled: true };
  const crmDetail: MCPServerDetailDTO = {
    ...crm,
    discovery: { id: "d1", discovery_hash: "dh", created_at: "", tools: [{ remote_name: "echo", description: "Echo it", input_schema: {}, schema_hash: "h-echo" }, { remote_name: "lookup", description: "Look it up", input_schema: {}, schema_hash: "h-lookup" }] },
  };
  const withMCP = { issueVoiceToken: vi.fn(), getSessionTraces: vi.fn(), getMCPServers: vi.fn().mockResolvedValue([crm]), getMCPServer: vi.fn().mockResolvedValue(crmDetail) } as unknown as ApiClient;
  async function openTools(user: ReturnType<typeof userEvent.setup>) {
    fireEvent.click(screen.getByText("main"));
    await user.click(screen.getByRole("tab", { name: /tools/i }));
  }

  it("chooses a tool for the agent, which makes the draft unpublished and is published with it", async () => {
    const { user, onSubmit } = setup({ agent, client: withMCP });
    await openTools(user);
    await user.click(await screen.findByRole("checkbox", { name: /^echo/ }));
    expect(screen.getByText(/unpublished changes/)).toBeVisible();
    await user.click(screen.getByRole("button", { name: "Publish" }));
    expect(screen.getByRole("dialog")).toHaveTextContent("Added tool crm/echo");
    await user.click(screen.getByRole("button", { name: "Publish v2" }));
    expect(onSubmit.mock.calls[0][0].mcp_tools).toEqual([{ mcp_server_id: "crm", remote_name: "echo", schema_hash: "h-echo", required: false }]);
  });

  it("takes a removed tool off the steps that used it", async () => {
    const tools = [
      { mcp_server_id: "crm", remote_name: "echo", schema_hash: "h-echo", required: false },
      { mcp_server_id: "crm", remote_name: "lookup", schema_hash: "h-lookup", required: false },
    ];
    const stepsAgent: AgentDTO = {
      ...agent,
      current_version: {
        ...agent.current_version!,
        mcp_tools: tools,
        config: { start: "a", steps: [{ id: "a", name: "Reception", tools: ["crm/echo", "crm/lookup"], config: flat }, { id: "b", name: "Billing", tools: ["crm/lookup"], config: { llm: flat.llm, tts: flat.tts } }], handoffs: [] },
      },
    };
    const { user, onSubmit } = setup({ agent: stepsAgent, client: withMCP });
    fireEvent.click(screen.getByText("Reception"));
    await user.click(screen.getByRole("tab", { name: /tools/i }));
    await user.click(await screen.findByRole("checkbox", { name: /^lookup/ }));
    await user.click(screen.getByRole("button", { name: "Publish" }));
    await user.click(screen.getByRole("button", { name: "Publish v2" }));
    const sent = onSubmit.mock.calls[0][0] as { mcp_tools: { remote_name: string }[]; config: { steps: { tools?: string[] }[] } };
    expect(sent.mcp_tools.map((t) => t.remote_name)).toEqual(["echo"]);
    expect(sent.config.steps.map((st) => st.tools)).toEqual([["crm/echo"], undefined]);
  });

  it("marks a tool as running in the background: an unpublished change, listed in the dialog, sent on the start step", async () => {
    const tools = [{ mcp_server_id: "crm", remote_name: "echo", schema_hash: "h-echo", required: false }];
    const withTool: AgentDTO = { ...agent, current_version: { ...agent.current_version!, mcp_tools: tools } };
    const { user, onSubmit } = setup({ agent: withTool, client: withMCP });
    await openTools(user);
    await user.click(await screen.findByRole("checkbox", { name: "Background: echo" }));
    expect(screen.getByText(/unpublished changes/)).toBeVisible();
    await user.click(screen.getByRole("button", { name: "Publish" }));
    expect(screen.getByRole("dialog")).toHaveTextContent("Agent settings: background tools changed");
    await user.click(screen.getByRole("button", { name: "Publish v2" }));
    expect((onSubmit.mock.calls[0][0].config as { async_tools?: string[] }).async_tools).toEqual(["crm/echo"]);
  });

  it("opens a stored background tool ticked", async () => {
    const tools = [{ mcp_server_id: "crm", remote_name: "echo", schema_hash: "h-echo", required: false }];
    const stored: AgentDTO = { ...agent, current_version: { ...agent.current_version!, mcp_tools: tools, config: { ...flat, async_tools: ["crm/echo"] } } };
    const { user } = setup({ agent: stored, client: withMCP });
    await openTools(user);
    expect(await screen.findByRole("checkbox", { name: "Background: echo" })).toBeChecked();
  });
  it("restores a previous version without the call-wide settings it kept on a later step", async () => {
    const legacy = {
      start: "a",
      steps: [
        { id: "a", name: "Front", config: { ...flat, tts: { ...flat.tts, clean_text: false }, failover: { on: ["client_error"] } } },
        { id: "b", name: "Sales", config: { llm: flat.llm, tts: { ...flat.tts, clean_text: false }, failover: { on: ["timeout"] } } },
      ],
      handoffs: [{ id: "e1", from: "a", to: "b", condition: "buy" }],
    };
    const getAgentVersion = vi.fn().mockResolvedValue({ id: "v1", number: 1, schema_version: 1, config: legacy, config_hash: "h", created_at: "", mcp_tools: [] });
    const twice: AgentDTO = { ...agent, current_version: { ...agent.current_version!, number: 2 } };
    const { user, onSubmit } = setup({ agent: twice, client: { ...client, getAgentVersion } as unknown as ApiClient });
    await user.click(screen.getByRole("button", { name: "Agent settings" }));
    await user.click(screen.getByRole("button", { name: "Restore v1…" }));
    await user.click(screen.getByRole("button", { name: "Restore v1" }));
    const sent = onSubmit.mock.calls[0][0] as { config: { steps: { id: string; config: { tts: Record<string, unknown>; failover?: unknown } }[] } };
    const [front, sales] = sent.config.steps;
    expect(front.config.tts.clean_text).toBe(false);
    expect(front.config.failover).toEqual({ on: ["client_error"] });
    expect(sales.config.tts).not.toHaveProperty("clean_text");
    expect(sales.config).not.toHaveProperty("failover");
  });
});
