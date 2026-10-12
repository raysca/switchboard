import { describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";
import { ReactFlowProvider } from "@xyflow/react";
import { Canvas, flowEdges } from "./Canvas";
import { addDraftRoute, fromAgentConfig } from "./model";

const ready = { llm: { provider: "google", model: "gemini" }, tts: { provider: "google", voice: "v" } };
const graph = fromAgentConfig({
  entry: "a",
  nodes: [
    { id: "a", name: "Reception", config: { stt: { provider: "google" }, ...ready } },
    { id: "b", name: "Billing", config: {} },
    { id: "c", name: "Support", config: ready },
  ],
  edges: [{ id: "e1", from: "a", to: "b", condition: "asks about an invoice" }],
});

const card = (name: string) => screen.getByText(name).closest(".agent-node") as HTMLElement;

describe("Canvas step cards", () => {
  it("shows Ready, Needs setup and No route leads here", () => {
    render(<ReactFlowProvider><Canvas graph={graph} /></ReactFlowProvider>);
    expect(card("Reception")).toHaveTextContent("Ready");
    expect(card("Reception")).toHaveTextContent("Start");
    expect(card("Billing")).toHaveTextContent("Needs setup");
    expect(card("Billing")).toHaveClass("agent-node--incomplete");
    expect(card("Support")).toHaveTextContent("No route leads here");
  });

  // React Flow draws no edges in jsdom (no measured handles), so the edge mapping is tested directly.
  it("labels a route with no condition and draws it as a draft", () => {
    const draft = flowEdges(addDraftRoute(graph, "a", "c")).find((e) => e.target === "c")!;
    expect(draft.label).toBe("Set a condition");
    expect(draft.className).toBe("agent-edge--draft");
    expect(flowEdges(graph)[0]).toMatchObject({ label: "asks about an invoice", className: undefined });
  });

  it("shows no setup status on a read-only canvas", () => {
    render(<ReactFlowProvider><Canvas graph={graph} readOnly /></ReactFlowProvider>);
    expect(card("Billing")).not.toHaveTextContent("Needs setup");
  });
});

