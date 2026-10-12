import { describe, expect, it, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { ReactFlowProvider } from "@xyflow/react";
import { MemoryRouter, Route, Routes } from "react-router";
import type { AgentDTO, ApiClient, ProviderStatus } from "../../api/client";
import { ApiError } from "../../api/client";
import { FlowPage } from "./FlowPage";

const svc = { available: true, custom: true, models: [{ id: "gemini", label: "Gemini", default: true }], voices: [{ id: "aoede", label: "Aoede", default: true }], languages: [{ id: "en", label: "English", default: true }] };
const providers: ProviderStatus[] = [{ id: "google", display_name: "Google", capabilities: ["llm", "stt", "tts"], detected: true, env: [], services: { stt: svc, llm: svc, tts: svc } }];
const flat = { stt: { provider: "google" }, llm: { provider: "google", model: "gemini" }, tts: { provider: "google", voice: "aoede" } };
const flow = (over: Partial<AgentDTO> = {}): AgentDTO => ({
  id: "a1",
  name: "Front desk",
  description: "",
  enabled: true,
  archived: false,
  current_version: { id: "v1", number: 1, schema_version: 1, config: flat, config_hash: "h1", created_at: "", mcp_tools: [] },
  readiness: { state: "ready", reasons: [] },
  created_at: "",
  updated_at: "",
  ...over,
});

function setup(client: Partial<ApiClient>, path: string, key: string | null = "k") {
  const full = {
    getAgents: vi.fn().mockResolvedValue([flow()]),
    getProviders: vi.fn().mockResolvedValue(providers),
    getSessions: vi.fn().mockResolvedValue([]),
    getSessionTraces: vi.fn(),
    issueVoiceToken: vi.fn(),
    ...client,
  } as unknown as ApiClient;
  render(
    <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
      <MemoryRouter initialEntries={[path]}>
        <ReactFlowProvider>
          <Routes>
            <Route path="/flows" element={<p>the list</p>} />
            <Route path="/flows/new" element={<FlowPage client={full} keyValue={key} />} />
            <Route path="/flows/:flowId" element={<FlowPage client={full} keyValue={key} />} />
          </Routes>
        </ReactFlowProvider>
      </MemoryRouter>
    </QueryClientProvider>,
  );
  return { client: full, user: userEvent.setup() };
}

describe("FlowPage", () => {
  it("asks for a management key first", () => {
    setup({}, "/flows/a1", null);
    expect(screen.getByText(/Enter a management key/)).toBeVisible();
  });

  it("opens a stored flow in the editor under a breadcrumb back to the list", async () => {
    setup({}, "/flows/a1");
    expect(await screen.findByRole("heading", { name: "Front desk", level: 1 })).toBeVisible();
    expect(screen.getByRole("link", { name: "Flows" })).toHaveAttribute("href", "/flows");
  });

  it("opens a new flow under the name it was given, exactly", async () => {
    setup({}, "/flows/new?name=R%26D%20line%20%C3%B1");
    expect(await screen.findByRole("heading", { name: "R&D line ñ", level: 1 })).toBeVisible();
    expect(screen.getByRole("button", { name: "Add first step" })).toBeVisible();
  });

  it("says so, with a way back, for a flow that does not exist", async () => {
    setup({}, "/flows/nope");
    expect(await screen.findByText("That flow was not found.")).toBeVisible();
    expect(screen.getByRole("link", { name: "Back to flows" })).toHaveAttribute("href", "/flows");
    expect(screen.queryByRole("button", { name: "Publish" })).toBeNull();
  });

  it("treats an archived flow as not found", async () => {
    setup({ getAgents: vi.fn().mockResolvedValue([flow({ archived: true })]) }, "/flows/a1");
    expect(await screen.findByText("That flow was not found.")).toBeVisible();
  });

  it("opens on the test call panel for ?panel=test", async () => {
    setup({}, "/flows/a1?panel=test");
    expect(await screen.findByRole("complementary", { name: "Test call" })).toBeVisible();
  });

  it("publishes an edit, stays in the editor, and confirms the version with calls still on the old one", async () => {
    const updated = flow({ current_version: { id: "v2", number: 2, schema_version: 1, config: flat, config_hash: "h2", created_at: "", mcp_tools: [] } });
    const updateAgent = vi.fn().mockResolvedValue(updated);
    const { user } = setup(
      {
        updateAgent,
        // After publishing, the server's list shows the new version, as a real refetch would.
        getAgents: vi.fn().mockResolvedValueOnce([flow()]).mockResolvedValue([updated]),
        getSessions: vi.fn().mockResolvedValue([
          { id: "s1", agent_id: "a1", agent_version_id: "v1", config_hash: "h1", started_at: "", status: "active", turn_count: 0, safe_summary: "" },
        ]),
      },
      "/flows/a1",
    );
    await screen.findByRole("heading", { name: "Front desk", level: 1 });
    // A rename is a publishable change on its own.
    await user.click(screen.getByRole("button", { name: "Flow settings" }));
    await user.type(screen.getByLabelText("Flow name"), " line");
    await user.click(screen.getByRole("button", { name: "Publish" }));
    await user.click(screen.getByRole("button", { name: "Publish v2" }));
    expect(updateAgent).toHaveBeenCalledWith("a1", expect.objectContaining({ name: "Front desk line" }));
    const banner = await screen.findByRole("status", { name: "Publish confirmation" });
    expect(banner).toHaveTextContent("Published v2.");
    await waitFor(() => expect(banner).toHaveTextContent("1 call is finishing on an earlier version"));
    expect(screen.getByRole("heading", { level: 1 })).toBeVisible();
    await user.click(screen.getByRole("button", { name: "Dismiss" }));
    expect(screen.queryByText(/Published v2/)).toBeNull();
  });

  it("creates a new flow on first publish and moves to its own address without losing the editor", async () => {
    const created = flow({ id: "n1", name: "After hours" });
    const createAgent = vi.fn().mockResolvedValue(created);
    // The refetch after publishing never answers: the page must already know the new flow.
    const getAgents = vi.fn().mockResolvedValueOnce([]).mockImplementation(() => new Promise(() => {}));
    const { user } = setup({ createAgent, getAgents }, "/flows/new?name=After%20hours");
    await user.click(await screen.findByRole("button", { name: "Add first step" }));
    await user.selectOptions(screen.getByLabelText("LLM provider"), "google");
    await user.selectOptions(screen.getByLabelText("TTS provider"), "google");
    await user.click(screen.getByRole("button", { name: "Flow settings" }));
    await user.selectOptions(screen.getByLabelText("STT provider"), "google");
    await user.click(screen.getByRole("button", { name: "Publish" }));
    await user.click(screen.getByRole("button", { name: "Publish v1" }));
    await waitFor(() => expect(createAgent).toHaveBeenCalledWith(expect.objectContaining({ name: "After hours" })));
    expect(await screen.findByRole("status", { name: "Publish confirmation" })).toHaveTextContent("Published v1.");
    expect(screen.queryByText("That flow was not found.")).toBeNull();
    expect(screen.getByRole("heading", { name: "After hours", level: 1 })).toBeVisible();
  });

  it("shows a failed publish with a way to dismiss it", async () => {
    const { user } = setup({ updateAgent: vi.fn().mockRejectedValue(new ApiError("version conflict", "version_conflict", 409, "r")) }, "/flows/a1");
    await screen.findByRole("heading", { name: "Front desk", level: 1 });
    await user.click(screen.getByRole("button", { name: "Flow settings" }));
    await user.type(screen.getByLabelText("Flow name"), "!");
    await user.click(screen.getByRole("button", { name: "Publish" }));
    await user.click(screen.getByRole("button", { name: "Publish v2" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("version conflict");
  });

  it("closing the editor goes back to the list", async () => {
    const { user } = setup({}, "/flows/a1");
    await user.click(await screen.findByRole("button", { name: "Close" }));
    expect(screen.getByText("the list")).toBeVisible();
  });
});
