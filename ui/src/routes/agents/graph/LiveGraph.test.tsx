import { describe, expect, it, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { ApiClient, SessionTracesDTO } from "../../../api/client";
import { LiveGraph } from "./LiveGraph";
import { fromAgentConfig } from "./model";

const stage = { provider: "google", credential_id: "c" };
const graph = fromAgentConfig({
  entry: "front",
  nodes: [
    { id: "front", name: "Front", config: { stt: stage, llm: { ...stage, model: "m" }, tts: stage } },
    { id: "sales", name: "Sales", config: { llm: { ...stage, model: "m" }, tts: stage } },
  ],
  edges: [{ id: "e1", from: "front", to: "sales", condition: "buy" }],
});
const traces = (...spans: [string, string][]): SessionTracesDTO => ({
  session_id: "s1",
  events: [],
  tool_calls: [],
  spans: spans.map(([name, at], i) => ({ id: `${i}`, name, started_at: at, attributes: { from_node: "front", to_node: "sales" } })),
});

function renderLive(client: Partial<ApiClient>, props: Partial<React.ComponentProps<typeof LiveGraph>> = {}) {
  return render(
    <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
      <LiveGraph graph={graph} sessionId="s1" client={client as ApiClient} userSpeaking={false} botSpeaking={false} live pollMs={10} {...props} />
    </QueryClientProvider>,
  );
}

describe("LiveGraph", () => {
  it("follows the call from the entry agent through a handoff", async () => {
    const getSessionTraces = vi
      .fn()
      .mockResolvedValueOnce(traces())
      .mockResolvedValueOnce(traces(["handoff.preparing", "2026-10-11T10:00:01Z"]))
      .mockResolvedValue(traces(["handoff.preparing", "2026-10-11T10:00:01Z"], ["handoff.active", "2026-10-11T10:00:02Z"]));
    renderLive({ getSessionTraces });

    expect(await screen.findByRole("status")).toHaveTextContent("Front");
    await waitFor(() => expect(screen.getByRole("status")).toHaveTextContent(/preparing Sales/i));
    await waitFor(() => expect(screen.getByRole("status")).toHaveTextContent(/Sales is answering/i));
    const trail = screen.getByRole("list", { name: /path taken/i });
    expect(trail).toHaveTextContent("Front");
    expect(trail).toHaveTextContent("Sales");
  });

  it("replays a finished session once, without polling", async () => {
    const getSessionTraces = vi.fn().mockResolvedValue(traces(["handoff.active", "2026-10-11T10:00:02Z"]));
    renderLive({ getSessionTraces }, { live: false });
    await waitFor(() => expect(screen.getByRole("status")).toHaveTextContent(/Sales is answering/i));
    await new Promise((r) => setTimeout(r, 60));
    expect(getSessionTraces).toHaveBeenCalledTimes(1);
  });

  it("shows what the active agent is doing from the voice state", async () => {
    renderLive({ getSessionTraces: vi.fn().mockResolvedValue(traces()) }, { botSpeaking: true });
    await waitFor(() => expect(screen.getByRole("status")).toHaveTextContent(/speaking/i));
  });

  it("waits for a session before asking for traces", () => {
    const getSessionTraces = vi.fn();
    renderLive({ getSessionTraces }, { sessionId: null });
    expect(getSessionTraces).not.toHaveBeenCalled();
    expect(screen.getByRole("status")).toHaveTextContent("Front");
  });
});
