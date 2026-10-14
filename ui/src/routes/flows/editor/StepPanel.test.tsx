import { describe, expect, it, vi } from "vitest";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useState } from "react";
import type { ProviderStatus } from "../../../api/client";
import { StepPanel } from "./StepPanel";
import { addDraftRoute, fromFlowConfig, type FlowDraft, type StepTab } from "./model";

const svc = { available: true, custom: true, models: [{ id: "gemini", label: "Gemini", default: true }], voices: [{ id: "aoede", label: "Aoede", default: true }] };
const providers: ProviderStatus[] = [
  { id: "google", display_name: "Google", capabilities: ["llm", "stt", "tts"], detected: true, env: ["GOOGLE_API_KEY"], services: { stt: svc, llm: svc, tts: svc } },
];
const ready = { llm: { provider: "google", model: "gemini" }, tts: { provider: "google", voice: "aoede" } };
const start = () =>
  fromFlowConfig({
    start: "a",
    steps: [
      { id: "a", name: "Reception", config: { stt: { provider: "google" }, ...ready } },
      { id: "b", name: "Billing", config: {} },
    ],
    routes: [],
  });

function Harness({
  initial = start(),
  stepId = "b",
  initialTab = "basics" as StepTab,
  flowTools = [] as string[],
  toolPicker = undefined as React.ReactNode,
  onChange = vi.fn(),
}: {
  initial?: FlowDraft;
  stepId?: string;
  initialTab?: StepTab;
  flowTools?: string[];
  toolPicker?: React.ReactNode;
  onChange?: (g: FlowDraft) => void;
}) {
  const [graph, setGraph] = useState<FlowDraft>(initial);
  const [tab, setTab] = useState<StepTab>(initialTab);
  const [focusRoute, setFocusRoute] = useState<string | undefined>();
  return (
    <div style={{ position: "relative" }}>
      <StepPanel
        graph={graph}
        stepId={stepId}
        providers={providers}
        flowTools={flowTools}
        toolPicker={toolPicker}
        tab={tab}
        onTab={setTab}
        focusRoute={focusRoute}
        onFocusRoute={setFocusRoute}
        wide={false}
        onToggleWide={vi.fn()}
        onClose={vi.fn()}
        onChange={(g) => {
          setGraph(g);
          onChange(g);
        }}
      />
    </div>
  );
}
const lastGraph = (fn: ReturnType<typeof vi.fn>) => fn.mock.calls.at(-1)![0] as FlowDraft;

