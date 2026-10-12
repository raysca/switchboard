import { describe, expect, it, vi } from "vitest";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useState } from "react";
import type { ProviderStatus } from "../../../api/client";
import { StepPanel } from "./StepPanel";
import { addDraftRoute, fromAgentConfig, type GraphDraft, type StepTab } from "./model";

const svc = { available: true, custom: true, models: [{ id: "gemini", label: "Gemini", default: true }], voices: [{ id: "aoede", label: "Aoede", default: true }] };
const providers: ProviderStatus[] = [
  { id: "google", display_name: "Google", capabilities: ["llm", "stt", "tts"], detected: true, env: ["GOOGLE_API_KEY"], services: { stt: svc, llm: svc, tts: svc } },
];
const ready = { llm: { provider: "google", model: "gemini" }, tts: { provider: "google", voice: "aoede" } };
const start = () =>
  fromAgentConfig({
    entry: "a",
    nodes: [
      { id: "a", name: "Reception", config: { stt: { provider: "google" }, ...ready } },
      { id: "b", name: "Billing", config: {} },
    ],
    edges: [],
  });

function Harness({
  initial = start(),
  stepId = "b",
  initialTab = "basics" as StepTab,
  flowTools = [] as string[],
  onChange = vi.fn(),
}: {
  initial?: GraphDraft;
  stepId?: string;
  initialTab?: StepTab;
  flowTools?: string[];
  onChange?: (g: GraphDraft) => void;
}) {
  const [graph, setGraph] = useState<GraphDraft>(initial);
  const [tab, setTab] = useState<StepTab>(initialTab);
  const [focusRoute, setFocusRoute] = useState<string | undefined>();
  return (
    <div style={{ position: "relative" }}>
      <StepPanel
        graph={graph}
        stepId={stepId}
        providers={providers}
        flowTools={flowTools}
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
const lastGraph = (fn: ReturnType<typeof vi.fn>) => fn.mock.calls.at(-1)![0] as GraphDraft;

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
    const b = lastGraph(onChange).nodes.find((n) => n.id === "b")!;
    expect(b.name).toBe("Billing desk");
    expect(b.config).toMatchObject({ llm: { provider: "google", model: "gemini", system_prompt: "Explain invoices." }, tts: { provider: "google", voice: "aoede" } });
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
    expect(g.nodes.map((n) => n.name)).toEqual(["Reception", "Billing", "New step"]);
    expect(g.edges).toEqual([expect.objectContaining({ from: "a", condition: "" })]);
    expect(screen.getByLabelText("Condition for the route to New step")).toHaveFocus();
  });

  it("chooses which of the flow's tools a step can call", async () => {
    const user = userEvent.setup();
    const onChange = vi.fn();
    render(<Harness stepId="a" initialTab="tools" flowTools={["crm/lookup", "crm/verify"]} onChange={onChange} />);
    await user.click(screen.getByLabelText("crm/verify"));
    expect(lastGraph(onChange).nodes[0].tools).toContain("crm/verify");
    expect(screen.getByRole("tab", { name: /tools/i })).toHaveTextContent("1");
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
    expect(g.entry).toBe("b");
    expect(g.nodes.find((n) => n.id === "b")!.config.stt).toEqual({ provider: "google" });
    expect(g.nodes.find((n) => n.id === "a")!.config.stt).toBeUndefined();
  });

  it("focuses the condition of a route drawn on the graph", () => {
    const g = addDraftRoute(start(), "a", "b");
    function Drawn() {
      const [graph, setGraph] = useState(g);
      return (
        <div style={{ position: "relative" }}>
          <StepPanel graph={graph} stepId="a" providers={providers} flowTools={[]} tab="routes" onTab={vi.fn()} focusRoute={graph.edges[0].id} onFocusRoute={vi.fn()} wide={false} onToggleWide={vi.fn()} onClose={vi.fn()} onChange={setGraph} />
        </div>
      );
    }
    render(<Drawn />);
    expect(screen.getByLabelText("Condition for the route to Billing")).toHaveFocus();
    expect(within(screen.getByRole("complementary")).getByRole("heading", { name: "Reception" })).toBeInTheDocument();
  });
});
