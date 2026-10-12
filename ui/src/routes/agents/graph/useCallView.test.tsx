import { describe, expect, it, vi } from "vitest";
import { renderHook, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { ReactNode } from "react";
import type { ApiClient, SessionTracesDTO } from "../../../api/client";
import { fromAgentConfig } from "./model";
import { useCallView, type CallInfo } from "./useCallView";

const stage = { provider: "google" };
const graph = fromAgentConfig({
  entry: "front",
  nodes: [
    { id: "front", name: "Front", config: { stt: stage, llm: { ...stage, model: "m" }, tts: stage } },
    { id: "sales", name: "Sales", config: { llm: { ...stage, model: "m" }, tts: stage } },
  ],
  edges: [{ id: "e1", from: "front", to: "sales", condition: "buy" }],
});
const handedOff: SessionTracesDTO = {
  session_id: "s1",
  events: [],
  tool_calls: [],
  spans: [{ id: "1", name: "handoff.active", started_at: "2026-10-11T10:00:02Z", attributes: { from_node: "front", to_node: "sales" } }],
};

describe("useCallView", () => {
  it("clears the just-taken route after its beat even when the speaker changes meanwhile", async () => {
    const getSessionTraces = vi.fn().mockResolvedValueOnce({ ...handedOff, spans: [] }).mockResolvedValue(handedOff);
    const client = { getSessionTraces } as unknown as ApiClient;
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const wrapper = ({ children }: { children: ReactNode }) => <QueryClientProvider client={qc}>{children}</QueryClientProvider>;
    const call = (userSpeaking: boolean): CallInfo => ({ sessionId: "s1", userSpeaking, botSpeaking: false, live: true });
    const { result, rerender } = renderHook(({ c }) => useCallView(graph, c, client, 20), { wrapper, initialProps: { c: call(false) } });
    await waitFor(() => expect(result.current.justHanded).toBe("e1"));
    rerender({ c: call(true) }); // the caller starts speaking right after the route is taken
    await waitFor(() => expect(result.current.justHanded).toBeUndefined(), { timeout: 1500 });
  });
});
