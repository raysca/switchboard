import { describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router";
import { ReactFlowProvider } from "@xyflow/react";
import type { AgentDTO, ApiClient, ProviderStatus } from "../../../api/client";
import { FlowEditor, type FlowEditorProps } from "./FlowEditor";

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

function setup(props: Partial<FlowEditorProps> = {}) {
  const onSubmit = vi.fn();
  render(
    <QueryClientProvider client={new QueryClient()}>
      <MemoryRouter>
        <ReactFlowProvider>
          <FlowEditor agent={null} providers={providers} client={client} keyValue="k" pending={false} onSubmit={onSubmit} onCancel={vi.fn()} {...props} />
        </ReactFlowProvider>
      </MemoryRouter>
    </QueryClientProvider>,
  );
  return { onSubmit, user: userEvent.setup() };
}

describe("FlowEditor", () => {
  it("starts a new flow blank and opens the first step's panel on Basics, name focused", async () => {
    const { user } = setup();
    expect(screen.getByRole("heading", { name: "New flow" })).toBeVisible();
    expect(screen.getByRole("status")).toHaveTextContent("No steps yet");
    await user.click(screen.getByRole("button", { name: "Add first step" }));
    expect(screen.getByRole("complementary", { name: "New step" })).toBeVisible();
    expect(screen.getByLabelText("Step name")).toHaveFocus();
    expect(screen.getByRole("status")).toHaveTextContent("1 step needs setup");
  });

  it("cannot test a flow that was never published", () => {
    setup();
    expect(screen.getByRole("button", { name: "Test call" })).toBeDisabled();
  });

  it("blocks publish until every step and the flow are set up, then sends the graph", async () => {
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
    await user.click(screen.getByRole("button", { name: "Flow settings" }));
    await user.type(screen.getByLabelText("Flow name"), "After hours");
    await user.selectOptions(screen.getByLabelText("STT provider"), "google");
    await user.click(screen.getByRole("button", { name: "Publish" }));
    await user.click(screen.getByRole("button", { name: "Publish v1" }));
    // A named step goes as a graph, so its name is kept.
    const sent = onSubmit.mock.calls[0][0] as { name: string; config: { nodes: { name: string; config: { llm: { provider: string } } }[] } };
    expect(sent.name).toBe("After hours");
    expect(sent.config.nodes).toEqual([expect.objectContaining({ name: "New step", config: expect.objectContaining({ llm: expect.objectContaining({ provider: "google" }) }) })]);
  });

  it("says the test call runs the published version while the draft differs", async () => {
    const { user } = setup({ agent });
    // A click without a mousedown: d3-drag (React Flow's node drag) reads event.view, which jsdom leaves null.
    fireEvent.click(screen.getByText("main"));
    await user.type(screen.getByLabelText("Instructions"), " Be brief.");
    await user.click(screen.getByRole("button", { name: "Test call" }));
    const panel = screen.getByRole("complementary", { name: "Test call" });
    expect(within(panel).getByRole("note")).toHaveTextContent("This call runs v1, the published version. Publish to hear your changes.");
    expect(within(panel).getByTestId("start-voice-test-btn")).toBeVisible();
  });

  it("lists the changes since the published version in the publish dialog", async () => {
    const { user } = setup({ agent });
    await user.click(screen.getByRole("button", { name: "Add step" }));
    await user.click(screen.getByRole("button", { name: "Publish" }));
    expect(screen.getByRole("dialog", { name: "Publish Front desk v2?" })).toHaveTextContent("Added step New step");
  });
});

describe("FlowEditor payloads", () => {
  it("keeps the flow's tools on the start step when a step is added", async () => {
    const withTools: AgentDTO = {
      ...agent,
      current_version: { ...agent.current_version!, mcp_tools: [{ mcp_server_id: "crm", remote_name: "lookup", schema_hash: "h", required: false }] },
    };
    const { user, onSubmit } = setup({ agent: withTools });
    await user.click(screen.getByRole("button", { name: "Add step" }));
    await user.click(screen.getByRole("button", { name: "Publish" }));
    await user.click(screen.getByRole("button", { name: "Publish v2" }));
    const config = onSubmit.mock.calls[0][0].config as { entry: string; nodes: { id: string; tools?: string[] }[] };
    expect(config.nodes.find((n) => n.id === config.entry)!.tools).toEqual(["crm/lookup"]);
  });

  it("sends a plain one-step flow as a flat config", async () => {
    const { user, onSubmit } = setup({ agent });
    await user.click(screen.getByRole("button", { name: "Publish" }));
    await user.click(screen.getByRole("button", { name: "Publish v2" }));
    expect(onSubmit.mock.calls[0][0].config).toEqual(flat);
  });
});

describe("FlowEditor focus and keyboard", () => {
  const node = (name: string) => screen.getByText(name).closest(".react-flow__node") as HTMLElement;

  it("opens an existing step from the keyboard, and returns focus to the title when that step is deleted", async () => {
    const { user } = setup({ agent });
    await user.click(screen.getByRole("button", { name: "Add step" }));
    await user.click(screen.getByRole("button", { name: "Done" }));
    node("New step").focus();
    fireEvent.keyDown(node("New step"), { key: "Enter" });
    expect(await screen.findByRole("complementary", { name: "New step" })).toBeVisible();
    await user.click(screen.getByRole("button", { name: "Delete step" }));
    expect(screen.getByRole("heading", { name: "Front desk", level: 1 })).toHaveFocus();
  });

  it("puts focus on a new step's name even when another panel was open", async () => {
    const { user } = setup({ agent });
    await user.click(screen.getByRole("button", { name: "Flow settings" }));
    await user.click(screen.getByRole("button", { name: "Add step" }));
    expect(screen.getByLabelText("Step name")).toHaveFocus();
  });

  it("counts renaming the flow as a change to publish", async () => {
    const { user } = setup({ agent });
    await user.click(screen.getByRole("button", { name: "Flow settings" }));
    await user.clear(screen.getByLabelText("Flow name"));
    await user.type(screen.getByLabelText("Flow name"), "Main line");
    expect(screen.getByText("v1 · unpublished changes")).toBeVisible();
    await user.click(screen.getByRole("button", { name: "Publish" }));
    expect(screen.getByRole("dialog")).toHaveTextContent("Renamed the flow to Main line");
  });
});

describe("FlowEditor start state", () => {
  it("starts a new flow with the name it was given, exactly", () => {
    setup({ initialName: "R&D line ñ" });
    expect(screen.getByRole("heading", { name: "R&D line ñ", level: 1 })).toBeVisible();
  });

  it("ignores the starting name for a stored flow", () => {
    setup({ agent, initialName: "Other" });
    expect(screen.getByRole("heading", { name: "Front desk", level: 1 })).toBeVisible();
  });

  it("opens the test call panel on arrival for a flow that can take calls", () => {
    setup({ agent, initialPanel: "call" });
    expect(screen.getByRole("complementary", { name: "Test call" })).toBeVisible();
  });

  it("does not open the test call panel for a flow that cannot take calls", () => {
    setup({ agent: { ...agent, readiness: { state: "not_ready", reasons: ["missing_configuration"] } }, initialPanel: "call" });
    expect(screen.queryByRole("complementary", { name: "Test call" })).toBeNull();
  });

  it("shows the published version in Flow settings", async () => {
    const { user } = setup({ agent });
    await user.click(screen.getByRole("button", { name: "Flow settings" }));
    expect(screen.getByRole("group", { name: "Published version" })).toHaveTextContent("v1");
  });
});

describe("FlowEditor chrome", () => {
  it("has the breadcrumb in its header bar, the step count and the route hint in the toolbar", () => {
    setup({ agent });
    const head = screen.getByRole("heading", { name: "Front desk", level: 1 }).closest("header") as HTMLElement;
    expect(within(head).getByRole("link", { name: "Flows" })).toHaveAttribute("href", "/flows");
    expect(screen.getByText("1 of 8 steps")).toBeVisible();
    expect(screen.getByText(/Drag from the dot on a step's edge to add a route/)).toBeVisible();
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
    await user.click(screen.getByRole("button", { name: "Flow settings" }));
    expect(canvas.getAttribute("data-fit")).not.toBe(before);
    const open = canvas.getAttribute("data-fit");
    await user.click(screen.getByRole("button", { name: "Close Flow settings" }));
    expect(canvas.getAttribute("data-fit")).not.toBe(open);
  });
});