describe("StepPanel", () => {
  it("lists what a new step needs and marks the Basics tab", () => {
    render(<Harness />);
    expect(screen.getByRole("tab", { name: /basics.*needs attention/i })).toHaveAttribute("aria-selected", "true");
    const note = screen.getByRole("note");
    expect(note).toHaveTextContent("Choose a model.");
    expect(note).toHaveTextContent("Choose a voice.");
  });

  it("finishes a step from Basics: name, instructions, model, voice", async () => {
    const user = userEvent.setup();
    const onChange = vi.fn();
    render(<Harness onChange={onChange} />);
    await user.clear(screen.getByLabelText("Step name"));
    await user.type(screen.getByLabelText("Step name"), "Billing desk");
    await user.type(screen.getByLabelText("Instructions"), "Explain invoices.");
    await user.selectOptions(screen.getByLabelText("LLM provider"), "google");
    await user.selectOptions(screen.getByLabelText("TTS provider"), "google");
    expect(screen.queryByRole("note")).toBeNull();
    const b = lastGraph(onChange).steps.find((n) => n.id === "b")!;
    expect(b.name).toBe("Billing desk");
    expect(b.config).toMatchObject({ llm: { provider: "google", model: "gemini", system_prompt: "Explain invoices." }, tts: { provider: "google", voice: "aoede" } });
  });

  it("lets the start step choose the call's speech-to-text provider, right beside its model and voice", async () => {
    const user = userEvent.setup();
    const onChange = vi.fn();
    render(<Harness stepId="a" onChange={onChange} />);
    const listening = screen.getByRole("group", { name: "Listening" });
    await user.selectOptions(within(listening).getByLabelText("STT provider"), "google");
    const a = lastGraph(onChange).steps.find((n) => n.id === "a")!;
    expect(a.config.stt).toMatchObject({ provider: "google", model: "gemini" });
  });

  it("says where listening is set on a step that is not the start step, and offers no STT choice there", () => {
    render(<Harness stepId="b" />);
    expect(screen.queryByLabelText("STT provider")).toBeNull();
    expect(screen.getByText(/Listening is set once for the whole call, on the start step \(Reception\)\./)).toBeVisible();
  });

  it("has a greeting field on the start step, with its own hint, and clearing it removes the key", async () => {
    const user = userEvent.setup();
    const onChange = vi.fn();
    render(<Harness stepId="a" onChange={onChange} />);
    const field = screen.getByLabelText("Greeting");
    expect(screen.getByText("Said word for word as soon as the call connects. Leave empty to let the caller speak first.")).toBeVisible();
    await user.type(field, "Hello!");
    expect(lastGraph(onChange).steps.find((n) => n.id === "a")!.config.greeting).toBe("Hello!");
    await user.clear(field);
    expect(lastGraph(onChange).steps.find((n) => n.id === "a")!.config).not.toHaveProperty("greeting");
  });

  it("has a greeting field on another step, worded for a transfer", async () => {
    const user = userEvent.setup();
    const onChange = vi.fn();
    render(<Harness stepId="b" onChange={onChange} />);
    expect(screen.getByText("Said word for word when a call is transferred here, in this step's voice. Leave empty to let the model greet the caller.")).toBeVisible();
    await user.type(screen.getByLabelText("Greeting"), "Billing here.");
    expect(lastGraph(onChange).steps.find((n) => n.id === "b")!.config.greeting).toBe("Billing here.");
  });

  it("counts bytes near the limit and flags a greeting that is too long", async () => {
    const user = userEvent.setup();
    render(<Harness stepId="a" />);
    const field = screen.getByLabelText("Greeting");
    await user.click(field);
    await user.paste("é".repeat(230)); // 460 bytes
    expect(screen.getByText("460 of 500 bytes")).toBeVisible();
    await user.paste("é".repeat(30)); // 520 bytes
    expect(screen.getByRole("note")).toHaveTextContent("Greeting is over 500 bytes.");
    expect(screen.getByRole("tab", { name: /basics.*needs attention/i })).toBeInTheDocument();
  });

  it("adds a route from the keyboard, focuses its condition, and flags it until filled", async () => {
    const user = userEvent.setup();
    render(<Harness stepId="a" initialTab="routes" />);
    await user.selectOptions(screen.getByLabelText("Route to"), "b");
    await user.click(screen.getByRole("button", { name: "Add route" }));
    const condition = screen.getByLabelText("Condition for the route to Billing");
    expect(condition).toHaveFocus();
    expect(screen.getByRole("tab", { name: /routes.*needs attention/i })).toBeInTheDocument();
    await user.type(condition, "asks about an invoice");
    expect(screen.getByRole("tab", { name: /routes/i })).not.toHaveAccessibleName(/needs attention/i);
    await user.click(screen.getByRole("button", { name: "Remove the route to Billing" }));
    expect(screen.queryByLabelText("Condition for the route to Billing")).toBeNull();
  });

  it("routes to a brand-new step in one go", async () => {
    const user = userEvent.setup();
    const onChange = vi.fn();
    render(<Harness stepId="a" initialTab="routes" onChange={onChange} />);
    await user.click(screen.getByRole("button", { name: "Route to a new step" }));
    const g = lastGraph(onChange);
    expect(g.steps.map((n) => n.name)).toEqual(["Reception", "Billing", "New step"]);
    expect(g.routes).toEqual([expect.objectContaining({ from: "a", condition: "" })]);
    expect(screen.getByLabelText("Condition for the route to New step")).toHaveFocus();
  });

  it("shows the flow's tool picker above the step's own choice, and says when no tool is chosen yet", () => {
    render(<Harness stepId="a" initialTab="tools" toolPicker={<p>PICKER</p>} />);
    const section = screen.getByRole("group", { name: "Flow tools" });
    expect(within(section).getByText("PICKER")).toBeVisible();
    expect(within(section).getByText(/Shared by every step/)).toBeVisible();
    expect(screen.getByText("No tools are chosen for this flow yet.")).toBeVisible();
    expect(screen.queryByText(/Choose MCP tools for the flow first/)).toBeNull();
  });

  it("chooses which of the flow's tools a step can call", async () => {
    const user = userEvent.setup();
    const onChange = vi.fn();
    render(<Harness stepId="a" initialTab="tools" flowTools={["crm/lookup", "crm/verify"]} onChange={onChange} />);
    await user.click(screen.getByLabelText("crm/verify"));
    expect(lastGraph(onChange).steps[0].tools).toContain("crm/verify");
    expect(screen.getByRole("tab", { name: /tools/i })).toHaveTextContent("1");
  });

  it("lets a step be given the end_call tool", async () => {
    const user = userEvent.setup();
    const onChange = vi.fn();
    render(<Harness stepId="a" initialTab="tools" onChange={onChange} />);
    await user.click(screen.getByRole("checkbox", { name: /can end the call/i }));
    expect(lastGraph(onChange).steps[0].end_call).toBe(true);
    expect(screen.getByRole("tab", { name: /tools/i })).toHaveTextContent("1");
  });

  it("disables the end_call tool on a one-step flow and says why", () => {
    const one = fromFlowConfig({ start: "a", steps: [{ id: "a", name: "Reception", config: {} }], routes: [] });
    render(<Harness initial={one} stepId="a" initialTab="tools" />);
    expect(screen.getByRole("checkbox", { name: /can end the call/i })).toBeDisabled();
    expect(screen.getByText(/needs a second step/i)).toBeInTheDocument();
  });

  it("shows end_call unchecked and disabled on a one-step flow that still carries the flag", () => {
    const one = fromFlowConfig({ start: "a", steps: [{ id: "a", name: "Reception", end_call: true, config: {} }], routes: [] });
    render(<Harness initial={one} stepId="a" initialTab="tools" />);
    const box = screen.getByRole("checkbox", { name: /can end the call/i });
    expect(box).not.toBeChecked();
    expect(box).toBeDisabled();
  });

  it("offers Delete step and Make start step only on other steps", () => {
    const { unmount } = render(<Harness stepId="a" />);
    expect(screen.queryByRole("button", { name: "Delete step" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Make start step" })).toBeNull();
    unmount();
    render(<Harness stepId="b" />);
    expect(screen.getByRole("button", { name: "Delete step" })).toBeVisible();
    expect(screen.getByRole("button", { name: "Make start step" })).toBeVisible();
  });

  it("moves the flow settings when another step becomes the start", async () => {
    const user = userEvent.setup();
    const onChange = vi.fn();
    render(<Harness stepId="b" onChange={onChange} />);
    await user.click(screen.getByRole("button", { name: "Make start step" }));
    const g = lastGraph(onChange);
    expect(g.start).toBe("b");
    expect(g.steps.find((n) => n.id === "b")!.config.stt).toEqual({ provider: "google" });
    expect(g.steps.find((n) => n.id === "a")!.config.stt).toBeUndefined();
  });

  it("focuses the condition of a route drawn on the graph", () => {
    const g = addDraftRoute(start(), "a", "b");
    function Drawn() {
      const [graph, setGraph] = useState(g);
      return (
        <div style={{ position: "relative" }}>
          <StepPanel graph={graph} stepId="a" providers={providers} flowTools={[]} tab="routes" onTab={vi.fn()} focusRoute={graph.routes[0].id} onFocusRoute={vi.fn()} wide={false} onToggleWide={vi.fn()} onClose={vi.fn()} onChange={setGraph} />
        </div>
      );
    }
    render(<Drawn />);
    expect(screen.getByLabelText("Condition for the route to Billing")).toHaveFocus();
    expect(within(screen.getByRole("complementary")).getByRole("heading", { name: "Reception" })).toBeInTheDocument();
  });
});
