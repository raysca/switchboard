import { describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router";
import type { AgentDTO, ApiClient } from "../../api/client";
import { AgentVoiceTest, type AgentVoiceTestProps } from "./AgentVoiceTest";

const stage = { provider: "google" };
const node = (id: string, name: string, extra = {}) => ({
  id,
  name,
  config: { ...(id === "front" ? { stt: stage } : {}), llm: { ...stage, model: "m" }, tts: stage, ...extra },
});
const agent = (config: Record<string, unknown>): AgentDTO => ({
  id: "a1",
  name: "Bot",
  description: "",
  enabled: true,
  archived: false,
  current_version: { id: "v", number: 1, schema_version: 1, config, config_hash: "h", created_at: "", mcp_tools: [] },
  readiness: { state: "ready", reasons: [] },
  created_at: "",
  updated_at: "",
});

const graphAgent = agent({
  entry: "front",
  nodes: [node("front", "Front"), node("sales", "Sales")],
  edges: [{ id: "e1", from: "front", to: "sales", condition: "buy" }],
});

function renderTest(a: AgentDTO, extra: Partial<AgentVoiceTestProps> = {}) {
  const client = { issueVoiceToken: vi.fn(), getSessionTraces: vi.fn() } as unknown as ApiClient;
  return render(
    <QueryClientProvider client={new QueryClient()}>
      <MemoryRouter>
        <AgentVoiceTest agent={a} client={client} keyValue="k" {...extra} />
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

describe("AgentVoiceTest call reporting", () => {
  it("reports the idle call to the page, and NO_CALL when it goes away", () => {
    const onCall = vi.fn();
    const { unmount } = renderTest(graphAgent, { onCall });
    expect(onCall).toHaveBeenLastCalledWith({ sessionId: null, userSpeaking: false, botSpeaking: false, live: false });
    onCall.mockClear();
    unmount();
    expect(onCall).toHaveBeenLastCalledWith({ sessionId: null, userSpeaking: false, botSpeaking: false, live: false });
  });
});
