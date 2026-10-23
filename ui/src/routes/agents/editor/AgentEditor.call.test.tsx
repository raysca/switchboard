import { describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router";
import { ReactFlowProvider } from "@xyflow/react";
import { useEffect } from "react";
import type { AgentDTO, ApiClient } from "../../../api/client";
import type { CallInfo } from "./useCallView";
import { AgentEditor } from "./AgentEditor";

// A voice test that reports a live call at once (a real one needs a microphone and a server).
vi.mock("../AgentVoiceTest", () => ({
  AgentVoiceTest: ({ onCall }: { onCall?: (c: CallInfo) => void }) => {
    useEffect(() => onCall?.({ sessionId: "s1", userSpeaking: false, botSpeaking: true, live: true }), [onCall]);
    return <p>Call in progress</p>;
  },
}));

const flat = { stt: { provider: "google" }, llm: { provider: "google", model: "gemini" }, tts: { provider: "google", voice: "aoede" } };
const agent: AgentDTO = {
  id: "a1", name: "Front desk", description: "", enabled: true, archived: false,
  current_version: { id: "v1", number: 1, schema_version: 1, config: flat, config_hash: "h", created_at: "", mcp_tools: [] },
  readiness: { state: "ready", reasons: [] }, created_at: "", updated_at: "",
};
const client = { getSessionTraces: vi.fn().mockResolvedValue({ session_id: "s1", events: [], tool_calls: [], spans: [] }) } as unknown as ApiClient;

describe("AgentEditor during a test call", () => {
  it("keeps the call: panels and editing that would replace the call panel are off while it runs", async () => {
    const user = userEvent.setup();
    render(
      <QueryClientProvider client={new QueryClient()}>
        <MemoryRouter><ReactFlowProvider>
          <AgentEditor agent={agent} providers={[]} client={client} keyValue="k" pending={false} onSubmit={vi.fn()} onCancel={vi.fn()} />
        </ReactFlowProvider></MemoryRouter>
      </QueryClientProvider>,
    );
    await user.click(screen.getByRole("button", { name: "Test call" }));
    expect(await screen.findByText("Call in progress")).toBeVisible();
    expect(screen.getByRole("button", { name: "Agent settings" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "Add step" })).toBeDisabled();
  });
});
