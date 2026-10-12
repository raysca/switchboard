import { describe, expect, it, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { ApiClient } from "../../../api/client";
import { LiveGraph } from "./LiveGraph";
import { fromAgentConfig } from "./model";

// The canvas is not what is under test: capture the props LiveGraph gives it.
vi.mock("./Canvas", () => ({
  Canvas: (p: { justHandedEdge?: string; traversedEdges?: string[] }) => (
    <div data-testid="canvas" data-just-handed={p.justHandedEdge ?? ""} data-traversed={(p.traversedEdges ?? []).join(",")} />
  ),
}));

const stage = { provider: "google" };
const graph = fromAgentConfig({
  entry: "front",
  nodes: [
    { id: "front", name: "Front", config: { stt: stage, llm: { ...stage, model: "m" }, tts: stage } },
    { id: "sales", name: "Sales", config: { llm: { ...stage, model: "m" }, tts: stage } },
  ],
  edges: [{ id: "e1", from: "front", to: "sales", condition: "buy" }],
});
const span = (name: string, at: string) => ({ id: name, name, started_at: at, attributes: { from_node: "front", to_node: "sales" } });

describe("LiveGraph handoff moment", () => {
  it("marks the edge just taken for a moment, then clears it", async () => {
    const getSessionTraces = vi
      .fn()
      .mockResolvedValueOnce({ session_id: "s", events: [], tool_calls: [], spans: [] })
      .mockResolvedValue({ session_id: "s", events: [], tool_calls: [], spans: [span("handoff.active", "2026-10-11T10:00:02Z")] });
    render(
      <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
        <LiveGraph graph={graph} sessionId="s" client={{ getSessionTraces } as unknown as ApiClient} userSpeaking={false} botSpeaking={false} live pollMs={10} />
      </QueryClientProvider>,
    );
    expect(screen.getByTestId("canvas")).toHaveAttribute("data-just-handed", "");
    await waitFor(() => expect(screen.getByTestId("canvas")).toHaveAttribute("data-just-handed", "e1"));
    await waitFor(() => expect(screen.getByTestId("canvas")).toHaveAttribute("data-just-handed", ""), { timeout: 2000 });
    expect(screen.getByTestId("canvas")).toHaveAttribute("data-traversed", "e1");
  });
});
