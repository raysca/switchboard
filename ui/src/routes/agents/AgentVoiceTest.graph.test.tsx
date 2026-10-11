import { describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router";
import type { AgentDTO, ApiClient } from "../../api/client";
import { AgentVoiceTest } from "./AgentVoiceTest";

const stage = { provider: "google", credential_id: "c" };
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

function renderTest(a: AgentDTO) {
  const client = { issueVoiceToken: vi.fn(), getSessionTraces: vi.fn() } as unknown as ApiClient;
  return render(
    <QueryClientProvider client={new QueryClient()}>
      <MemoryRouter>
        <AgentVoiceTest agent={a} client={client} keyValue="k" />
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

describe("AgentVoiceTest graph view", () => {
  it("shows the path through the graph for a multi-node agent", () => {
    renderTest(
      agent({
        entry: "front",
        nodes: [node("front", "Front"), node("sales", "Sales")],
        edges: [{ id: "e1", from: "front", to: "sales", condition: "buy" }],
      }),
    );
    expect(screen.getByRole("list", { name: /path taken/i })).toHaveTextContent("Front");
  });

  it("shows no graph for a plain agent", () => {
    renderTest(agent({ stt: stage, llm: { ...stage, model: "m" }, tts: stage }));
    expect(screen.queryByRole("list", { name: /path taken/i })).toBeNull();
  });
});
